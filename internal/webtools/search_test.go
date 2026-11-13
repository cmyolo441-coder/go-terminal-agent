package webtools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func init() { retryBase = 5 * time.Millisecond }

func TestBackendFromEnv(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if _, err := BackendFromEnv(getenv, nil); !errors.Is(err, ErrNoSearchBackend) || !strings.Contains(err.Error(), "BRAVE_API_KEY") {
		t.Fatalf("none: %v", err)
	}
	env[EnvSearXNG] = "http://localhost:8888/"
	b, err := BackendFromEnv(getenv, nil)
	if err != nil || b.Name() != "SearXNG" || b.(*SearXNG).endpoint != "http://localhost:8888/search" {
		t.Fatalf("searxng: %v %v", b, err)
	}
	env[EnvTavilyKey] = "tv"
	if b, _ := BackendFromEnv(getenv, nil); b.Name() != "Tavily" {
		t.Fatalf("tavily should win over searxng: %s", b.Name())
	}
	env[EnvBraveKey] = "br"
	if b, _ := BackendFromEnv(getenv, nil); b.Name() != "Brave" {
		t.Fatalf("brave should win: %s", b.Name())
	}
	env[EnvBackend] = "SearXNG"
	if b, _ := BackendFromEnv(getenv, nil); b.Name() != "SearXNG" {
		t.Fatalf("explicit choice: %s", b.Name())
	}
	env[EnvBackend] = "bing"
	if _, err := BackendFromEnv(getenv, nil); err == nil || !strings.Contains(err.Error(), "bing") {
		t.Fatalf("unknown backend: %v", err)
	}
	env = map[string]string{EnvBackend: "brave"}
	if _, err := BackendFromEnv(getenv, nil); err == nil || !strings.Contains(err.Error(), EnvBraveKey) {
		t.Fatalf("forced without key: %v", err)
	}
	env = map[string]string{EnvSearXNG: "localhost:8888"}
	if _, err := BackendFromEnv(getenv, nil); err == nil {
		t.Fatal("searxng URL without scheme must be rejected")
	}
}

func runSearch(t *testing.T, b Backend, input string) (string, error) {
	t.Helper()
	tool := NewSearchTool(b)
	call, err := tool.Prepare(nil, json.RawMessage(input))
	if err != nil {
		return "", err
	}
	if call.Kind != tools.KindRead {
		t.Fatal("web_search must be a read")
	}
	return call.Run(context.Background())
}

func TestBraveSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if q.Get("q") != "go generics (site:go.dev OR site:github.com) -site:spam.com" || q.Get("count") != "4" {
			t.Errorf("query: %v", q)
		}
		io.WriteString(w, `{"web":{"results":[
			{"title":"Tutorial: <strong>Generics</strong>","url":"https://go.dev/doc/tutorial/generics","description":"Learn &amp; use <strong>generics</strong>.","age":"2 days ago"},
			{"title":"dup","url":"https://go.dev/doc/tutorial/generics","description":"duplicate"},
			{"title":"Elsewhere","url":"https://example.org/x","description":"off-domain"},
			{"title":"Spam","url":"https://www.spam.com/a","description":"blocked"},
			{"title":"","url":"https://pkg.github.com/r","description":"subdomain"},
			{"title":"bad","url":"javascript:alert(1)","description":"bad scheme"}
		]}}`)
	}))
	defer srv.Close()
	b := NewBrave("secret", BackendOptions{BaseURL: srv.URL})
	out, err := runSearch(t, b, `{"query":"go generics","max_results":2,"allowed_domains":["https://go.dev/","GitHub.com"],"blocked_domains":["spam.com"]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := "Search results for \"go generics\" (via Brave):\n\n" +
		"1. Tutorial: Generics\n   https://go.dev/doc/tutorial/generics\n   Learn & use generics.\n   Published: 2 days ago\n\n" +
		"2. pkg.github.com\n   https://pkg.github.com/r\n   subdomain"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	_, err = runSearch(t, NewBrave("wrong", BackendOptions{BaseURL: srv.URL}), `{"query":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "check BRAVE_API_KEY") {
		t.Fatalf("auth error: %v", err)
	}
}

func TestTavilySearchRetriesAndFilters(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer tv-key" {
			t.Errorf("request: %s %s", r.Method, r.Header.Get("Authorization"))
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["query"] != "rust async" || body["max_results"] != float64(6) || body["exclude_domains"] == nil {
			t.Errorf("body: %v", body)
		}
		io.WriteString(w, `{"results":[{"title":"Async Book","url":"https://rust-lang.github.io/async-book/","content":"Async programming","published_date":"2025-01-02"},{"title":"Bad","url":"https://medium.com/x","content":"blocked"}]}`)
	}))
	defer srv.Close()
	out, err := runSearch(t, NewTavily("tv-key", BackendOptions{BaseURL: srv.URL}), `{"query":"rust async","max_results":3,"blocked_domains":["medium.com"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !strings.Contains(out, "1. Async Book") || strings.Contains(out, "medium") || !strings.Contains(out, "Published: 2025-01-02") {
		t.Fatalf("calls=%d out=%s", calls.Load(), out)
	}
}

func TestSearXNGSearch(t *testing.T) {
	var mode atomic.Value
	mode.Store("ok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/searx/search" || r.URL.Query().Get("format") != "json" {
			t.Errorf("path %s query %s", r.URL.Path, r.URL.RawQuery)
		}
		switch mode.Load() {
		case "forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "empty":
			io.WriteString(w, `{"results":[]}`)
		case "garbage":
			io.WriteString(w, `<html>not json</html>`)
		case "down":
			w.WriteHeader(http.StatusBadGateway)
		default:
			io.WriteString(w, `{"results":[{"title":"Zig","url":"https://ziglang.org/","content":"A language","publishedDate":null}]}`)
		}
	}))
	defer srv.Close()
	b, err := NewSearXNG(srv.URL+"/searx", BackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runSearch(t, b, `{"query":"zig"}`); err != nil || !strings.Contains(out, "1. Zig\n   https://ziglang.org/") {
		t.Fatalf("ok: %s %v", out, err)
	}
	mode.Store("empty")
	if out, err := runSearch(t, b, `{"query":"zig","allowed_domains":["x.org"]}`); err != nil || !strings.Contains(out, "No results") || !strings.Contains(out, "relaxing") {
		t.Fatalf("empty: %s %v", out, err)
	}
	for m, want := range map[string]string{"forbidden": "enable it", "garbage": "unexpected response", "down": "502"} {
		mode.Store(m)
		if _, err := runSearch(t, b, `{"query":"zig"}`); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", m, want, err)
		}
	}
}

func TestSearchInputValidation(t *testing.T) {
	if _, err := NewSearchTool(nil).Prepare(nil, json.RawMessage(`{"query":"x"}`)); !errors.Is(err, ErrNoSearchBackend) {
		t.Fatalf("nil backend: %v", err)
	}
	b := NewBrave("k", BackendOptions{BaseURL: "http://127.0.0.1:1"})
	tool := NewSearchTool(b)
	for _, in := range []string{`{}`, `{"query":"  "}`, `{"query":"x","max_results":21}`, `{"query":"x","max_results":-1}`,
		`{"query":"` + strings.Repeat("q", 401) + `"}`, `{"query":"x","allowed_domains":["a b"]}`, `{"query":"x","extra":true}`} {
		if _, err := tool.Prepare(nil, json.RawMessage(in)); err == nil {
			t.Errorf("%s should be rejected", in)
		}
	}
	if tool.Backend() != b || tool.Spec().Name != "web_search" || tool.Kind() != tools.KindRead {
		t.Fatal("accessors")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("3"); d != 3*time.Second {
		t.Fatal(d)
	}
	if d := parseRetryAfter("120"); d != 10*time.Second {
		t.Fatal("must cap", d)
	}
	if d := parseRetryAfter(time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)); d <= 0 || d > 2*time.Second {
		t.Fatal("date", d)
	}
	if parseRetryAfter("") != 0 || parseRetryAfter("soon") != 0 {
		t.Fatal("invalid values")
	}
}
