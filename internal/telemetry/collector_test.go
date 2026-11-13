package telemetry

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// collector is a fake OTLP/HTTP receiver that decodes every request body
// as generic JSON so tests check the wire format, not the SDK's structs.
type collector struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []collected
	// respond, if set, chooses the response for the n-th request (0-based).
	respond func(n int, w http.ResponseWriter) bool
}

type collected struct {
	Path    string
	Header  http.Header
	Body    map[string]any
	Gzipped bool
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{t: t}
	c.srv = httptest.NewServer(http.HandlerFunc(c.handle))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *collector) handle(w http.ResponseWriter, r *http.Request) {
	var rd io.Reader = r.Body
	gz := r.Header.Get("Content-Encoding") == "gzip"
	if gz {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rd = zr
	}
	raw, err := io.ReadAll(rd)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		c.t.Errorf("collector: body is not JSON: %v: %s", err, raw)
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	n := len(c.requests)
	c.requests = append(c.requests, collected{Path: r.URL.Path, Header: r.Header.Clone(), Body: body, Gzipped: gz})
	respond := c.respond
	c.mu.Unlock()
	if respond != nil && respond(n, w) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (c *collector) Requests() []collected {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]collected(nil), c.requests...)
}

func (c *collector) ByPath(path string) []collected {
	var out []collected
	for _, r := range c.Requests() {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// JSON navigation helpers. They fail the test on a shape mismatch.

func obj(t *testing.T, v any, key string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected object holding %q, got %T", key, v)
	}
	o, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is %T, want object (in %v)", key, m[key], m)
	}
	return o
}

func arr(t *testing.T, v any, key string) []any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected object holding %q, got %T", key, v)
	}
	a, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%q is %T, want array (in %v)", key, m[key], m)
	}
	return a
}

func str(t *testing.T, v any, key string) string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected object holding %q, got %T", key, v)
	}
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("%q is %T (%v), want string", key, m[key], m[key])
	}
	return s
}

func num(t *testing.T, v any, key string) float64 {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected object holding %q, got %T", key, v)
	}
	f, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%q is %T (%v), want number", key, m[key], m[key])
	}
	return f
}

// int64Str checks a proto3-JSON int64: a string of decimal digits.
func int64Str(t *testing.T, v any, key string) int64 {
	t.Helper()
	s := str(t, v, key)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("%q = %q is not a decimal int64 string", key, s)
	}
	return n
}

// attrMap flattens an OTLP attribute list to key -> AnyValue object.
func attrMap(t *testing.T, v any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	m, _ := v.(map[string]any)
	list, _ := m["attributes"].([]any)
	for _, a := range list {
		kv := a.(map[string]any)
		out[kv["key"].(string)] = kv["value"].(map[string]any)
	}
	return out
}

func strAttr(t *testing.T, attrs map[string]map[string]any, key string) string {
	t.Helper()
	v, ok := attrs[key]
	if !ok {
		t.Fatalf("attribute %q missing (have %v)", key, attrs)
	}
	s, ok := v["stringValue"].(string)
	if !ok {
		t.Fatalf("attribute %q = %v, want stringValue", key, v)
	}
	return s
}
