package webtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

// Environment variables that configure web_search. Keys are only ever read
// from the environment, never from config files.
const (
	EnvBackend   = "GTA_WEB_SEARCH_BACKEND" // optional: brave, tavily or searxng
	EnvBraveKey  = "BRAVE_API_KEY"
	EnvTavilyKey = "TAVILY_API_KEY"
	EnvSearXNG   = "SEARXNG_URL"
)

// Search limits.
const (
	DefaultResults = 8
	MaxResults     = 20
	maxQueryLen    = 400
	maxSearchBody  = 2 << 20
)

// ErrNoSearchBackend is returned when no search API is configured.
var ErrNoSearchBackend = errors.New("web_search is not configured: set BRAVE_API_KEY (Brave Search API), TAVILY_API_KEY (Tavily) or SEARXNG_URL (a SearXNG instance with the JSON format enabled)")

// Result is one search hit.
type Result struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet"`
	Published string `json:"published,omitempty"`
}

// Query is a search request.
type Query struct {
	Text           string
	Count          int
	AllowedDomains []string
	BlockedDomains []string
}

// Backend is a search API.
type Backend interface {
	Name() string
	Search(ctx context.Context, q Query) ([]Result, error)
}

// BackendOptions are shared by the backends.
type BackendOptions struct {
	// Client defaults to a client with a 20 second timeout.
	Client *http.Client
	// BaseURL overrides the API endpoint (for gateways and tests).
	BaseURL string
}

func (o BackendOptions) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// BackendFromEnv picks a backend from the environment: the one named by
// GTA_WEB_SEARCH_BACKEND, or else the first configured of Brave, Tavily and
// SearXNG. getenv is usually os.Getenv.
func BackendFromEnv(getenv func(string) string, client *http.Client) (Backend, error) {
	o := BackendOptions{Client: client}
	brave := strings.TrimSpace(getenv(EnvBraveKey))
	tavily := strings.TrimSpace(getenv(EnvTavilyKey))
	searx := strings.TrimSpace(getenv(EnvSearXNG))
	switch choice := strings.ToLower(strings.TrimSpace(getenv(EnvBackend))); choice {
	case "":
	case "brave":
		if brave == "" {
			return nil, fmt.Errorf("%s=brave but %s is not set", EnvBackend, EnvBraveKey)
		}
		return NewBrave(brave, o), nil
	case "tavily":
		if tavily == "" {
			return nil, fmt.Errorf("%s=tavily but %s is not set", EnvBackend, EnvTavilyKey)
		}
		return NewTavily(tavily, o), nil
	case "searxng":
		if searx == "" {
			return nil, fmt.Errorf("%s=searxng but %s is not set", EnvBackend, EnvSearXNG)
		}
		return NewSearXNG(searx, o)
	default:
		return nil, fmt.Errorf("%s=%q: want brave, tavily or searxng", EnvBackend, choice)
	}
	switch {
	case brave != "":
		return NewBrave(brave, o), nil
	case tavily != "":
		return NewTavily(tavily, o), nil
	case searx != "":
		return NewSearXNG(searx, o)
	}
	return nil, ErrNoSearchBackend
}

// Brave queries the Brave Search API.
type Brave struct {
	key  string
	base string
	hc   *http.Client
}

// NewBrave builds a Brave backend.
func NewBrave(key string, o BackendOptions) *Brave {
	base := o.BaseURL
	if base == "" {
		base = "https://api.search.brave.com/res/v1/web/search"
	}
	return &Brave{key: key, base: base, hc: o.client()}
}

// Name implements Backend.
func (*Brave) Name() string { return "Brave" }

// Search implements Backend.
func (b *Brave) Search(ctx context.Context, q Query) ([]Result, error) {
	u, err := url.Parse(b.base)
	if err != nil {
		return nil, fmt.Errorf("brave: bad endpoint: %w", err)
	}
	v := u.Query()
	v.Set("q", siteQuery(q))
	v.Set("count", strconv.Itoa(min(q.Count, MaxResults)))
	v.Set("text_decorations", "false")
	u.RawQuery = v.Encode()
	var resp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				Age         string `json:"age"`
				PageAge     string `json:"page_age"`
			} `json:"results"`
		} `json:"web"`
	}
	err = doJSON(ctx, b.hc, "Brave Search", EnvBraveKey, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Subscription-Token", b.key)
		return req, nil
	}, &resp)
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(resp.Web.Results))
	for _, r := range resp.Web.Results {
		pub := r.Age
		if pub == "" {
			pub = r.PageAge
		}
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Description, Published: pub})
	}
	return out, nil
}

// Tavily queries the Tavily search API.
type Tavily struct {
	key  string
	base string
	hc   *http.Client
}

// NewTavily builds a Tavily backend.
func NewTavily(key string, o BackendOptions) *Tavily {
	base := o.BaseURL
	if base == "" {
		base = "https://api.tavily.com/search"
	}
	return &Tavily{key: key, base: base, hc: o.client()}
}

// Name implements Backend.
func (*Tavily) Name() string { return "Tavily" }

// Search implements Backend.
func (t *Tavily) Search(ctx context.Context, q Query) ([]Result, error) {
	payload := map[string]any{
		"query":        q.Text,
		"max_results":  min(q.Count, MaxResults),
		"search_depth": "basic",
	}
	if len(q.AllowedDomains) > 0 {
		payload["include_domains"] = q.AllowedDomains
	}
	if len(q.BlockedDomains) > 0 {
		payload["exclude_domains"] = q.BlockedDomains
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			PublishedDate string `json:"published_date"`
		} `json:"results"`
	}
	err = doJSON(ctx, t.hc, "Tavily", EnvTavilyKey, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+t.key)
		return req, nil
	}, &resp)
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content, Published: r.PublishedDate})
	}
	return out, nil
}

// SearXNG queries a SearXNG instance's JSON API.
type SearXNG struct {
	endpoint string
	hc       *http.Client
}

// NewSearXNG builds a SearXNG backend from the instance URL, for example
// http://localhost:8888. The instance must allow the json format.
func NewSearXNG(instance string, o BackendOptions) (*SearXNG, error) {
	base := instance
	if o.BaseURL != "" {
		base = o.BaseURL
	}
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s must be an http(s) URL such as http://localhost:8888, got %q", EnvSearXNG, base)
	}
	if !strings.HasSuffix(u.Path, "/search") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/search"
	}
	u.RawQuery = ""
	return &SearXNG{endpoint: u.String(), hc: o.client()}, nil
}

// Name implements Backend.
func (*SearXNG) Name() string { return "SearXNG" }

// Search implements Backend.
func (s *SearXNG) Search(ctx context.Context, q Query) ([]Result, error) {
	u, _ := url.Parse(s.endpoint)
	v := url.Values{}
	v.Set("q", siteQuery(q))
	v.Set("format", "json")
	v.Set("safesearch", "1")
	u.RawQuery = v.Encode()
	var resp struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
	}
	err := doJSON(ctx, s.hc, "SearXNG", EnvSearXNG, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		return req, nil
	}, &resp)
	if err != nil {
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusForbidden {
			return nil, fmt.Errorf("SearXNG refused the JSON API (HTTP 403): enable it with `search: formats: [html, json]` in the instance's settings.yml")
		}
		return nil, err
	}
	out := make([]Result, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content, Published: r.PublishedDate})
	}
	return out, nil
}

// siteQuery adds site: operators for backends without domain parameters.
// Results are filtered again afterwards, so this only improves recall.
func siteQuery(q Query) string {
	text := q.Text
	if len(q.AllowedDomains) > 0 {
		var sites []string
		for _, d := range q.AllowedDomains {
			sites = append(sites, "site:"+d)
		}
		text += " (" + strings.Join(sites, " OR ") + ")"
	}
	for _, d := range q.BlockedDomains {
		text += " -site:" + d
	}
	return text
}

type statusError struct {
	service, keyEnv string
	code            int
	status          string
	body            string
	retryAfter      time.Duration
}

func (e *statusError) Error() string {
	switch e.code {
	case http.StatusUnauthorized, http.StatusForbidden:
		if e.keyEnv != EnvSearXNG {
			return fmt.Sprintf("%s authentication failed (HTTP %d): check %s", e.service, e.code, e.keyEnv)
		}
	case http.StatusTooManyRequests:
		return fmt.Sprintf("%s rate limit reached (HTTP 429); try again shortly", e.service)
	}
	msg := fmt.Sprintf("%s returned HTTP %s", e.service, e.status)
	if e.body != "" {
		msg += ": " + e.body
	}
	return msg
}

// searchAttempts is how many times a request is tried on rate limits,
// server errors and network failures.
const searchAttempts = 3

// retryBase is the first backoff delay; tests shorten it.
var retryBase = 400 * time.Millisecond

// doJSON sends a request built by mk, retrying transient failures with
// exponential backoff (honouring Retry-After), and decodes a JSON reply.
func doJSON(ctx context.Context, hc *http.Client, service, keyEnv string, mk func() (*http.Request, error), out any) error {
	var lastErr error
	for attempt := 0; attempt < searchAttempts; attempt++ {
		if attempt > 0 {
			delay := retryBase << (attempt - 1)
			var se *statusError
			if errors.As(lastErr, &se) && se.code == http.StatusTooManyRequests {
				if se.retryAfter > 0 {
					delay = se.retryAfter
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		req, err := mk()
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "gta/"+version.Version)
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("%s request failed: %w", service, err)
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxSearchBody+1))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			se := &statusError{
				service: service, keyEnv: keyEnv, code: resp.StatusCode, status: resp.Status,
				body:       clip(collapseSpace(string(body)), 300),
				retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			}
			lastErr = se
			if retryable(resp.StatusCode) {
				continue
			}
			return se
		}
		if rerr != nil {
			lastErr = fmt.Errorf("%s: reading response: %w", service, rerr)
			continue
		}
		if len(body) > maxSearchBody {
			return fmt.Errorf("%s returned more than %d bytes", service, maxSearchBody)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("%s returned an unexpected response: %w", service, err)
		}
		return nil
	}
	return lastErr
}

func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout || code == http.StatusInternalServerError
}

// parseRetryAfter reads a Retry-After value in seconds or as an HTTP date,
// capped at ten seconds so a tool call never stalls for long.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if s, err := strconv.Atoi(v); err == nil {
		d = time.Duration(s) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	if d <= 0 {
		return 0
	}
	return min(d, 10*time.Second)
}

// SearchTool is the web_search tool.
type SearchTool struct {
	backend Backend
	err     error
}

var _ tools.Tool = (*SearchTool)(nil)

// NewSearchTool wraps a backend. A nil backend makes every call fail with
// ErrNoSearchBackend.
func NewSearchTool(b Backend) *SearchTool {
	t := &SearchTool{backend: b}
	if b == nil {
		t.err = ErrNoSearchBackend
	}
	return t
}

// Backend returns the search backend, or nil.
func (t *SearchTool) Backend() Backend { return t.backend }

// Kind implements tools.Tool.
func (*SearchTool) Kind() tools.Kind { return tools.KindRead }

// Spec implements tools.Tool.
func (t *SearchTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "web_search",
		Description: "Search the web and return titles, URLs and snippets. Use it for current information, documentation you do not know the URL of, " +
			"error messages and release notes; then read promising results with web_fetch. Cite the URLs you rely on.",
		Properties: map[string]any{
			"query":           map[string]any{"type": "string", "description": "The search query."},
			"max_results":     map[string]any{"type": "integer", "description": fmt.Sprintf("Number of results, 1 to %d (default %d).", MaxResults, DefaultResults)},
			"allowed_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only return results from these domains (and their subdomains)."},
			"blocked_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Never return results from these domains."},
		},
		Required: []string{"query"},
	}
}

type searchInput struct {
	Query          string   `json:"query"`
	MaxResults     int      `json:"max_results"`
	AllowedDomains []string `json:"allowed_domains"`
	BlockedDomains []string `json:"blocked_domains"`
}

// Prepare implements tools.Tool.
func (t *SearchTool) Prepare(env *tools.Env, input json.RawMessage) (*tools.Call, error) {
	if t.err != nil {
		return nil, t.err
	}
	var in searchInput
	if err := strictDecode(input, &in); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(in.Query)
	if q == "" {
		return nil, errors.New(`invalid tool input: "query" is required`)
	}
	if len(q) > maxQueryLen {
		return nil, fmt.Errorf("invalid tool input: query is longer than %d characters", maxQueryLen)
	}
	n := in.MaxResults
	if n == 0 {
		n = DefaultResults
	}
	if n < 1 || n > MaxResults {
		return nil, fmt.Errorf("invalid tool input: max_results must be between 1 and %d", MaxResults)
	}
	allowed, err := normalizeDomains(in.AllowedDomains)
	if err != nil {
		return nil, err
	}
	blocked, err := normalizeDomains(in.BlockedDomains)
	if err != nil {
		return nil, err
	}
	query := Query{Text: q, Count: n, AllowedDomains: allowed, BlockedDomains: blocked}
	backend := t.backend
	run := func(ctx context.Context) (string, error) {
		fetchQ := query
		if len(allowed)+len(blocked) > 0 {
			// Ask for more so filtering still leaves enough results.
			fetchQ.Count = min(MaxResults, n*2)
		}
		results, err := backend.Search(ctx, fetchQ)
		if err != nil {
			return "", err
		}
		return formatResults(backend.Name(), query, filterResults(results, allowed, blocked, n)), nil
	}
	summary := fmt.Sprintf("Search the web for %q", q)
	return tools.NewCall("web_search", tools.KindRead, summary, q, "", run), nil
}

// normalizeDomains turns entries such as "https://www.Go.dev/doc" into
// "www.go.dev".
func normalizeDomains(in []string) ([]string, error) {
	var out []string
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if strings.Contains(d, "://") {
			u, err := url.Parse(d)
			if err != nil || u.Hostname() == "" {
				return nil, fmt.Errorf("invalid tool input: bad domain %q", d)
			}
			d = u.Hostname()
		}
		d = strings.TrimPrefix(d, "*.")
		d = strings.TrimPrefix(d, ".")
		if i := strings.IndexByte(d, '/'); i >= 0 {
			d = d[:i]
		}
		if d == "" || strings.ContainsAny(d, " \t?#@") {
			return nil, fmt.Errorf("invalid tool input: bad domain %q", d)
		}
		out = append(out, d)
	}
	return out, nil
}

func domainMatch(host, domain string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// filterResults applies domain filters, drops duplicates and non-http
// links, cleans snippets and keeps at most n results.
func filterResults(in []Result, allowed, blocked []string, n int) []Result {
	seen := map[string]bool{}
	var out []Result
	for _, r := range in {
		u, err := url.Parse(strings.TrimSpace(r.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		host := u.Hostname()
		ok := len(allowed) == 0
		for _, d := range allowed {
			if domainMatch(host, d) {
				ok = true
				break
			}
		}
		for _, d := range blocked {
			if domainMatch(host, d) {
				ok = false
				break
			}
		}
		key := u.Host + u.EscapedPath() + "?" + u.RawQuery
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		r.URL = u.String()
		r.Title = collapseSpace(stripTags(r.Title))
		r.Snippet = clip(collapseSpace(stripTags(r.Snippet)), 600)
		if r.Title == "" {
			r.Title = host
		}
		out = append(out, r)
		if len(out) == n {
			break
		}
	}
	return out
}

// stripTags removes HTML markup (search APIs highlight matches with
// <strong>) and decodes entities.
func stripTags(s string) string {
	if !strings.ContainsAny(s, "<&") {
		return s
	}
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			return b.String()
		case html.TextToken:
			b.Write(z.Text())
		}
	}
}

func formatResults(backend string, q Query, rs []Result) string {
	var b strings.Builder
	if len(rs) == 0 {
		fmt.Fprintf(&b, "No results for %q (via %s).", q.Text, backend)
		if len(q.AllowedDomains)+len(q.BlockedDomains) > 0 {
			b.WriteString(" Try relaxing the domain filters.")
		}
		return b.String()
	}
	fmt.Fprintf(&b, "Search results for %q (via %s):\n", q.Text, backend)
	for i, r := range rs {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
		if r.Published != "" {
			fmt.Fprintf(&b, "   Published: %s\n", r.Published)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

var osGetenv = os.Getenv

// Options configure the tool set.
type Options struct {
	Fetch FetchOptions
	// Getenv reads the search configuration; nil means os.Getenv.
	Getenv func(string) string
	// SearchClient overrides the HTTP client used for search APIs.
	SearchClient *http.Client
}

// Tools returns web_fetch, plus web_search when a backend is configured.
// The error explains why web_search is missing (ErrNoSearchBackend or a
// misconfiguration) and is informational: web_fetch is always returned.
func Tools(o Options) ([]tools.Tool, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = osGetenv
	}
	out := []tools.Tool{NewFetchTool(NewFetcher(o.Fetch))}
	b, err := BackendFromEnv(getenv, o.SearchClient)
	if err != nil {
		return out, err
	}
	return append(out, NewSearchTool(b)), nil
}
