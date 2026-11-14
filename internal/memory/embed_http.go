package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPEmbedderConfig configures an HTTPEmbedder.
type HTTPEmbedderConfig struct {
	// BaseURL of an OpenAI-compatible API, e.g. "https://api.openai.com/v1"
	// or "http://localhost:11434/v1". "/embeddings" is appended unless the
	// URL already ends with it.
	BaseURL string
	// APIKey is sent as a bearer token when non-empty.
	APIKey string
	// Model is the embedding model name (required).
	Model string
	// Dimensions, when positive, is sent as the "dimensions" request field
	// (supported by text-embedding-3-* and many compatible servers) and is
	// the expected vector size. When zero the size is learned from the
	// first response.
	Dimensions int
	// BatchSize is the number of inputs per request (default 64).
	BatchSize int
	// MaxInputBytes truncates each input on a UTF-8 boundary (default
	// 24000, comfortably under the 8k-token limit of common models).
	MaxInputBytes int
	// MaxRetries is the number of retries after the first attempt for
	// network errors, 408, 409, 429 and 5xx responses (default 4).
	MaxRetries int
	// InitialBackoff and MaxBackoff bound the exponential backoff with full
	// jitter (defaults 500ms and 20s). A Retry-After header takes priority
	// up to MaxBackoff.
	InitialBackoff, MaxBackoff time.Duration
	// Timeout bounds one HTTP attempt (default 60s).
	Timeout time.Duration
	// Headers are added to every request (e.g. organisation or project
	// headers, or an "api-key" header for Azure-style gateways).
	Headers map[string]string
	// Client is the HTTP client (default: a client with no overall timeout;
	// Timeout is applied per attempt through the context).
	Client *http.Client
}

// HTTPEmbedder calls an OpenAI-compatible /v1/embeddings endpoint.
type HTTPEmbedder struct {
	cfg      HTTPEmbedderConfig
	endpoint string
	mu       sync.RWMutex
	dim      int
	sleep    func(ctx context.Context, d time.Duration) error
}

// NewHTTPEmbedder validates cfg and returns an embedder.
func NewHTTPEmbedder(cfg HTTPEmbedderConfig) (*HTTPEmbedder, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, errors.New("memory: http embedder needs a base URL")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("memory: http embedder base URL %q is not http(s)", base)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("memory: http embedder needs a model")
	}
	if cfg.Dimensions < 0 {
		return nil, errors.New("memory: negative embedding dimensions")
	}
	base = strings.TrimRight(base, "/")
	endpoint := base
	if !strings.HasSuffix(base, "/embeddings") {
		endpoint = base + "/embeddings"
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 64
	}
	if cfg.MaxInputBytes <= 0 {
		cfg.MaxInputBytes = 24000
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	} else if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 4
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = 500 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 20 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	return &HTTPEmbedder{cfg: cfg, endpoint: endpoint, dim: cfg.Dimensions, sleep: sleepCtx}, nil
}

// Dim implements Embedder; 0 until known.
func (e *HTTPEmbedder) Dim() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dim
}

// ID implements Embedder.
func (e *HTTPEmbedder) ID() string {
	id := "http/" + e.cfg.Model
	if e.cfg.Dimensions > 0 {
		id += "/d" + strconv.Itoa(e.cfg.Dimensions)
	}
	return id
}

// Embed implements Embedder, sending the texts in batches.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += e.cfg.BatchSize {
		end := min(start+e.cfg.BatchSize, len(texts))
		vecs, err := e.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

type embedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
	Dimensions     int      `json:"dimensions,omitempty"`
}

type embedDatum struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embedResponse struct {
	Data  []embedDatum `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// HTTPError is a non-2xx response from the embedding endpoint.
type HTTPError struct {
	Status    int
	Message   string
	Retryable bool
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("embeddings endpoint returned %d: %s", e.Status, e.Message)
}

const maxEmbedResponse = 256 << 20

func (e *HTTPEmbedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	inputs := make([]string, len(texts))
	for i, t := range texts {
		t = truncateUTF8(strings.ToValidUTF8(t, "�"), e.cfg.MaxInputBytes)
		if strings.TrimSpace(t) == "" {
			t = " " // most servers reject empty inputs
		}
		inputs[i] = t
	}
	body, err := json.Marshal(embedRequest{Model: e.cfg.Model, Input: inputs, EncodingFormat: "float", Dimensions: e.cfg.Dimensions})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= e.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := e.backoff(attempt, lastErr)
			if err := e.sleep(ctx, wait); err != nil {
				return nil, fmt.Errorf("%w: %w (last error: %w)", ErrEmbedding, err, lastErr)
			}
		}
		vecs, err := e.attempt(ctx, body, len(texts))
		if err == nil {
			return vecs, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", ErrEmbedding, ctx.Err())
		}
		var he *HTTPError
		if errors.As(err, &he) && !he.Retryable {
			break
		}
		var de dimError
		if errors.As(err, &de) {
			break
		}
	}
	return nil, fmt.Errorf("%w: %w", ErrEmbedding, lastErr)
}

type dimError struct{ msg string }

func (d dimError) Error() string { return d.msg }

// retryAfterError carries a server-requested delay.
type retryAfterError struct {
	*HTTPError
	after time.Duration
}

func (e *HTTPEmbedder) attempt(ctx context.Context, body []byte, n int) ([][]float32, error) {
	actx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range e.cfg.Headers {
		req.Header.Set(k, v)
	}
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	resp, err := e.cfg.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbedResponse+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxEmbedResponse {
		return nil, &HTTPError{Status: resp.StatusCode, Message: "response too large"}
	}
	if resp.StatusCode/100 != 2 {
		he := &HTTPError{Status: resp.StatusCode, Message: errorMessage(data, e.cfg.APIKey)}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusRequestTimeout,
			resp.StatusCode == http.StatusConflict, resp.StatusCode >= 500:
			he.Retryable = true
		}
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok && he.Retryable {
			return nil, retryAfterError{he, d}
		}
		return nil, he
	}
	var parsed embedResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("decoding embeddings response: %w", err)
	}
	if parsed.Error != nil {
		return nil, &HTTPError{Status: resp.StatusCode, Message: parsed.Error.Message}
	}
	if len(parsed.Data) != n {
		return nil, fmt.Errorf("embeddings response has %d vectors for %d inputs", len(parsed.Data), n)
	}
	slices.SortFunc(parsed.Data, func(a, b embedDatum) int { return a.Index - b.Index })
	out := make([][]float32, n)
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, d := range parsed.Data {
		if d.Index != i {
			return nil, fmt.Errorf("embeddings response indexes are not 0..%d", n-1)
		}
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embeddings response has an empty vector at %d", i)
		}
		if e.dim == 0 {
			e.dim = len(d.Embedding)
		}
		if len(d.Embedding) != e.dim {
			return nil, dimError{fmt.Sprintf("embedding has %d dimensions, expected %d", len(d.Embedding), e.dim)}
		}
		out[i] = d.Embedding
	}
	return out, nil
}

func (e retryAfterError) Unwrap() error { return e.HTTPError }

func (e *HTTPEmbedder) backoff(attempt int, lastErr error) time.Duration {
	var ra retryAfterError
	if errors.As(lastErr, &ra) {
		return min(ra.after, e.cfg.MaxBackoff)
	}
	ceil := e.cfg.InitialBackoff << min(attempt-1, 20)
	if ceil <= 0 || ceil > e.cfg.MaxBackoff {
		ceil = e.cfg.MaxBackoff
	}
	// Full jitter, but never less than a quarter of the ceiling.
	return ceil/4 + time.Duration(rand.Int64N(int64(ceil-ceil/4)+1))
}

// errorMessage extracts a short message from an error body, never echoing
// the API key.
func errorMessage(data []byte, key string) string {
	var parsed embedResponse
	msg := ""
	if json.Unmarshal(data, &parsed) == nil && parsed.Error != nil {
		msg = parsed.Error.Message
	}
	if msg == "" {
		msg = strings.TrimSpace(string(data))
	}
	if key != "" {
		msg = strings.ReplaceAll(msg, key, "[REDACTED]")
	}
	msg = strings.ToValidUTF8(msg, "?")
	if len(msg) > 300 {
		msg = truncateUTF8(msg, 300) + "..."
	}
	if msg == "" {
		msg = "(empty body)"
	}
	return msg
}

func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
