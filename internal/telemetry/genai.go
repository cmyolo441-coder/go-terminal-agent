package telemetry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/pricing"
)

// GenAI semantic-convention attribute keys.
const (
	AttrGenAIOperationName       = "gen_ai.operation.name"
	AttrGenAISystem              = "gen_ai.system"
	AttrGenAIProviderName        = "gen_ai.provider.name"
	AttrGenAIRequestModel        = "gen_ai.request.model"
	AttrGenAIRequestMaxTokens    = "gen_ai.request.max_tokens"
	AttrGenAIResponseModel       = "gen_ai.response.model"
	AttrGenAIResponseFinish      = "gen_ai.response.finish_reasons"
	AttrGenAIUsageInputTokens    = "gen_ai.usage.input_tokens"
	AttrGenAIUsageOutputTokens   = "gen_ai.usage.output_tokens"
	AttrGenAIUsageCacheRead      = "gen_ai.usage.cache_read.input_tokens"
	AttrGenAIUsageCacheCreation  = "gen_ai.usage.cache_creation.input_tokens"
	AttrGenAITokenType           = "gen_ai.token.type"
	AttrGenAIToolName            = "gen_ai.tool.name"
	AttrGenAIToolCallID          = "gen_ai.tool.call.id"
	AttrGenAIToolType            = "gen_ai.tool.type"
	AttrGenAIAgentName           = "gen_ai.agent.name"
	AttrGenAIConversationID      = "gen_ai.conversation.id"
	AttrErrorType                = "error.type"
	AttrGTACostUSD               = "gta.cost.usd"
	AttrGTARequestEffort         = "gta.request.effort"
	AttrGTARequestMessages       = "gta.request.messages"
	AttrGTARequestTools          = "gta.request.tools"
	AttrGTAToolOutcome           = "gta.tool.outcome"
	AttrGTAComponent             = "gta.component"
	AttrGTARoute                 = "gta.route"
	AttrGTAReason                = "gta.reason"
	OperationChat                = "chat"
	OperationExecuteTool         = "execute_tool"
	OperationInvokeAgent         = "invoke_agent"
	errorTypeOther               = "_OTHER"
	instrumentationName          = "github.com/cmyolo441-coder/go-terminal-agent/internal/telemetry"
	genAISchemaURL               = "https://opentelemetry.io/schemas/1.37.0"
	defaultMetricIntervalSeconds = 60
)

// Bucket boundaries recommended by the GenAI semantic conventions.
var (
	durationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	tokenBuckets    = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
	ttftBuckets     = []float64{0.001, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}
	tpotBuckets     = []float64{0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.4, 0.5, 0.75, 1, 2.5}
)

// Instruments are the metrics GTA records. Every method is safe on a nil
// receiver and on instruments that were not created.
type Instruments struct {
	TokenUsage         *Int64Histogram   // gen_ai.client.token.usage
	OperationDuration  *Float64Histogram // gen_ai.client.operation.duration
	TimeToFirstToken   *Float64Histogram // gen_ai.server.time_to_first_token
	TimePerOutputToken *Float64Histogram // gen_ai.server.time_per_output_token
	Tokens             *Int64Counter     // gta.tokens
	Cost               *Float64Counter   // gta.cost
	ToolCalls          *Int64Counter     // gta.tool.calls
	ToolDuration       *Float64Histogram // gta.tool.duration
	Errors             *Int64Counter     // gta.errors
	Retries            *Int64Counter     // gta.model.retries
	Fallbacks          *Int64Counter     // gta.model.fallbacks
}

// NewInstruments creates GTA's instruments on m.
func NewInstruments(m *Meter) (*Instruments, error) {
	var in Instruments
	var errs []error
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	var err error
	in.TokenUsage, err = m.Int64Histogram("gen_ai.client.token.usage", WithUnit("{token}"),
		WithDescription("Number of input and output tokens used per model call."), WithExplicitBuckets(tokenBuckets...))
	keep(err)
	in.OperationDuration, err = m.Float64Histogram("gen_ai.client.operation.duration", WithUnit("s"),
		WithDescription("Duration of model calls."), WithExplicitBuckets(durationBuckets...))
	keep(err)
	in.TimeToFirstToken, err = m.Float64Histogram("gen_ai.server.time_to_first_token", WithUnit("s"),
		WithDescription("Time from request to the first streamed token, as seen by the client."), WithExplicitBuckets(ttftBuckets...))
	keep(err)
	in.TimePerOutputToken, err = m.Float64Histogram("gen_ai.server.time_per_output_token", WithUnit("s"),
		WithDescription("Time per output token after the first, as seen by the client."), WithExplicitBuckets(tpotBuckets...))
	keep(err)
	in.Tokens, err = m.Int64Counter("gta.tokens", WithUnit("{token}"),
		WithDescription("Tokens consumed, by model and token type (input, output, cache_read, cache_write)."))
	keep(err)
	in.Cost, err = m.Float64Counter("gta.cost", WithUnit("USD"), WithDescription("Estimated spend on model calls."))
	keep(err)
	in.ToolCalls, err = m.Int64Counter("gta.tool.calls", WithUnit("{call}"), WithDescription("Tool calls, by tool name and outcome."))
	keep(err)
	in.ToolDuration, err = m.Float64Histogram("gta.tool.duration", WithUnit("s"),
		WithDescription("Duration of tool calls."), WithExplicitBuckets(durationBuckets...))
	keep(err)
	in.Errors, err = m.Int64Counter("gta.errors", WithUnit("{error}"), WithDescription("Errors, by component and error type."))
	keep(err)
	in.Retries, err = m.Int64Counter("gta.model.retries", WithUnit("{retry}"), WithDescription("Model call retries, by route and reason."))
	keep(err)
	in.Fallbacks, err = m.Int64Counter("gta.model.fallbacks", WithUnit("{fallback}"), WithDescription("Fallbacks from one model route to the next."))
	keep(err)
	return &in, errors.Join(errs...)
}

// RecordError counts one error of kind in component (for example "model",
// "tool", "mcp").
func (in *Instruments) RecordError(ctx context.Context, component, kind string) {
	if in == nil {
		return
	}
	in.Errors.Add(ctx, 1, String(AttrGTAComponent, component), String(AttrErrorType, orOther(kind)))
}

// RecordRetry counts one retry of a model route.
func (in *Instruments) RecordRetry(ctx context.Context, route, reason string) {
	if in == nil {
		return
	}
	in.Retries.Add(ctx, 1, String(AttrGTARoute, route), String(AttrGTAReason, orOther(reason)))
}

// RecordFallback counts one fallback between model routes.
func (in *Instruments) RecordFallback(ctx context.Context, from, to, reason string) {
	if in == nil {
		return
	}
	in.Fallbacks.Add(ctx, 1, String("gta.route.from", from), String("gta.route.to", to), String(AttrGTAReason, orOther(reason)))
}

func orOther(s string) string {
	if strings.TrimSpace(s) == "" {
		return errorTypeOther
	}
	return s
}

// GenAISystem maps a GTA provider name onto the gen_ai.system /
// gen_ai.provider.name well-known values where one exists.
func GenAISystem(provider string) string {
	switch p := strings.ToLower(provider); p {
	case "gemini":
		return "gcp.gemini"
	case "mistral":
		return "mistral_ai"
	case "":
		return errorTypeOther
	default:
		return p
	}
}

// finishReason maps a stop reason onto the semantic-convention values.
func finishReason(r llm.StopReason) string {
	switch r {
	case llm.StopEndTurn:
		return "stop"
	case llm.StopToolUse:
		return "tool_calls"
	case llm.StopMaxTokens:
		return "length"
	case llm.StopRefusal:
		return "content_filter"
	case "":
		return "unknown"
	}
	return string(r)
}

type providerConfig struct {
	classify func(error) string
	cost     func(model string, u llm.Usage) (float64, bool)
}

// ProviderOption configures WrapProvider.
type ProviderOption func(*providerConfig)

// WithErrorClassifier sets how errors become the error.type attribute (for
// example router.Classify(err).Kind.String()). The default reports the Go
// type of the error, or "timeout" and "cancelled" for context errors.
func WithErrorClassifier(f func(error) string) ProviderOption {
	return func(c *providerConfig) {
		if f != nil {
			c.classify = f
		}
	}
}

// WithCostFunc overrides the price table used for gta.cost.
func WithCostFunc(f func(model string, u llm.Usage) (float64, bool)) ProviderOption {
	return func(c *providerConfig) {
		if f != nil {
			c.cost = f
		}
	}
}

func defaultClassify(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return errorTypeName(err)
}

// WrapProvider returns p instrumented so every model call becomes a client
// span named "chat {model}" with GenAI semantic-convention attributes, and
// records token, cost, latency and time-to-first-token metrics. With a nil
// or disabled Telemetry it returns p unchanged.
func (t *Telemetry) WrapProvider(p llm.Provider, opts ...ProviderOption) llm.Provider {
	if t == nil || !t.enabled || p == nil {
		return p
	}
	cfg := providerConfig{classify: defaultClassify, cost: pricing.Cost}
	for _, o := range opts {
		o(&cfg)
	}
	return &instrumentedProvider{inner: p, t: t, cfg: cfg}
}

type instrumentedProvider struct {
	inner llm.Provider
	t     *Telemetry
	cfg   providerConfig
}

// Name implements llm.Provider; it is the wrapped provider's name.
func (p *instrumentedProvider) Name() string { return p.inner.Name() }

// Unwrap returns the wrapped provider.
func (p *instrumentedProvider) Unwrap() llm.Provider { return p.inner }

// Stream implements llm.Provider.
func (p *instrumentedProvider) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	system := GenAISystem(p.inner.Name())
	attrs := []KeyValue{
		String(AttrGenAIOperationName, OperationChat),
		String(AttrGenAISystem, system),
		String(AttrGenAIProviderName, system),
		String(AttrGenAIRequestModel, req.Model),
		Int(AttrGTARequestMessages, len(req.Messages)),
		Int(AttrGTARequestTools, len(req.Tools)),
	}
	if req.MaxTokens > 0 {
		attrs = append(attrs, Int64(AttrGenAIRequestMaxTokens, req.MaxTokens))
	}
	if req.Effort != "" {
		attrs = append(attrs, String(AttrGTARequestEffort, req.Effort))
	}
	name := OperationChat
	if req.Model != "" {
		name += " " + req.Model
	}
	ctx, span := p.t.Tracer.Start(ctx, name, WithSpanKind(SpanKindClient), WithAttributes(attrs...))
	start := time.Now()
	var (
		mu    sync.Mutex
		first time.Time
	)
	wrapped := func(ev llm.Event) {
		switch ev.Type {
		case llm.EventTextDelta, llm.EventThinkingDelta, llm.EventToolUseStart, llm.EventServerToolStart:
			mu.Lock()
			isFirst := first.IsZero()
			if isFirst {
				first = time.Now()
			}
			mu.Unlock()
			if isFirst {
				span.AddEvent("gen_ai.first_token", nil)
			}
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}
	resp, err := p.inner.Stream(ctx, req, wrapped)
	end := time.Now()
	mu.Lock()
	firstAt := first
	mu.Unlock()

	in := p.t.instruments()
	mattrs := []KeyValue{
		String(AttrGenAIOperationName, OperationChat),
		String(AttrGenAISystem, system),
		String(AttrGenAIRequestModel, req.Model),
	}
	if err != nil {
		kind := orOther(p.cfg.classify(err))
		span.RecordError(err)
		span.SetAttributes(String(AttrErrorType, kind))
		span.SetStatus(StatusError, err.Error())
		in.OperationDuration.Record(ctx, end.Sub(start).Seconds(), append(mattrs, String(AttrErrorType, kind))...)
		in.RecordError(ctx, "model", kind)
		span.End(WithEndTime(end))
		return resp, err
	}
	model := resp.Model
	if model == "" {
		model = req.Model
	}
	u := resp.Usage
	input := u.ContextTokens()
	span.SetAttributes(
		String(AttrGenAIResponseModel, model),
		StringSlice(AttrGenAIResponseFinish, []string{finishReason(resp.StopReason)}),
		Int64(AttrGenAIUsageInputTokens, input),
		Int64(AttrGenAIUsageOutputTokens, u.OutputTokens),
		Int64(AttrGenAIUsageCacheRead, u.CacheReadTokens),
		Int64(AttrGenAIUsageCacheCreation, u.CacheWriteTokens),
	)
	mattrs = append(mattrs, String(AttrGenAIResponseModel, model))
	in.OperationDuration.Record(ctx, end.Sub(start).Seconds(), mattrs...)
	in.TokenUsage.Record(ctx, input, append(mattrs, String(AttrGenAITokenType, "input"))...)
	in.TokenUsage.Record(ctx, u.OutputTokens, append(mattrs, String(AttrGenAITokenType, "output"))...)
	for _, tc := range []struct {
		typ string
		n   int64
	}{{"input", u.InputTokens}, {"output", u.OutputTokens}, {"cache_read", u.CacheReadTokens}, {"cache_write", u.CacheWriteTokens}} {
		if tc.n > 0 {
			in.Tokens.Add(ctx, tc.n, String(AttrGenAIResponseModel, model), String(AttrGenAITokenType, tc.typ))
		}
	}
	if c, ok := p.cfg.cost(model, u); ok {
		span.SetAttributes(Float64(AttrGTACostUSD, c))
		in.Cost.Add(ctx, c, String(AttrGenAIResponseModel, model))
	}
	if !firstAt.IsZero() {
		in.TimeToFirstToken.Record(ctx, firstAt.Sub(start).Seconds(), mattrs...)
		if u.OutputTokens > 1 {
			in.TimePerOutputToken.Record(ctx, end.Sub(firstAt).Seconds()/float64(u.OutputTokens-1), mattrs...)
		}
	}
	span.End(WithEndTime(end))
	return resp, nil
}

// Outcome is how a tool call ended.
type Outcome string

// Tool call outcomes.
const (
	OutcomeSuccess   Outcome = "success"
	OutcomeError     Outcome = "error"
	OutcomeDenied    Outcome = "denied"
	OutcomeCancelled Outcome = "cancelled"
	OutcomeTimeout   Outcome = "timeout"
)

// ToolSpan tracks one tool call. End must be called exactly once; later
// calls are ignored.
type ToolSpan struct {
	t     *Telemetry
	ctx   context.Context
	span  *Span
	name  string
	start time.Time
	once  sync.Once
}

// StartTool begins an "execute_tool {name}" span for a tool call. It is
// safe on a nil Telemetry.
func (t *Telemetry) StartTool(ctx context.Context, name, callID string, attrs ...KeyValue) (context.Context, *ToolSpan) {
	ts := &ToolSpan{t: t, ctx: ctx, name: name, start: time.Now()}
	if t == nil || !t.enabled {
		ts.span = &Span{}
		return ctx, ts
	}
	all := append([]KeyValue{
		String(AttrGenAIOperationName, OperationExecuteTool),
		String(AttrGenAIToolName, name),
		String(AttrGenAIToolType, "function"),
	}, attrs...)
	if callID != "" {
		all = append(all, String(AttrGenAIToolCallID, callID))
	}
	ctx, ts.span = t.Tracer.Start(ctx, OperationExecuteTool+" "+name, WithSpanKind(SpanKindInternal), WithAttributes(all...))
	ts.ctx = ctx
	return ctx, ts
}

// Span returns the underlying span, for extra attributes or events.
func (ts *ToolSpan) Span() *Span { return ts.span }

// End finishes the tool span and records gta.tool.calls and
// gta.tool.duration. A non-nil err with OutcomeSuccess is reported as
// OutcomeError.
func (ts *ToolSpan) End(outcome Outcome, err error) {
	if ts == nil {
		return
	}
	ts.once.Do(func() {
		if err != nil && outcome == OutcomeSuccess {
			outcome = OutcomeError
		}
		if outcome == "" {
			outcome = OutcomeSuccess
		}
		ts.span.SetAttributes(String(AttrGTAToolOutcome, string(outcome)))
		kind := string(outcome)
		if outcome == OutcomeError && err != nil {
			kind = defaultClassify(err)
		}
		if outcome != OutcomeSuccess {
			desc := string(outcome)
			if err != nil {
				ts.span.RecordError(err)
				desc = err.Error()
			}
			ts.span.SetAttributes(String(AttrErrorType, kind))
			ts.span.SetStatus(StatusError, desc)
		}
		ts.span.End()
		if ts.t == nil || !ts.t.enabled {
			return
		}
		in := ts.t.instruments()
		attrs := []KeyValue{String(AttrGenAIToolName, ts.name), String(AttrGTAToolOutcome, string(outcome))}
		in.ToolCalls.Add(ts.ctx, 1, attrs...)
		in.ToolDuration.Record(ts.ctx, time.Since(ts.start).Seconds(), attrs...)
		if outcome == OutcomeError {
			in.RecordError(ts.ctx, "tool", kind)
		}
	})
}

// StartAgent begins an "invoke_agent {name}" span grouping one agent turn's
// model and tool spans. It is safe on a nil Telemetry, returning a
// non-recording span.
func (t *Telemetry) StartAgent(ctx context.Context, name, conversationID string, attrs ...KeyValue) (context.Context, *Span) {
	if t == nil || !t.enabled {
		return ctx, &Span{}
	}
	all := append([]KeyValue{
		String(AttrGenAIOperationName, OperationInvokeAgent),
		String(AttrGenAIAgentName, name),
	}, attrs...)
	if conversationID != "" {
		all = append(all, String(AttrGenAIConversationID, conversationID))
	}
	return t.Tracer.Start(ctx, OperationInvokeAgent+" "+name, WithSpanKind(SpanKindInternal), WithAttributes(all...))
}
