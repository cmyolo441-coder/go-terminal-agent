package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPEmbedder(t *testing.T) {
	var calls, failures atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer k-123" || r.Header.Get("X-Org") != "o" {
			http.Error(w, "bad request shape", http.StatusBadRequest)
			return
		}
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "m" || req.Dimensions != 4 {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if strings.Contains(req.Input[0], "flaky") && failures.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
			return
		}
		if strings.Contains(req.Input[0], "reject") {
			http.Error(w, `{"error":{"message":"invalid key k-123"}}`, http.StatusUnauthorized)
			return
		}
		resp := embedResponse{}
		// Answer in reverse order to exercise index sorting.
		for i := len(req.Input) - 1; i >= 0; i-- {
			resp.Data = append(resp.Data, embedDatum{Index: i, Embedding: []float32{float32(len(req.Input[i])), 1, 0, 0}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	e, err := NewHTTPEmbedder(HTTPEmbedderConfig{
		BaseURL: srv.URL + "/v1/", APIKey: "k-123", Model: "m", Dimensions: 4, BatchSize: 2,
		Headers: map[string]string{"X-Org": "o"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	e.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	ctx := context.Background()
	vecs, err := e.Embed(ctx, []string{"a", "bb", "ccc", "dddd", "eeeee"})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(vecs) != 5 {
		t.Fatalf("calls %d vecs %d", calls.Load(), len(vecs))
	}
	for i, v := range vecs {
		if v[0] != float32(i+1) {
			t.Fatalf("vector %d out of order: %v", i, v)
		}
	}
	if e.Dim() != 4 || e.ID() != "http/m/d4" {
		t.Fatalf("dim %d id %s", e.Dim(), e.ID())
	}
	// Retries on 429 honour Retry-After.
	if _, err := e.Embed(ctx, []string{"flaky"}); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 2 || slept[0] != 0 {
		t.Fatalf("backoff %v", slept)
	}
	// 401 is not retried, and the key is not echoed.
	before := calls.Load()
	_, err = e.Embed(ctx, []string{"reject"})
	var he *HTTPError
	if !errors.Is(err, ErrEmbedding) || !errors.As(err, &he) || he.Status != 401 || strings.Contains(err.Error(), "k-123") {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != before+1 {
		t.Fatal("non-retryable error was retried")
	}
	// It plugs into a store.
	s, err := Open(ctx, Options{Embedder: e, Redact: testRedact})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Remember(ctx, Record{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPEmbedderErrors(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,2,3]}]}`)
		case 2:
			fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,2]}]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	e, _ := NewHTTPEmbedder(HTTPEmbedderConfig{BaseURL: srv.URL, Model: "m", MaxRetries: 2})
	e.sleep = func(context.Context, time.Duration) error { return nil }
	ctx := context.Background()
	if _, err := e.Embed(ctx, []string{"x"}); err != nil || e.Dim() != 3 {
		t.Fatalf("learn dim: %v %d", err, e.Dim())
	}
	if _, err := e.Embed(ctx, []string{"x"}); err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("dim mismatch: %v", err)
	}
	if _, err := e.Embed(ctx, []string{"x"}); err == nil || n.Load() != 5 {
		t.Fatalf("5xx retries: %v calls %d", err, n.Load())
	}
	if _, err := NewHTTPEmbedder(HTTPEmbedderConfig{BaseURL: "ftp://x", Model: "m"}); err == nil {
		t.Fatal("bad scheme accepted")
	}
	if d, ok := parseRetryAfter("Wed, 21 Oct 2015 07:28:00 GMT", time.Date(2015, 10, 21, 7, 27, 0, 0, time.UTC)); !ok || d != time.Minute {
		t.Fatalf("http-date retry-after %v %v", d, ok)
	}
	b := e.backoff(3, errors.New("x"))
	if b < 500*time.Millisecond || b > 2*time.Second {
		t.Fatalf("backoff %v", b)
	}
}
