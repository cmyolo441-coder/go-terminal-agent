// Package webtools provides the web_fetch and web_search tools for models
// whose provider has no server-side web tools. web_fetch reads one page and
// converts HTML to Markdown, refusing any destination that is not on the
// public internet; web_search queries a search API chosen by environment
// variables (Brave, Tavily or SearXNG).
package webtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

// Defaults for FetchOptions fields left zero.
const (
	DefaultFetchTimeout = 30 * time.Second
	DefaultMaxBytes     = 5 << 20
	DefaultMaxRedirects = 5
	DefaultMaxChars     = 40000
	// MaxCharsLimit is the most a single call may ask for.
	MaxCharsLimit   = 200000
	DefaultCacheTTL = 15 * time.Minute
	defaultCacheMax = 32 << 20
)

// FetchOptions configure a Fetcher.
type FetchOptions struct {
	// Timeout bounds a whole fetch including redirects.
	Timeout time.Duration
	// MaxBytes caps the decompressed body; longer bodies are cut off and
	// the result says so.
	MaxBytes int64
	// MaxRedirects is how many redirects are followed.
	MaxRedirects int
	// MaxChars is how much converted text one call returns by default;
	// the model pages through the rest with start_index.
	MaxChars int
	// UserAgent overrides the default "gta/<version>" agent string.
	UserAgent string
	// Proxy routes requests through an HTTP proxy, for example
	// http.ProxyFromEnvironment. Nil connects directly, which lets every
	// connection be checked at dial time. Through a proxy, each target
	// host (and each redirect) is resolved and checked before the request
	// is sent instead, and the proxy itself may be on a private network.
	Proxy func(*http.Request) (*url.URL, error)
	// CacheTTL keeps fetched pages for paging and repeat reads. Zero uses
	// DefaultCacheTTL; negative disables the cache.
	CacheTTL time.Duration
}

// Page is a fetched and converted document.
type Page struct {
	// URL is the final URL after redirects.
	URL         string
	Status      int
	ContentType string
	Title       string
	// Content is Markdown for HTML pages and the decoded text otherwise.
	Content string
	// Source is the decoded body as sent (HTML source for HTML pages).
	Source string
	// Truncated is true when the body exceeded MaxBytes.
	Truncated bool
	Fetched   time.Time
}

// Fetcher downloads web pages safely.
type Fetcher struct {
	opts   FetchOptions
	guard  *guard
	client *http.Client
	cache  *pageCache
	// trustedProxies are proxy addresses the dialler may reach even though
	// they are not public.
	trustedProxies sync.Map
}

// NewFetcher builds a fetcher.
func NewFetcher(opts FetchOptions) *Fetcher {
	return newFetcher(opts, &guard{lookup: systemLookup})
}

func newFetcher(opts FetchOptions, g *guard) *Fetcher {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultFetchTimeout
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxRedirects <= 0 {
		opts.MaxRedirects = DefaultMaxRedirects
	}
	if opts.MaxChars <= 0 {
		opts.MaxChars = DefaultMaxChars
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "gta/" + version.Version + " (web_fetch; +https://github.com/cmyolo441-coder/go-terminal-agent)"
	}
	if opts.CacheTTL == 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	f := &Fetcher{opts: opts, guard: g}
	if opts.CacheTTL > 0 {
		f.cache = newPageCache(opts.CacheTTL, defaultCacheMax)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	guarded := g.dialContext(dialer)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if _, ok := f.trustedProxies.Load(addr); ok {
				return dialer.DialContext(ctx, network, addr)
			}
			return guarded(ctx, network, addr)
		},
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           16,
		IdleConnTimeout:        60 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  opts.Timeout,
		MaxResponseHeaderBytes: 256 << 10,
	}
	if opts.Proxy != nil {
		tr.Proxy = func(req *http.Request) (*url.URL, error) {
			pu, err := opts.Proxy(req)
			if err != nil || pu == nil {
				return pu, err
			}
			// The proxy resolves the target, so check it here first.
			if _, err := g.resolve(req.Context(), req.URL.Hostname()); err != nil {
				return nil, err
			}
			f.trustedProxies.Store(proxyAddr(pu), true)
			return pu, nil
		}
	}
	f.client = &http.Client{
		Transport: tr,
		Timeout:   opts.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > opts.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", opts.MaxRedirects)
			}
			if err := checkURL(req.URL); err != nil {
				return fmt.Errorf("redirect to %s refused: %w", req.URL.Redacted(), err)
			}
			return nil
		},
	}
	return f
}

func proxyAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// checkURL accepts only plain http(s) URLs without embedded credentials.
func checkURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http and https URLs can be fetched, not %q", u.Scheme)
	}
	if u.User != nil {
		return errors.New("URLs with embedded credentials are not fetched")
	}
	if u.Hostname() == "" {
		return errors.New("the URL has no host")
	}
	return nil
}

// ParseURL validates a URL given to web_fetch. A missing scheme means https.
func ParseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if err := checkURL(u); err != nil {
		return nil, err
	}
	u.Fragment = ""
	return u, nil
}

// Fetch downloads and converts one page, serving repeat requests from the
// cache.
func (f *Fetcher) Fetch(ctx context.Context, raw string) (*Page, error) {
	u, err := ParseURL(raw)
	if err != nil {
		return nil, err
	}
	key := u.String()
	if f.cache != nil {
		if p, ok := f.cache.get(key); ok {
			return p, nil
		}
	}
	p, err := f.fetch(ctx, u)
	if err != nil {
		return nil, err
	}
	if f.cache != nil {
		f.cache.put(key, p)
	}
	return p, nil
}

// FetchError is an HTTP error status, with the start of the page body.
type FetchError struct {
	URL     string
	Status  string
	Excerpt string
}

func (e *FetchError) Error() string {
	msg := fmt.Sprintf("HTTP %s from %s", e.Status, e.URL)
	if e.Excerpt != "" {
		msg += "\n\n" + e.Excerpt
	}
	return msg
}

func (f *Fetcher) fetch(ctx context.Context, u *url.URL) (*Page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.opts.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/markdown;q=0.95,text/plain;q=0.9,application/json;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en;q=1.0, *;q=0.5")
	resp, err := f.client.Do(req)
	if err != nil {
		var be *BlockedAddressError
		if errors.As(err, &be) {
			return nil, be
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			if ue.Timeout() {
				return nil, fmt.Errorf("fetching %s timed out after %s", u.Redacted(), f.opts.Timeout)
			}
			return nil, fmt.Errorf("fetching %s: %w", u.Redacted(), ue.Err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, f.opts.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", resp.Request.URL.Redacted(), err)
	}
	truncated := int64(len(body)) > f.opts.MaxBytes
	if truncated {
		body = body[:f.opts.MaxBytes]
	}
	final := resp.Request.URL
	page := &Page{URL: final.String(), Status: resp.StatusCode, Truncated: truncated, Fetched: time.Now()}

	ctype := resp.Header.Get("Content-Type")
	mediaType, params, perr := mime.ParseMediaType(ctype)
	if perr != nil || mediaType == "" {
		sniff := http.DetectContentType(body)
		mediaType, params, _ = mime.ParseMediaType(sniff)
		ctype = sniff
	}
	page.ContentType = ctype
	kind := classify(mediaType, body)

	if resp.StatusCode >= 400 {
		excerpt := ""
		if kind != kindBinary {
			if text, err := decodeText(body, mediaType, params, kind == kindHTML); err == nil {
				if kind == kindHTML {
					if doc, err := HTMLToMarkdown(text, final); err == nil {
						text = doc.Markdown
					}
				}
				excerpt = clip(strings.TrimSpace(text), 800)
			}
		}
		return nil, &FetchError{URL: final.Redacted(), Status: resp.Status, Excerpt: excerpt}
	}
	if kind == kindBinary {
		return nil, fmt.Errorf("%s is %s, which web_fetch cannot read; it reads HTML, text, JSON and XML", final.Redacted(), describeType(mediaType))
	}
	text, err := decodeText(body, mediaType, params, kind == kindHTML)
	if err != nil {
		return nil, err
	}
	page.Source = text
	switch kind {
	case kindHTML:
		doc, err := HTMLToMarkdown(text, final)
		if err != nil {
			return nil, fmt.Errorf("parsing HTML from %s: %w", final.Redacted(), err)
		}
		page.Title, page.Content = doc.Title, doc.Markdown
	case kindJSON:
		var buf bytes.Buffer
		if json.Indent(&buf, []byte(text), "", "  ") == nil {
			page.Content = buf.String()
		} else {
			page.Content = text
		}
	default:
		page.Content = text
	}
	return page, nil
}

type contentKind int

const (
	kindText contentKind = iota
	kindHTML
	kindJSON
	kindBinary
)

func classify(mediaType string, body []byte) contentKind {
	mt := strings.ToLower(mediaType)
	switch {
	case mt == "text/html" || mt == "application/xhtml+xml":
		return kindHTML
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		return kindJSON
	case strings.HasPrefix(mt, "text/"),
		mt == "application/xml" || strings.HasSuffix(mt, "+xml"),
		mt == "application/javascript", mt == "application/ecmascript",
		mt == "application/x-yaml", mt == "application/yaml", mt == "application/toml",
		mt == "application/x-sh", mt == "application/x-httpd-php", mt == "application/sql",
		mt == "application/graphql", mt == "application/x-ndjson":
		return kindText
	case mt == "application/octet-stream" || mt == "binary/octet-stream":
		// Servers often mislabel text files; trust the bytes.
		if looksLikeText(body) {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(string(body[:min(len(body), 256)]))), "<!doctype html") {
				return kindHTML
			}
			return kindText
		}
	}
	return kindBinary
}

func looksLikeText(b []byte) bool {
	n := min(len(b), 8192)
	if bytes.IndexByte(b[:n], 0) >= 0 {
		return false
	}
	return utf8.Valid(b[:n]) || n == 0
}

func describeType(mt string) string {
	switch {
	case mt == "application/pdf":
		return "a PDF document"
	case strings.HasPrefix(mt, "image/"):
		return "an image (" + mt + ")"
	case strings.HasPrefix(mt, "video/"), strings.HasPrefix(mt, "audio/"):
		return "media (" + mt + ")"
	}
	return "binary content (" + mt + ")"
}

// decodeText converts the body to UTF-8 using, in order, a byte-order mark,
// the Content-Type charset, an HTML <meta> declaration, or UTF-8 detection.
func decodeText(body []byte, mediaType string, params map[string]string, isHTML bool) (string, error) {
	var enc encoding.Encoding
	name := ""
	if cs := strings.TrimSpace(params["charset"]); cs != "" {
		enc, name = charset.Lookup(cs)
	}
	if enc == nil {
		switch {
		case isHTML:
			enc, name, _ = charset.DetermineEncoding(body, mediaType)
		case utf8.Valid(body):
			name = "utf-8"
		default:
			enc, name, _ = charset.DetermineEncoding(body, "text/html")
		}
	}
	if enc == nil || name == "utf-8" {
		body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
		return strings.ToValidUTF8(string(body), "�"), nil
	}
	out, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return "", fmt.Errorf("decoding %s text: %w", name, err)
	}
	return strings.ToValidUTF8(string(out), "�"), nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// FetchTool is the web_fetch tool.
type FetchTool struct {
	f *Fetcher
}

var _ tools.Tool = (*FetchTool)(nil)

// NewFetchTool wraps a fetcher as a tool.
func NewFetchTool(f *Fetcher) *FetchTool { return &FetchTool{f: f} }

// Kind implements tools.Tool. Fetching only reads from the network.
func (*FetchTool) Kind() tools.Kind { return tools.KindRead }

// Spec implements tools.Tool.
func (t *FetchTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "web_fetch",
		Description: "Fetch a public web page or text document (HTML, Markdown, plain text, JSON, XML) and return it as Markdown. " +
			"Use it to read documentation, issues, changelogs or articles at a known URL. " +
			"Long pages are returned in parts: when the result says more is available, call again with start_index to continue. " +
			"Only public internet hosts can be reached; local and private network addresses are refused. Pages are cached for a few minutes.",
		Properties: map[string]any{
			"url":         map[string]any{"type": "string", "description": "The http(s) URL to fetch. A missing scheme means https."},
			"start_index": map[string]any{"type": "integer", "description": "Character offset to start from, for reading past the end of an earlier result. Default 0."},
			"max_length":  map[string]any{"type": "integer", "description": fmt.Sprintf("Most characters to return (default %d, max %d).", t.f.opts.MaxChars, MaxCharsLimit)},
			"raw":         map[string]any{"type": "boolean", "description": "Return the page source (HTML) instead of Markdown."},
		},
		Required: []string{"url"},
	}
}

type fetchInput struct {
	URL        string `json:"url"`
	StartIndex int    `json:"start_index"`
	MaxLength  int    `json:"max_length"`
	Raw        bool   `json:"raw"`
}

// Prepare implements tools.Tool.
func (t *FetchTool) Prepare(env *tools.Env, input json.RawMessage) (*tools.Call, error) {
	var in fetchInput
	if err := strictDecode(input, &in); err != nil {
		return nil, err
	}
	u, err := ParseURL(in.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid tool input: %w", err)
	}
	if in.StartIndex < 0 {
		return nil, errors.New("invalid tool input: start_index must not be negative")
	}
	if in.MaxLength < 0 {
		return nil, errors.New("invalid tool input: max_length must not be negative")
	}
	limit := in.MaxLength
	if limit == 0 {
		limit = t.f.opts.MaxChars
	}
	limit = min(limit, MaxCharsLimit)
	target := u.String()
	summary := "Fetch " + u.Redacted()
	run := func(ctx context.Context) (string, error) {
		p, err := t.f.Fetch(ctx, target)
		if err != nil {
			return "", err
		}
		return render(p, in.Raw, in.StartIndex, limit), nil
	}
	return tools.NewCall("web_fetch", tools.KindRead, summary, u.Redacted(), "", run), nil
}

// render formats a page slice for the model.
func render(p *Page, raw bool, start, limit int) string {
	content := p.Content
	if raw {
		content = p.Source
	}
	var b strings.Builder
	fmt.Fprintf(&b, "URL: %s\n", p.URL)
	if p.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", p.Title)
	}
	fmt.Fprintf(&b, "Content-Type: %s\n", p.ContentType)
	if p.Truncated {
		b.WriteString("Note: the document was larger than the download limit and was cut off.\n")
	}
	b.WriteString("\n")
	total := len(content)
	if start >= total {
		if total == 0 {
			b.WriteString("(the page has no readable content)")
		} else {
			fmt.Fprintf(&b, "(start_index %d is past the end of the content, which is %d characters long)", start, total)
		}
		return b.String()
	}
	for start > 0 && !utf8.RuneStart(content[start]) {
		start--
	}
	end := min(start+limit, total)
	for end < total && !utf8.RuneStart(content[end]) {
		end--
	}
	b.WriteString(content[start:end])
	if end < total {
		fmt.Fprintf(&b, "\n\n[Showing characters %d to %d of %d. Call web_fetch again with start_index=%d to read more.]", start, end, total, end)
	}
	return b.String()
}

// strictDecode rejects unknown fields and trailing data.
func strictDecode(input json.RawMessage, v any) error {
	if len(bytes.TrimSpace(input)) == 0 {
		input = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid tool input: %w", err)
	}
	if dec.More() {
		return errors.New("invalid tool input: trailing data")
	}
	return nil
}
