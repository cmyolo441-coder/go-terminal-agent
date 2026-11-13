package pricing

import (
	"math"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

func TestCost(t *testing.T) {
	c, ok := Cost("claude-opus-5-5", llm.Usage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 1_000_000})
	if !ok || math.Abs(c-(4+2+0.2)) > 1e-9 {
		t.Fatalf("got %v %v", c, ok)
	}
	if _, ok := Cost("some-other-model", llm.Usage{}); ok {
		t.Fatal("unknown model should not price")
	}
	if _, ok := Lookup("claude-opus-5-5-fast"); !ok {
		t.Fatal("prefix lookup failed")
	}
}
