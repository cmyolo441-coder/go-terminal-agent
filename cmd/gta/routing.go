package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm/anthropic"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm/openai"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/router"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/telemetry"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

// routing is the model-call stack: telemetry around each provider and,
// when fallback models are configured, a router with retries, circuit
// breakers, rate limits and fallback between them.
type routing struct {
	tel    *telemetry.Telemetry
	router *router.Router
	// notify shows a message to the user; set once the UI exists.
	notify atomic.Pointer[func(string)]
}

// startTelemetry builds the telemetry pipeline from the config.
func startTelemetry(cfg config.Config) (*telemetry.Telemetry, error) {
	if !cfg.Telemetry.Enabled && os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return nil, nil
	}
	c := cfg.Telemetry
	c.Enabled = true
	tel, err := telemetry.New(c)
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}
	for _, w := range tel.Warnings() {
		fmt.Fprintln(os.Stderr, "gta: telemetry:", w)
	}
	return tel, nil
}

// shutdownTelemetry flushes and stops telemetry, waiting at most 5s.
func shutdownTelemetry(tel *telemetry.Telemetry) {
	if tel == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "gta: telemetry:", err)
	}
}

// buildRouting wraps primary with telemetry and, when fallback models are
// configured, routes between it and them. checkModel vets each fallback
// against the managed policy.
func buildRouting(cfg config.Config, primary llm.Provider, tel *telemetry.Telemetry, checkModel func(provider, model string) error) (llm.Provider, *routing, error) {
	rt := &routing{tel: tel}
	cls := telemetry.WithErrorClassifier(func(err error) string { return router.Classify(err).Kind.String() })
	if len(cfg.FallbackModels) == 0 {
		return tel.WrapProvider(primary, cls), rt, nil
	}
	routes := []router.Route{{Name: "primary " + cfg.Model, Provider: tel.WrapProvider(primary, cls), RPM: cfg.Routing.RPM, TPM: cfg.Routing.TPM}}
	shared := map[string]llm.Provider{}
	for i, f := range cfg.FallbackModels {
		prov := f.Provider
		if prov == "" {
			prov = cfg.Provider
		}
		if prov == "" {
			prov = "anthropic"
		}
		if err := checkModel(prov, f.Model); err != nil {
			return nil, nil, fmt.Errorf("fallback_models[%d]: %w", i, err)
		}
		p, ok := shared[prov]
		if !ok {
			var err error
			if p, err = fallbackProvider(cfg, prov); err != nil {
				return nil, nil, fmt.Errorf("fallback_models[%d]: %w", i, err)
			}
			shared[prov] = p
		}
		r := router.Route{Name: prov + " " + f.Model, Provider: tel.WrapProvider(p, cls), Model: f.Model, MaxTokens: f.MaxTokens, RPM: f.RPM, TPM: f.TPM}
		if prov != "anthropic" {
			r.Effort = "none"
			if r.MaxTokens == 0 {
				r.MaxTokens = nonClaudeMaxTokens
			}
		}
		routes = append(routes, r)
	}
	rc := cfg.Routing
	opts := router.Options{
		MaxRetries:        rc.MaxRetries,
		FirstTokenTimeout: time.Duration(rc.FirstTokenTimeoutSec) * time.Second,
		MaxRateWait:       time.Duration(rc.MaxRateWaitSec) * time.Second,
		RetryAfterStream:  rc.RetryAfterStream,
		Breaker:           router.BreakerOptions{FailureThreshold: rc.BreakerFailures, Cooldown: time.Duration(rc.BreakerCooldownSec) * time.Second},
		Observer:          rt.observe,
	}
	r, err := router.New(routes, opts)
	if err != nil {
		return nil, nil, err
	}
	rt.router = r
	return r, rt, nil
}

// fallbackProvider builds a provider for a fallback route, with its own
// retries off because the router retries.
func fallbackProvider(cfg config.Config, name string) (llm.Provider, error) {
	if name == "anthropic" {
		o := anthropic.Options{MaxRetries: -1}
		if isAnthropic(cfg) {
			o.BaseURL = cfg.BaseURL
		}
		return anthropic.New(o), nil
	}
	opts, err := openai.Preset(name)
	if err != nil {
		return nil, err
	}
	if name == cfg.Provider && cfg.BaseURL != "" {
		opts.BaseURL = cfg.BaseURL
	}
	opts.MaxRetries = -1
	return openai.New(opts)
}

func (rt *routing) observe(ctx context.Context, ev router.Event) {
	var in *telemetry.Instruments
	if rt.tel != nil {
		in = rt.tel.Instruments
	}
	switch ev.Kind {
	case router.EventRetry:
		in.RecordRetry(ctx, ev.Route, ev.Class.Kind.String())
		rt.say(fmt.Sprintf("%s: %s, retrying in %s", ev.Route, ev.Class.Kind, ev.Delay.Round(100*time.Millisecond)))
	case router.EventFallback:
		in.RecordFallback(ctx, ev.Route, ev.Next, ev.Class.Kind.String())
		telemetry.SpanFromContext(ctx).AddEvent("gta.fallback", []telemetry.KeyValue{telemetry.String("from", ev.Route), telemetry.String("to", ev.Next)})
		rt.say(fmt.Sprintf("%s failed (%s); falling back to %s", ev.Route, ev.Class.Kind, ev.Next))
	case router.EventBreaker:
		rt.say(fmt.Sprintf("%s circuit %s → %s", ev.Route, ev.From, ev.To))
	}
}

func (rt *routing) say(msg string) {
	if f := rt.notify.Load(); f != nil {
		(*f)("Routing: " + msg)
	}
}

// setNotify routes routing messages to fn.
func (rt *routing) setNotify(fn func(string)) { rt.notify.Store(&fn) }

// capabilities reports routing and telemetry for /status and the banner.
func (rt *routing) capabilities() []ui.Capability {
	var out []ui.Capability
	if rt.router != nil {
		routes := rt.router.Routes()
		out = append(out, ui.Capability{Group: "Model", Name: "routing", Value: fmt.Sprintf("%d routes · retries, breakers, fallback", len(routes)), State: ui.CapOn, Banner: true})
		for _, r := range routes {
			state := ui.CapOn
			if r.Breaker != router.StateClosed {
				state = ui.CapWarn
			}
			val := fmt.Sprintf("%s · %d ok / %d failed", r.Breaker, r.Successes, r.Failures)
			if r.LastError != "" {
				val += " · " + oneLine(r.LastError, 50)
			}
			out = append(out, ui.Capability{Group: "Model", Name: "  " + r.Name, Value: val, State: state})
		}
	} else {
		out = append(out, ui.Capability{Group: "Model", Name: "routing", Value: "single model (add \"fallback_models\" for failover)", State: ui.CapOff})
	}
	if rt.tel != nil {
		out = append(out, ui.Capability{Group: "Integrations", Name: "telemetry", Value: strings.TrimSpace(rt.tel.Description()), State: ui.CapOn, Banner: true})
	} else {
		out = append(out, ui.Capability{Group: "Integrations", Name: "telemetry", Value: "off (\"telemetry\": {\"enabled\": true} exports OTLP)", State: ui.CapOff})
	}
	return out
}
