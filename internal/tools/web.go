package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const maxFetchBytes = 5 << 20
const maxFetchChars = 60000

var (
	reDrop    = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head|iframe)\b.*?</(script|style|noscript|svg|head|iframe)>`)
	reComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	reHead    = regexp.MustCompile(`(?is)<h([1-6])[^>]*>(.*?)</h[1-6]>`)
	reAnchor  = regexp.MustCompile(`(?is)<a\s[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reLi      = regexp.MustCompile(`(?i)<li[^>]*>`)
	reBlock   = regexp.MustCompile(`(?i)</?(p|div|br|tr|ul|ol|table|section|article|header|footer|pre|blockquote|h[1-6])\b[^>]*>`)
	reTag     = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpaces  = regexp.MustCompile(`[ \t]+`)
	reBlank   = regexp.MustCompile(`\n\s*\n\s*\n+`)
)

// HTMLToText converts an HTML page into readable markdown-ish text.
func HTMLToText(s string) string {
	s = reDrop.ReplaceAllString(s, "")
	s = reComment.ReplaceAllString(s, "")
	s = reHead.ReplaceAllStringFunc(s, func(m string) string {
		g := reHead.FindStringSubmatch(m)
		return "\n\n" + strings.Repeat("#", int(g[1][0]-'0')) + " " + g[2] + "\n\n"
	})
	s = reAnchor.ReplaceAllString(s, "[$2]($1)")
	s = reLi.ReplaceAllString(s, "\n- ")
	s = reBlock.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reSpaces.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(reBlank.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

func webFetchTool() *Tool {
	client := &http.Client{Timeout: 30 * time.Second}
	return &Tool{
		Name:        "WebFetch",
		Description: `Fetches a URL over HTTP(S) and returns the page as text (HTML is converted to readable text; JSON and plain text are returned as-is). Use it to read documentation or a page the user linked. Long pages are truncated.`,
		Properties: map[string]any{
			"url": prop("string", "The http(s) URL to fetch"),
		},
		Required: []string{"url"},
		Key:      func(in json.RawMessage, _ *Env) string { return str(in, "url") },
		Summary:  func(in json.RawMessage, _ *Env) string { return str(in, "url") },
		Run: func(ctx context.Context, raw json.RawMessage, _ *Env) Result {
			var in struct {
				URL string `json:"url"`
			}
			if err := decode(raw, []string{"url"}, &in); err != nil {
				return Errorf("%v", err)
			}
			if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
				return Errorf("only http and https URLs are supported")
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
			if err != nil {
				return Errorf("bad URL: %v", err)
			}
			req.Header.Set("User-Agent", "gocode-webfetch/1.0")
			req.Header.Set("Accept", "text/html,text/plain,application/json;q=0.9,*/*;q=0.5")
			resp, err := client.Do(req)
			if err != nil {
				return Errorf("fetch failed: %v", err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
			if err != nil {
				return Errorf("read failed: %v", err)
			}
			if resp.StatusCode >= 400 {
				return Errorf("HTTP %s\n%s", resp.Status, truncateMiddle(string(body), 2000))
			}
			text := string(body)
			ct := resp.Header.Get("Content-Type")
			if strings.Contains(ct, "html") || strings.Contains(strings.ToLower(text[:min(len(text), 500)]), "<html") {
				text = HTMLToText(text)
			}
			note := ""
			if len(text) > maxFetchChars {
				text = text[:maxFetchChars]
				note = "\n\n[content truncated]"
			}
			return Result{Text: fmt.Sprintf("URL: %s\nStatus: %s\n\n%s%s", resp.Request.URL, resp.Status, text, note),
				Display: fmt.Sprintf("Received %d bytes (%s)", len(body), resp.Status)}
		},
	}
}
