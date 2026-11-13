package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// newTestTelemetry builds an enabled pipeline that exports spans
// synchronously to memory and keeps metrics for manual collection.
func newTestTelemetry(t *testing.T) (*Telemetry, *InMemoryExporter) {
	t.Helper()
	exp := &InMemoryExporter{}
	tp := NewTracerProvider(WithSpanProcessor(NewSimpleSpanProcessor(exp)))
	mp := NewMeterProvider()
	in, err := NewInstruments(mp.Meter(instrumentationName))
	if err != nil {
		t.Fatal(err)
	}
	return &Telemetry{
		TracerProvider: tp, MeterProvider: mp, Tracer: tp.Tracer(instrumentationName),
		Instruments: in, enabled: true, description: "test",
	}, exp
}

type scriptedProvider struct {
	name   string
	events []llm.Event
	resp   *llm.Response
	err    error
	gotCtx context.Context
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	p.gotCtx = ctx
	for _, ev := range p.events {
		onEvent(ev)
	}
	return p.resp, p.err
}

func pointValue(t *testing.T, m Metric, want map[string]string) float64 {
	t.Helper()
	match := func(attrs []KeyValue) bool {
		for k, v := range want {
			found := false
			for _, kv := range attrs {
				if kv.Key == k && kv.Value.Emit() == v {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	switch d := m.Data.(type) {
	case Sum:
		for _, p := range d.DataPoints {
			if match(p.Attributes) {
				return p.Value()
			}
		}
	case Histogram:
		for _, p := range d.DataPoints {
			if match(p.Attributes) {
				return p.Sum
			}
		}
	}
	t.Fatalf("no %s point with %v", m.Name, want)
	return 0
}

func TestWrapProviderSuccess(t *testing.T) {
	tel, exp := newTestTelemetry(t)
	inner := &scriptedProvider{
		name:   "gemini",
		events: []llm.Event{{Type: llm.EventThinkingDelta, Text: "hm"}, {Type: llm.EventTextDelta, Text: "hi"}},
		resp: &llm.Response{
			StopReason: llm.StopToolUse, Model: "gemini-3-pro",
			Usage: llm.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 50, CacheWriteTokens: 5},
		},
	}
	p := tel.WrapProvider(inner, WithCostFunc(func(model string, u llm.Usage) (float64, bool) {
		return 0.125, model == "gemini-3-pro"
	}))
	if p.Name() != "gemini" || p.(interface{ Unwrap() llm.Provider }).Unwrap() != inner {
		t.Fatal("wrapper hides the provider")
	}
	ctx, agentSpan := tel.StartAgent(context.Background(), "gta", "conv-1")
	var seen int
	resp, err := p.Stream(ctx, llm.Request{Model: "gemini-3-pro", MaxTokens: 4096, Effort: "high", Messages: []llm.Message{llm.UserText("q")}},
		func(llm.Event) { seen++ })
	agentSpan.End()
	if err != nil || resp != inner.resp || seen != 2 {
		t.Fatalf("resp=%v err=%v seen=%d", resp, err, seen)
	}
	if !SpanContextFromContext(inner.gotCtx).IsValid() {
		t.Fatal("inner provider did not receive the span context")
	}
	spans := exp.Spans()
	if len(spans) != 2 {
		t.Fatalf("%d spans", len(spans))
	}
	chat, agent := spans[0], spans[1]
	if chat.Name != "chat gemini-3-pro" || chat.Kind != SpanKindClient || chat.Parent.SpanID != agent.SpanContext.SpanID {
		t.Fatalf("chat span %q kind %d parent %v", chat.Name, chat.Kind, chat.Parent.SpanID)
	}
	if agent.Name != "invoke_agent gta" {
		t.Fatalf("agent span %q", agent.Name)
	}
	want := map[string]any{
		AttrGenAIOperationName:      "chat",
		AttrGenAISystem:             "gcp.gemini",
		AttrGenAIProviderName:       "gcp.gemini",
		AttrGenAIRequestModel:       "gemini-3-pro",
		AttrGenAIRequestMaxTokens:   int64(4096),
		AttrGTARequestEffort:        "high",
		AttrGenAIResponseModel:      "gemini-3-pro",
		AttrGenAIUsageInputTokens:   int64(155),
		AttrGenAIUsageOutputTokens:  int64(20),
		AttrGenAIUsageCacheRead:     int64(50),
		AttrGenAIUsageCacheCreation: int64(5),
		AttrGTACostUSD:              0.125,
	}
	for k, v := range want {
		got, ok := chat.Attr(k)
		if !ok || got.AsInterface() != v {
			t.Fatalf("%s = %v, want %v", k, got.AsInterface(), v)
		}
	}
	if fr, _ := chat.Attr(AttrGenAIResponseFinish); fr.Emit() != "[tool_calls]" {
		t.Fatalf("finish reasons %v", fr.Emit())
	}
	if len(chat.Events) != 1 || chat.Events[0].Name != "gen_ai.first_token" || chat.Status.Code != StatusUnset {
		t.Fatalf("events %+v status %+v", chat.Events, chat.Status)
	}

	rm := tel.MeterProvider.Collect(context.Background())
	usage := findMetric(t, rm, "gen_ai.client.token.usage")
	if pointValue(t, usage, map[string]string{AttrGenAITokenType: "input"}) != 155 ||
		pointValue(t, usage, map[string]string{AttrGenAITokenType: "output"}) != 20 {
		t.Fatal("token usage histogram")
	}
	tokens := findMetric(t, rm, "gta.tokens")
	if pointValue(t, tokens, map[string]string{AttrGenAITokenType: "cache_read"}) != 50 ||
		pointValue(t, tokens, map[string]string{AttrGenAITokenType: "input"}) != 100 {
		t.Fatal("gta.tokens")
	}
	if pointValue(t, findMetric(t, rm, "gta.cost"), map[string]string{AttrGenAIResponseModel: "gemini-3-pro"}) != 0.125 {
		t.Fatal("gta.cost")
	}
	if findMetric(t, rm, "gen_ai.server.time_to_first_token").Data.(Histogram).DataPoints[0].Count != 1 ||
		findMetric(t, rm, "gen_ai.server.time_per_output_token").Data.(Histogram).DataPoints[0].Count != 1 ||
		findMetric(t, rm, "gen_ai.client.operation.duration").Data.(Histogram).DataPoints[0].Count != 1 {
		t.Fatal("latency histograms")
	}
}

func TestWrapProviderError(t *testing.T) {
	tel, exp := newTestTelemetry(t)
	boom := errors.New("server error (503)")
	p := tel.WrapProvider(&scriptedProvider{name: "nvidia", err: boom},
		WithErrorClassifier(func(error) string { return "overloaded" }))
	if _, err := p.Stream(context.Background(), llm.Request{Model: "kimi-k3.1-pro"}, nil); err != boom {
		t.Fatalf("error not passed through: %v", err)
	}
	s := exp.Spans()[0]
	if s.Status.Code != StatusError || s.Status.Description != boom.Error() {
		t.Fatalf("status %+v", s.Status)
	}
	if et, _ := s.Attr(AttrErrorType); et.AsString() != "overloaded" {
		t.Fatalf("error.type %q", et.AsString())
	}
	if len(s.Events) != 1 || s.Events[0].Name != "exception" {
		t.Fatalf("events %+v", s.Events)
	}
	rm := tel.MeterProvider.Collect(context.Background())
	if pointValue(t, findMetric(t, rm, "gta.errors"), map[string]string{AttrGTAComponent: "model", AttrErrorType: "overloaded"}) != 1 {
		t.Fatal("gta.errors")
	}
	d := findMetric(t, rm, "gen_ai.client.operation.duration").Data.(Histogram).DataPoints[0]
	found := false
	for _, kv := range d.Attributes {
		found = found || (kv.Key == AttrErrorType && kv.Value.AsString() == "overloaded")
	}
	if !found {
		t.Fatal("duration not tagged with error.type")
	}
	// Default classifier names context errors.
	p2 := tel.WrapProvider(&scriptedProvider{name: "x", err: context.Canceled})
	_, _ = p2.Stream(context.Background(), llm.Request{}, nil)
	last := exp.Spans()[1]
	if et, _ := last.Attr(AttrErrorType); et.AsString() != "cancelled" || last.Name != "chat" {
		t.Fatalf("default classifier: %q name %q", et.AsString(), last.Name)
	}
}

func TestDisabledTelemetryIsTransparent(t *testing.T) {
	inner := &scriptedProvider{name: "a", resp: &llm.Response{}}
	var nilTel *Telemetry
	if nilTel.WrapProvider(inner) != inner || Disabled().WrapProvider(inner) != inner {
		t.Fatal("disabled telemetry wrapped the provider")
	}
	ctx, ts := nilTel.StartTool(context.Background(), "bash", "id")
	ts.End(OutcomeError, errors.New("x"))
	if ctx == nil {
		t.Fatal("nil context")
	}
	_, s := Disabled().StartAgent(context.Background(), "gta", "")
	s.End()
	if nilTel.Enabled() || nilTel.Description() != "off" || nilTel.Shutdown(context.Background()) != nil || nilTel.ForceFlush(context.Background()) != nil {
		t.Fatal("nil telemetry helpers")
	}
	var in *Instruments
	in.RecordError(context.Background(), "a", "b")
	in.RecordRetry(context.Background(), "a", "b")
	in.RecordFallback(context.Background(), "a", "b", "c")
}

func TestToolSpans(t *testing.T) {
	tel, exp := newTestTelemetry(t)
	ctx := context.Background()
	_, ok := tel.StartTool(ctx, "bash", "call_1", String("gta.tool.command", "ls"))
	ok.End(OutcomeSuccess, nil)
	ok.End(OutcomeError, errors.New("ignored second End"))
	_, failed := tel.StartTool(ctx, "edit", "call_2")
	failed.End(OutcomeSuccess, errors.New("no match"))
	_, denied := tel.StartTool(ctx, "bash", "call_3")
	denied.End(OutcomeDenied, nil)
	_, timeout := tel.StartTool(ctx, "bash", "call_4")
	timeout.End(OutcomeError, context.DeadlineExceeded)

	spans := exp.Spans()
	if len(spans) != 4 {
		t.Fatalf("%d spans", len(spans))
	}
	if spans[0].Name != "execute_tool bash" || spans[0].Status.Code != StatusUnset {
		t.Fatalf("success span %+v", spans[0])
	}
	if id, _ := spans[0].Attr(AttrGenAIToolCallID); id.AsString() != "call_1" {
		t.Fatal("call id")
	}
	if o, _ := spans[1].Attr(AttrGTAToolOutcome); o.AsString() != "error" || spans[1].Status.Description != "no match" {
		t.Fatalf("error with success outcome not upgraded: %+v", spans[1])
	}
	if et, _ := spans[2].Attr(AttrErrorType); et.AsString() != "denied" || spans[2].Status.Code != StatusError {
		t.Fatalf("denied span %+v", spans[2])
	}
	if et, _ := spans[3].Attr(AttrErrorType); et.AsString() != "timeout" {
		t.Fatalf("timeout error.type %q", et.AsString())
	}
	rm := tel.MeterProvider.Collect(ctx)
	calls := findMetric(t, rm, "gta.tool.calls")
	if pointValue(t, calls, map[string]string{AttrGenAIToolName: "bash", AttrGTAToolOutcome: "success"}) != 1 ||
		pointValue(t, calls, map[string]string{AttrGenAIToolName: "bash", AttrGTAToolOutcome: "denied"}) != 1 ||
		pointValue(t, calls, map[string]string{AttrGenAIToolName: "edit", AttrGTAToolOutcome: "error"}) != 1 {
		t.Fatal("gta.tool.calls")
	}
	if pointValue(t, findMetric(t, rm, "gta.errors"), map[string]string{AttrGTAComponent: "tool", AttrErrorType: "timeout"}) != 1 {
		t.Fatal("tool error counted without its kind")
	}
	if findMetric(t, rm, "gta.tool.duration").Data.(Histogram).DataPoints[0].Count == 0 {
		t.Fatal("tool duration")
	}
}

func TestRetryAndFallbackCounters(t *testing.T) {
	tel, _ := newTestTelemetry(t)
	ctx := context.Background()
	tel.Instruments.RecordRetry(ctx, "anthropic", "overloaded")
	tel.Instruments.RecordRetry(ctx, "anthropic", "overloaded")
	tel.Instruments.RecordFallback(ctx, "anthropic", "nvidia/kimi-k3.1-pro", "")
	rm := tel.MeterProvider.Collect(ctx)
	if pointValue(t, findMetric(t, rm, "gta.model.retries"), map[string]string{AttrGTARoute: "anthropic"}) != 2 {
		t.Fatal("retries")
	}
	if pointValue(t, findMetric(t, rm, "gta.model.fallbacks"), map[string]string{"gta.route.to": "nvidia/kimi-k3.1-pro", AttrGTAReason: "_OTHER"}) != 1 {
		t.Fatal("fallbacks")
	}
}

func TestGenAISystemNames(t *testing.T) {
	for in, want := range map[string]string{"anthropic": "anthropic", "OpenAI": "openai", "gemini": "gcp.gemini", "mistral": "mistral_ai", "nvidia": "nvidia", "": "_OTHER"} {
		if got := GenAISystem(in); got != want {
			t.Fatalf("GenAISystem(%q) = %q, want %q", in, got, want)
		}
	}
	for r, want := range map[llm.StopReason]string{llm.StopEndTurn: "stop", llm.StopMaxTokens: "length", llm.StopRefusal: "content_filter", llm.StopPauseTurn: "pause_turn", "": "unknown"} {
		if got := finishReason(r); got != want {
			t.Fatalf("finishReason(%q) = %q", r, got)
		}
	}
}
