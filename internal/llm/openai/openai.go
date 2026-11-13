// Package openai implements llm.Provider for any endpoint that speaks the
// OpenAI Chat Completions API: OpenAI itself, OpenRouter, Ollama, vLLM,
// LM Studio, Groq, Together, DeepSeek and Google Gemini's OpenAI-compatible
// endpoint.
//
// It uses raw net/http and Server-Sent Events, so it adds no dependencies.
// Responses always stream; tool-call fragments are reassembled by index,
// reasoning deltas (reasoning_content / reasoning) surface as thinking, and
// transient failures before the stream starts are retried with exponential
// backoff that honours Retry-After. Nothing is retried once streaming has
// begun, because a partial answer cannot be replayed safely.
package openai

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

// DefaultName labels a provider whose Options leave Name empty.
const DefaultName = "openai"

// Defaults applied when the corresponding Options field is zero.
const (
	// DefaultMaxRetries is how many times a failed request is retried
	// before the stream starts.
	DefaultMaxRetries = 2
	// DefaultTimeout bounds the wait for response headers on each attempt.
	// It is generous because local servers such as Ollama load the model
	// into memory before they answer the first request.
	DefaultTimeout = 5 * time.Minute
	// DefaultIdleTimeout bounds the silence between two pieces of a stream.
	DefaultIdleTimeout = 5 * time.Minute
)

// Options configure a Provider. Build them by hand for a custom endpoint, or
// start from Preset and override fields.
type Options struct {
	// Name labels the provider in errors and is stored as Message.Provider
	// on the messages it produces. The name "openai" also selects
	// OpenAI-only request fields (max_completion_tokens, reasoning_effort).
	Name string
	// BaseURL is the API root, for example https://api.openai.com/v1. The
	// provider posts to BaseURL + "/chat/completions"; a BaseURL that
	// already ends in /chat/completions is used as is.
	BaseURL string
	// APIKey is sent as a Bearer token. Empty sends no Authorization header,
	// which suits local servers.
	APIKey string
	// APIKeyEnv names the environment variable the key comes from. It is
	// only used to make authentication errors actionable.
	APIKeyEnv string
	// RequireAPIKey makes Stream fail fast, without a network call, when
	// APIKey is empty.
	RequireAPIKey bool
	// ExtraHeaders are added to every request (for example an organisation
	// or project header, or a gateway's routing header).
	ExtraHeaders map[string]string
	// MaxRetries is the number of retries for transient failures before
	// the stream starts. Zero uses DefaultMaxRetries; negative disables
	// retries.
	MaxRetries int
	// Timeout bounds the wait for response headers on each attempt. Zero
	// uses DefaultTimeout; negative disables the limit. It never cuts off a
	// stream that is producing output.
	Timeout time.Duration
	// IdleTimeout bounds the silence between stream chunks once the
	// response has started. Zero uses DefaultIdleTimeout; negative disables
	// the limit.
	IdleTimeout time.Duration
	// HTTPClient overrides the HTTP client (for proxies or custom TLS). Its
	// own Timeout must be zero or it will cut long streams short.
	HTTPClient *http.Client
}

// PresetInfo describes a well-known OpenAI-compatible service. It is
// exposed so the configuration layer and the /model command can list
// providers and suggest model ids; Preset turns one into Options.
type PresetInfo struct {
	// Name is the preset key used in configuration ("openai", "nvidia", ...).
	Name string
	// Description is a human-readable label.
	Description string
	// BaseURL is the default API root.
	BaseURL string
	// KeyEnvs are the environment variables the API key is read from, in
	// order of preference. The first is the one to tell users to set.
	KeyEnvs []string
	// RequireKey is true for hosted services that reject anonymous calls.
	RequireKey bool
	// BaseURLEnvs may override BaseURL from the environment.
	BaseURLEnvs []string
	// KnownModels are suggested model ids. Model ids stay free-form: any id
	// the service accepts works, these are only suggestions.
	KnownModels []string
}

var presets = map[string]PresetInfo{
	"openai": {
		Description: "OpenAI", BaseURL: "https://api.openai.com/v1",
		KeyEnvs: []string{"OPENAI_API_KEY"}, RequireKey: true, BaseURLEnvs: []string{"OPENAI_BASE_URL"},
	},
	"openrouter": {
		Description: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1",
		KeyEnvs: []string{"OPENROUTER_API_KEY"}, RequireKey: true,
	},
	"ollama": {
		Description: "Ollama (local)", BaseURL: "http://localhost:11434/v1",
		KeyEnvs: []string{"OLLAMA_API_KEY"}, BaseURLEnvs: []string{"OLLAMA_HOST"},
	},
	"gemini": {
		Description: "Google Gemini (OpenAI-compatible endpoint)", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		KeyEnvs: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, RequireKey: true,
	},
	"groq": {
		Description: "Groq", BaseURL: "https://api.groq.com/openai/v1",
		KeyEnvs: []string{"GROQ_API_KEY"}, RequireKey: true,
	},
	"deepseek": {
		Description: "DeepSeek", BaseURL: "https://api.deepseek.com/v1",
		KeyEnvs: []string{"DEEPSEEK_API_KEY"}, RequireKey: true,
	},
	"together": {
		Description: "Together AI", BaseURL: "https://api.together.xyz/v1",
		KeyEnvs: []string{"TOGETHER_API_KEY"}, RequireKey: true,
	},
	"nvidia": {
		Description: "NVIDIA NIM", BaseURL: "https://integrate.api.nvidia.com/v1",
		KeyEnvs: []string{"NVIDIA_API_KEY"}, RequireKey: true,
		KnownModels: []string{"mistralai/mistral-large-4-1t-a52b", "kimi-k3.1-pro"},
	},
	"lmstudio": {
		Description: "LM Studio (local)", BaseURL: "http://localhost:1234/v1",
		KeyEnvs: []string{"LMSTUDIO_API_KEY"},
	},
	"vllm": {
		Description: "vLLM (local)", BaseURL: "http://localhost:8000/v1",
		KeyEnvs: []string{"VLLM_API_KEY"},
	},
}

// PresetNames lists the names Preset accepts, sorted.
func PresetNames() []string {
	names := make([]string, 0, len(presets))
	for n := range presets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Presets returns every preset, sorted by name. The slices are copies, so
// callers may modify them.
func Presets() []PresetInfo {
	out := make([]PresetInfo, 0, len(presets))
	for _, n := range PresetNames() {
		info, _ := LookupPreset(n)
		out = append(out, info)
	}
	return out
}

// LookupPreset returns the preset with the given name (case-insensitive).
func LookupPreset(name string) (PresetInfo, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	info, ok := presets[key]
	if !ok {
		return PresetInfo{}, false
	}
	info.Name = key
	info.KeyEnvs = append([]string(nil), info.KeyEnvs...)
	info.BaseURLEnvs = append([]string(nil), info.BaseURLEnvs...)
	info.KnownModels = append([]string(nil), info.KnownModels...)
	return info, true
}

// KnownModels returns the suggested model ids for a preset, or nil.
func KnownModels(name string) []string {
	info, _ := LookupPreset(name)
	return info.KnownModels
}

// Preset returns Options for a well-known service. Keys are read only from
// the environment, never from code or config: OPENAI_API_KEY,
// OPENROUTER_API_KEY, GEMINI_API_KEY (or GOOGLE_API_KEY), GROQ_API_KEY,
// DEEPSEEK_API_KEY, TOGETHER_API_KEY and NVIDIA_API_KEY; local servers need
// none but honour OLLAMA_API_KEY, LMSTUDIO_API_KEY and VLLM_API_KEY.
// OPENAI_BASE_URL overrides the openai base URL and OLLAMA_HOST (for example
// 127.0.0.1:11434) the ollama one. Callers may override any field
// afterwards, for example BaseURL from a config file.
//
// A hosted preset whose key is unset still returns Options, with
// RequireAPIKey set, so Stream fails fast with a message naming the
// variable to export; use MissingKeyError to report it earlier.
func Preset(name string) (Options, error) {
	info, ok := LookupPreset(name)
	if !ok {
		return Options{}, fmt.Errorf("unknown OpenAI-compatible provider %q (known: %s)", name, strings.Join(PresetNames(), ", "))
	}
	opts := Options{Name: info.Name, BaseURL: info.BaseURL, RequireAPIKey: info.RequireKey}
	if len(info.KeyEnvs) > 0 {
		opts.APIKeyEnv = info.KeyEnvs[0]
	}
	for _, env := range info.KeyEnvs {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			opts.APIKey, opts.APIKeyEnv = v, env
			break
		}
	}
	for _, env := range info.BaseURLEnvs {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			if env == "OLLAMA_HOST" {
				v = ollamaBaseURL(v)
			}
			opts.BaseURL = v
			break
		}
	}
	return opts, nil
}

// MissingKeyError returns a clear error when opts require an API key and
// none is set, and nil otherwise. Stream performs the same check.
func MissingKeyError(opts Options) error {
	if !opts.RequireAPIKey || strings.TrimSpace(opts.APIKey) != "" {
		return nil
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = DefaultName
	}
	if opts.APIKeyEnv == "" {
		return fmt.Errorf("%s: no API key configured", name)
	}
	return fmt.Errorf("%s: no API key: export %s=<your key> and try again", name, opts.APIKeyEnv)
}

// ollamaBaseURL turns an OLLAMA_HOST value (host, host:port or a full URL)
// into the OpenAI-compatible API root.
func ollamaBaseURL(host string) string {
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	u, err := url.Parse(host)
	if err != nil || u.Host == "" {
		return presets["ollama"].BaseURL
	}
	if u.Port() == "" {
		u.Host += ":11434"
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/v1") {
		u.Path += "/v1"
	}
	return u.String()
}

// Provider talks to one OpenAI-compatible endpoint. It is safe for
// concurrent use.
type Provider struct {
	opts     Options
	endpoint string
	client   *http.Client
	retries  int
	timeout  time.Duration
	idle     time.Duration
	// sleep waits between retries; tests replace it to stay fast.
	sleep func(ctx context.Context, d time.Duration) error
	// jitter returns a value in [0, 1); tests replace it for determinism.
	jitter func() float64
}

// New validates opts and builds a provider.
func New(opts Options) (*Provider, error) {
	opts.Name = strings.TrimSpace(opts.Name)
	if opts.Name == "" {
		opts.Name = DefaultName
	}
	opts.APIKey = strings.TrimSpace(opts.APIKey)
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("%s: base URL is required", opts.Name)
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid base URL %q: %w", opts.Name, opts.BaseURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s: base URL %q must be an absolute http(s) URL", opts.Name, opts.BaseURL)
	}
	opts.BaseURL = base
	endpoint := base
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	for k, v := range opts.ExtraHeaders {
		if !validHeaderName(k) {
			return nil, fmt.Errorf("%s: invalid extra header name %q", opts.Name, k)
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return nil, fmt.Errorf("%s: extra header %q has an invalid value", opts.Name, k)
		}
	}
	p := &Provider{
		opts:     opts,
		endpoint: endpoint,
		client:   opts.HTTPClient,
		retries:  pick(opts.MaxRetries, DefaultMaxRetries),
		timeout:  pickDur(opts.Timeout, DefaultTimeout),
		idle:     pickDur(opts.IdleTimeout, DefaultIdleTimeout),
		sleep:    sleepCtx,
		jitter:   rand.Float64,
	}
	if p.client == nil {
		p.client = &http.Client{Transport: http.DefaultTransport}
	}
	return p, nil
}

// NewPreset is Preset followed by New.
func NewPreset(name string) (*Provider, error) {
	opts, err := Preset(name)
	if err != nil {
		return nil, err
	}
	return New(opts)
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return p.opts.Name }

// Endpoint is the full Chat Completions URL requests go to.
func (p *Provider) Endpoint() string { return p.endpoint }

// Stream implements llm.Provider. It sends one streaming Chat Completions
// request, reports progress through onEvent and returns the assembled reply.
func (p *Provider) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	if onEvent == nil {
		onEvent = func(llm.Event) {}
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("%s: no model configured", p.opts.Name)
	}
	if err := MissingKeyError(p.opts); err != nil {
		return nil, err
	}
	body, err := p.buildBody(req)
	if err != nil {
		return nil, err
	}
	resp, attemptCtx, cancel, err := p.send(ctx, body, req.Model)
	if err != nil {
		return nil, err
	}
	defer cancel(nil)
	defer resp.Body.Close()
	return p.read(ctx, attemptCtx, cancel, resp, req.Model, onEvent)
}

func pick(v, def int) int {
	switch {
	case v < 0:
		return 0
	case v == 0:
		return def
	}
	return v
}

func pickDur(v, def time.Duration) time.Duration {
	switch {
	case v < 0:
		return 0
	case v == 0:
		return def
	}
	return v
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// validHeaderName reports whether s is an RFC 7230 token.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, c) >= 0 {
			return false
		}
	}
	return true
}

func userAgent() string { return "gta/" + version.Version }
