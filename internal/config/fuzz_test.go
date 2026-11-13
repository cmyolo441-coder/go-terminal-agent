package config

import (
	"encoding/json"
	"testing"
)

// FuzzDecodeConfig checks that any config file content decodes and
// validates without panicking, and that a valid config survives the save
// and load round trip SaveUser and Load perform.
func FuzzDecodeConfig(f *testing.F) {
	for _, s := range []string{
		`{}`, `{"model":"m","effort":"max","max_tokens":2048}`, `{"permission_mode":"yolo","sandbox":"read-only"}`,
		`{"hooks":{"stop":[{"command":"x"}]},"mcp_servers":{"s":{"command":"c"}}}`,
		`{"fallback_models":[{"provider":"openai","model":"gpt"}],"routing":{"max_retries":3}}`,
		`{"telemetry":{"enabled":true,"exporter":"file","file":"/tmp/x","sample_ratio":0.5}}`,
		`{"budget":{"session_usd":1,"warn_at":0.8}}`, `{"lsp_servers":[{"name":"gopls","command":"gopls","extensions":[".go"]}]}`,
		`{"unknown":1}`, `{} {}`, `[]`, `null`, ``, `{"max_tokens":1e99}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg := Default()
		if err := decodeInto(&cfg, data); err != nil {
			return
		}
		if cfg.Validate() != nil {
			return
		}
		out, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("valid config does not encode: %v", err)
		}
		again := Default()
		if err := decodeInto(&again, out); err != nil {
			t.Fatalf("saved config does not load: %v\n%s", err, out)
		}
		if err := again.Validate(); err != nil {
			t.Fatalf("saved config does not validate: %v\n%s", err, out)
		}
	})
}
