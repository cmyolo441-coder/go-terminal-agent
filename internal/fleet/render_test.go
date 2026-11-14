package fleet

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRenderStatus(t *testing.T) {
	clk := NewFakeClock(t0)
	c := openTest(t, testConfig("", clk))
	mustRegister(t, c, "worker-a", Capabilities{OS: "linux", Arch: "amd64", MaxConcurrency: 4})
	for range 5 {
		mustSubmit(t, c, TaskSpec{Tenant: "acme", Priority: PriorityHigh})
	}
	mustSubmit(t, c, TaskSpec{Tenant: "globex"})
	a := leaseOne(t, c, "worker-a")
	clk.Advance(1500 * time.Millisecond)
	mustComplete(t, c, "worker-a", a)
	clk.Advance(2 * time.Second)

	var b strings.Builder
	if err := RenderStatus(&b, c.Metrics(context.Background()), RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"leader", "acme", "globex", "high", "worker-a", "linux/amd64", "succeeded 1", "run time", "1.50s"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("colors without Color")
	}
	b.Reset()
	RenderStatus(&b, c.Metrics(context.Background()), RenderOptions{Color: true})
	if !strings.Contains(b.String(), "\x1b[32m") {
		t.Error("no colors with Color")
	}
	b.Reset()
	f, _ := New(testConfig("", clk))
	RenderStatus(&b, f.Metrics(context.Background()), RenderOptions{})
	if !strings.Contains(b.String(), "follower") {
		t.Errorf("follower output: %s", b.String())
	}
}
