package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

// DefaultModel is used when nothing else is configured.
const DefaultModel = "claude-opus-5-5"

// Aliases map short names to model ids.
var Aliases = map[string]string{
	"opus":   "claude-opus-5-5",
	"sonnet": "claude-sonnet-5-5",
	"haiku":  "claude-haiku-5-5",
	"fable":  "claude-fable-5-2",
}

// Efforts are the accepted effort levels.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// ResolveModel expands an alias.
func ResolveModel(name string) string {
	if id, ok := Aliases[strings.ToLower(strings.TrimSpace(name))]; ok {
		return id
	}
	return strings.TrimSpace(name)
}

type caps struct {
	adaptive  bool // adaptive thinking and the effort parameter
	fallback  bool // server-side refusal fallbacks
	webSearch bool // web_search_20260209
	context   int64
}

func capsFor(model string) caps {
	m := strings.ToLower(model)
	c := caps{adaptive: true, webSearch: true, context: 1_000_000}
	// Generations before 4.6 use a different thinking API and smaller windows;
	// run them without thinking rather than guessing a budget.
	for _, old := range []string{"haiku-4-5", "sonnet-4-5", "opus-4-5", "opus-4-1", "claude-3", "sonnet-4-0", "opus-4-0", "sonnet-4-2", "opus-4-2"} {
		if strings.Contains(m, old) {
			c.adaptive, c.webSearch, c.context = false, false, 200_000
		}
	}
	for _, fb := range []string{"fable-5-2", "fable-5-1", "opus-5-5", "opus-5", "sonnet-5-5"} {
		if strings.Contains(m, fb) {
			c.fallback = true
		}
	}
	if v, err := strconv.ParseInt(os.Getenv("GOCODE_CONTEXT_WINDOW"), 10, 64); err == nil && v > 0 {
		c.context = v
	}
	return c
}

// ContextWindow returns the model's context size in tokens.
func ContextWindow(model string) int64 { return capsFor(model).context }

// compactThreshold is the context size at which the history is summarized.
func compactThreshold(model string) int64 { return capsFor(model).context * 85 / 100 }

type price struct{ in, out, cacheRead float64 } // USD per million tokens

// Checked in order; the first substring match wins.
var prices = []struct {
	match string
	price
}{
	{"fable-5-2", price{8, 40, 0.20}}, {"mythos-5-2", price{8, 40, 0.20}},
	{"fable", price{10, 50, 1.0}}, {"mythos", price{10, 50, 1.0}},
	{"opus-5-5", price{4, 20, 0.20}},
	{"opus", price{5, 25, 0.50}},
	{"sonnet-5-5", price{2, 10, 0.10}},
	{"sonnet-5", price{2, 10, 0.20}},
	{"sonnet", price{3, 15, 0.30}},
	{"haiku-5-5", price{0.10, 0.50, 0.01}},
	{"haiku", price{1, 5, 0.10}},
}

// Usage accumulates token counts and estimated cost across the session,
// including sub-agents.
type Usage struct {
	mu                                    sync.Mutex
	Input, Output, CacheRead, CacheCreate int64
	Requests                              int
	Cost                                  float64
	priced                                bool
}

// Add records one response's usage.
func (u *Usage) Add(model string, x anthropic.BetaUsage) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.Requests++
	u.Input += x.InputTokens
	u.Output += x.OutputTokens
	u.CacheRead += x.CacheReadInputTokens
	u.CacheCreate += x.CacheCreationInputTokens
	m := strings.ToLower(model)
	for _, p := range prices {
		if strings.Contains(m, p.match) {
			u.priced = true
			u.Cost += (float64(x.InputTokens)*p.in + float64(x.OutputTokens)*p.out +
				float64(x.CacheReadInputTokens)*p.cacheRead + float64(x.CacheCreationInputTokens)*p.in*1.25) / 1e6
			break
		}
	}
}

// Snapshot returns a copy of the counters.
func (u *Usage) Snapshot() (in, out, cacheRead, cacheCreate int64, requests int, cost float64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.Input, u.Output, u.CacheRead, u.CacheCreate, u.Requests, u.Cost
}

// CostString formats the estimated cost.
func (u *Usage) CostString() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.priced {
		return "n/a"
	}
	return fmt.Sprintf("$%.4f", u.Cost)
}
