// Package pricing estimates request cost from token usage.
package pricing

import (
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Rate is USD per million tokens.
type Rate struct {
	Input, Output, CacheRead, CacheWrite float64
}

// Rates are first-party Claude API list prices (USD / 1M tokens). Cache
// writes use the 5-minute rate of 1.25x input.
var Rates = map[string]Rate{
	"claude-fable-5-2":  {8, 40, 0.20, 10},
	"claude-fable-5-1":  {10, 50, 1.00, 12.5},
	"claude-opus-5-5":   {4, 20, 0.20, 5},
	"claude-opus-5":     {5, 25, 0.50, 6.25},
	"claude-opus-4-8":   {5, 25, 0.50, 6.25},
	"claude-sonnet-5-5": {2, 10, 0.10, 2.5},
	"claude-sonnet-5":   {2, 10, 0.20, 2.5},
	"claude-haiku-5-5":  {0.10, 0.50, 0.01, 0.125},
	"claude-haiku-4-5":  {1, 5, 0.10, 1.25},
}

// Lookup finds the rate for a model, matching prefixes for dated or suffixed ids.
func Lookup(model string) (Rate, bool) {
	if r, ok := Rates[model]; ok {
		return r, true
	}
	best, bestLen := Rate{}, 0
	for id, r := range Rates {
		if strings.HasPrefix(model, id) && len(id) > bestLen {
			best, bestLen = r, len(id)
		}
	}
	return best, bestLen > 0
}

// Cost estimates the USD cost of usage on a model. ok is false for unknown models.
func Cost(model string, u llm.Usage) (usd float64, ok bool) {
	r, ok := Lookup(model)
	if !ok {
		return 0, false
	}
	const m = 1_000_000
	return float64(u.InputTokens)*r.Input/m + float64(u.OutputTokens)*r.Output/m +
		float64(u.CacheReadTokens)*r.CacheRead/m + float64(u.CacheWriteTokens)*r.CacheWrite/m, true
}
