package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/policy"
)

// An image path is a file read, so the managed policy's path rules apply.
func TestImagePathHonoursPolicy(t *testing.T) {
	p, err := policy.Parse([]byte(`{"version":1,"paths":{"deny_read":["secret.png"]}}`), "test.json")
	if err != nil {
		t.Fatal(err)
	}
	enf, err := policy.NewEnforcer(p)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) {
		inner := o.Factory
		o.Factory = func(ctx context.Context, so SessionOptions) (*agent.Agent, error) {
			ag, err := inner(ctx, so)
			if ag != nil {
				ag.Gov = &agent.Governance{Policy: enf}
			}
			return ag, err
		}
	})
	raw := pngData(t)
	for _, name := range []string{"secret.png", "ok.png"} {
		if err := os.WriteFile(filepath.Join(h.workspace, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id := h.create(nil)
	resp, body := h.do("POST", "/v1/sessions/"+id+"/messages", map[string]any{"text": "look", "images": []map[string]any{{"path": "secret.png"}}})
	e, _ := body["error"].(map[string]any)
	if resp.StatusCode != 400 || !strings.Contains(e["message"].(string), "denied by policy") {
		t.Fatalf("denied image: %d %v", resp.StatusCode, body)
	}
	h.script(textStep("I see it"))
	_, stream := h.send(id, map[string]any{"text": "look", "images": []map[string]any{{"path": "ok.png"}}})
	stream.until(t, EventDone)
}
