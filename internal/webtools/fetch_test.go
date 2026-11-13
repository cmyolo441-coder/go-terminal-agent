package webtools

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// testGuard allows loopback (for httptest) and resolves *.test names from a
// fixed table.
func testGuard(hosts map[string][]string) *guard {
	return &guard{allowLoopback: true, lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
		addrs, ok := hosts[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}}
}

func TestBlockReason(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.8.9.1", "10.0.0.5", "172.16.3.4", "172.31.255.255", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "255.255.255.255", "198.18.0.1",
		"::1", "::", "fe80::1", "fd00:ec2::254", "fc00::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::a9fe:a9fe", "2002:0a00:0001::1", "ff02::1",
	}
	for _, s := range blocked {
		if blockReason(netip.MustParseAddr(s)) == "" {
			t.Errorf("%s should be blocked", s)
		}
	}
	public := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "172.32.0.1", "2606:4700:4700::1111", "64:ff9b::808:808", "2002:0808:0808::1"}
	for _, s := range public {
		if why := blockReason(netip.MustParseAddr(s)); why != "" {
			t.Errorf("%s should be allowed, got %s", s, why)
		}
	}
}

func TestGuardResolve(t *testing.T) {
	g := testGuard(map[string][]string{
		"mixed.test":  {"8.8.8.8", "10.0.0.1"},
		"public.test": {"93.184.216.34"},
	})
	g.allowLoopback = false
	if _, err := g.resolve(context.Background(), "mixed.test"); err == nil || !strings.Contains(err.Error(), "10.0.0.1") {
		t.Fatalf("mixed records must be refused: %v", err)
	}
	if ips, err := g.resolve(context.Background(), "public.test."); err != nil || len(ips) != 1 {
		t.Fatalf("public: %v %v", ips, err)
	}
	for _, h := range []string{"localhost", "db.internal", "printer.local", "[::1]", "127.0.0.1"} {
		if _, err := g.resolve(context.Background(), h); err == nil {
			t.Errorf("%s should be refused", h)
		}
	}
	if _, err := g.resolve(context.Background(), "nope.test"); err == nil || !strings.Contains(err.Error(), "could not resolve") {
		t.Fatalf("unknown host: %v", err)
	}
}

func TestParseURL(t *testing.T) {
	u, err := ParseURL(" go.dev/doc#frag ")
	if err != nil || u.String() != "https://go.dev/doc" {
		t.Fatalf("%v %v", u, err)
	}
	for _, bad := range []string{"", "ftp://x.org/f", "file:///etc/passwd", "https://user:pw@x.org/", "http://"} {
		if _, err := ParseURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestFetchHTMLWithRedirectAndCache(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/page", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "gta/") {
			t.Errorf("user agent %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Hello</title></head><body><h1>Title</h1><p>`+strings.Repeat("word ", 50)+`</p><a href="/x">rel</a></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := newFetcher(FetchOptions{}, testGuard(nil))
	tool := NewFetchTool(f)
	if tool.Kind() != tools.KindRead || tool.Spec().Name != "web_fetch" {
		t.Fatal("spec")
	}
	call, err := tool.Prepare(nil, json.RawMessage(`{"url":"`+srv.URL+`/old","max_length":120}`))
	if err != nil {
		t.Fatal(err)
	}
	if call.Kind != tools.KindRead || !strings.Contains(call.Summary, "/old") {
		t.Fatalf("call: %+v", call)
	}
	out, err := call.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"URL: " + srv.URL + "/page", "Title: Hello", "Content-Type: text/html", "# Title", "start_index="} {
		if !strings.Contains(out, w) {
			t.Fatalf("missing %q in %s", w, out)
		}
	}
	// The next page comes from the cache.
	call, _ = tool.Prepare(nil, json.RawMessage(`{"url":"`+srv.URL+`/old","start_index":120,"max_length":100000}`))
	out, err = call.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "start_index=") || !strings.Contains(out, "[rel]("+srv.URL+"/x)") {
		t.Fatalf("second page: %s", out)
	}
	if hits.Load() != 1 {
		t.Fatalf("page fetched %d times, want 1 (cached)", hits.Load())
	}
	call, _ = tool.Prepare(nil, json.RawMessage(`{"url":"`+srv.URL+`/page","raw":true}`))
	if out, _ := call.Run(context.Background()); !strings.Contains(out, "<h1>Title</h1>") {
		t.Fatalf("raw: %s", out)
	}
	call, _ = tool.Prepare(nil, json.RawMessage(`{"url":"`+srv.URL+`/page","start_index":999999}`))
	if out, _ := call.Run(context.Background()); !strings.Contains(out, "past the end") {
		t.Fatalf("past end: %s", out)
	}
}

func TestFetchInputValidation(t *testing.T) {
	tool := NewFetchTool(newFetcher(FetchOptions{}, testGuard(nil)))
	for _, in := range []string{`{}`, `{"url":"ftp://x"}`, `{"url":"https://x.org","start_index":-1}`, `{"url":"https://x.org","max_length":-5}`, `{"url":"https://x.org","bogus":1}`, `{"url":"https://x.org"} {}`} {
		if _, err := tool.Prepare(nil, json.RawMessage(in)); err == nil {
			t.Errorf("%s should be rejected", in)
		}
	}
}

func TestFetchSSRF(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/to-private", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.1.2.3/admin", http.StatusFound)
	})
	mux.HandleFunc("/to-metadata", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://metadata.test/latest/meta-data/", http.StatusFound)
	})
	mux.HandleFunc("/to-file", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Without the test-only loopback allowance, the server itself is refused.
	strict := newFetcher(FetchOptions{}, &guard{lookup: systemLookup})
	_, err := strict.Fetch(context.Background(), srv.URL+"/to-private")
	var be *BlockedAddressError
	if !errors.As(err, &be) || be.Why != "loopback" {
		t.Fatalf("loopback must be blocked: %v", err)
	}

	f := newFetcher(FetchOptions{}, testGuard(map[string][]string{"metadata.test": {"169.254.169.254"}}))
	for path, want := range map[string]string{
		"/to-private":  "10.1.2.3 is a private address",
		"/to-metadata": "metadata.test resolves to 169.254.169.254",
		"/to-file":     "only http and https",
	} {
		_, err := f.Fetch(context.Background(), srv.URL+path)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", path, want, err)
		}
	}
}

func TestFetchThroughProxyChecksTargets(t *testing.T) {
	var seen atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.URL.String())
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "via proxy")
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	g := testGuard(map[string][]string{"public.test": {"93.184.216.34"}, "sneaky.test": {"192.168.0.10"}})
	g.allowLoopback = false // the proxy is trusted explicitly, not via loopback
	f := newFetcher(FetchOptions{Proxy: http.ProxyURL(pu)}, g)
	p, err := f.Fetch(context.Background(), "http://public.test/readme")
	if err != nil || p.Content != "via proxy" || seen.Load() != "http://public.test/readme" {
		t.Fatalf("proxied fetch: %+v %v %v", p, err, seen.Load())
	}
	if _, err := f.Fetch(context.Background(), "http://sneaky.test/"); err == nil || !strings.Contains(err.Error(), "192.168.0.10") {
		t.Fatalf("private target via proxy must be blocked: %v", err)
	}
}

func TestFetchContentHandling(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/latin1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=ISO-8859-1")
		w.Write([]byte("caf\xe9"))
	})
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// "Привет" in windows-1251, declared only in a meta tag.
		w.Write([]byte(`<html><head><meta charset="windows-1251"></head><body><p>` + "\xcf\xf0\xe8\xe2\xe5\xf2" + `</p></body></html>`))
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"a":[1,2]}`))
	})
	mux.HandleFunc("/png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG\r\n\x1a\n"))
	})
	mux.HandleFunc("/pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.7"))
	})
	mux.HandleFunc("/octet", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("# README\nplain text"))
	})
	mux.HandleFunc("/untyped", func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		w.Write([]byte("<!DOCTYPE html><html><body><h2>Sniffed</h2></body></html>"))
	})
	mux.HandleFunc("/gzip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write([]byte("decompressed body"))
		gz.Close()
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("x", 5000)))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("<h1>Not here</h1><p>Try the index.</p>"))
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	loop := 0
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		loop++
		http.Redirect(w, r, fmt.Sprintf("/loop?n=%d", loop), http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := newFetcher(FetchOptions{MaxBytes: 1000, Timeout: 300 * time.Millisecond, MaxRedirects: 3, CacheTTL: -1}, testGuard(nil))
	ctx := context.Background()

	get := func(path string) *Page {
		t.Helper()
		p, err := f.Fetch(ctx, srv.URL+path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return p
	}
	if p := get("/latin1"); p.Content != "café" {
		t.Errorf("latin1: %q", p.Content)
	}
	if p := get("/meta"); p.Content != "Привет" {
		t.Errorf("meta charset: %q", p.Content)
	}
	if p := get("/json"); p.Content != "{\n  \"a\": [\n    1,\n    2\n  ]\n}" {
		t.Errorf("json: %q", p.Content)
	}
	if p := get("/octet"); p.Content != "# README\nplain text" {
		t.Errorf("octet: %q", p.Content)
	}
	if p := get("/untyped"); p.Content != "## Sniffed" {
		t.Errorf("sniffed: %q", p.Content)
	}
	if p := get("/gzip"); p.Content != "decompressed body" {
		t.Errorf("gzip: %q", p.Content)
	}
	if p := get("/big"); !p.Truncated || len(p.Content) != 1000 {
		t.Errorf("size cap: truncated=%v len=%d", p.Truncated, len(p.Content))
	}
	if out := render(get("/big"), false, 0, 50); !strings.Contains(out, "cut off") {
		t.Errorf("render must mention truncation: %s", out)
	}
	for path, want := range map[string]string{
		"/png":     "an image (image/png)",
		"/pdf":     "a PDF document",
		"/missing": "HTTP 404 Not Found",
		"/slow":    "timed out",
		"/loop":    "stopped after 3 redirects",
	} {
		_, err := f.Fetch(ctx, srv.URL+path)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", path, want, err)
		}
	}
	_, err := f.Fetch(ctx, srv.URL+"/missing")
	var fe *FetchError
	if !errors.As(err, &fe) || !strings.Contains(fe.Excerpt, "# Not here") {
		t.Errorf("404 excerpt: %v", err)
	}
}

func TestPageCacheLRUAndTTL(t *testing.T) {
	c := newPageCache(time.Minute, 100)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	c.put("a", &Page{Content: strings.Repeat("a", 40)})
	c.put("b", &Page{Content: strings.Repeat("b", 40)})
	c.get("a") // a is now most recent
	c.put("c", &Page{Content: strings.Repeat("c", 40)})
	if _, ok := c.get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.get("a"); !ok {
		t.Fatal("a should be kept")
	}
	c.put("huge", &Page{Content: strings.Repeat("h", 200)})
	if _, ok := c.get("huge"); ok {
		t.Fatal("oversize entries are not cached")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := c.get("a"); ok || c.len() != 1 {
		t.Fatalf("expired entries must go: len=%d", c.len())
	}
}

func TestToolsFromEnv(t *testing.T) {
	ts, err := Tools(Options{Getenv: func(string) string { return "" }})
	if !errors.Is(err, ErrNoSearchBackend) || len(ts) != 1 || ts[0].Spec().Name != "web_fetch" {
		t.Fatalf("no backend: %v %v", ts, err)
	}
	env := map[string]string{EnvTavilyKey: "tv-key"}
	ts, err = Tools(Options{Getenv: func(k string) string { return env[k] }})
	if err != nil || len(ts) != 2 || ts[1].Spec().Name != "web_search" {
		t.Fatalf("tavily: %v %v", ts, err)
	}
}
