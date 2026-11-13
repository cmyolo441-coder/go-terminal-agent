package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMergesFilesAndEnv(t *testing.T) {
	userDir := t.TempDir()
	root := t.TempDir()
	t.Setenv("GTA_CONFIG_DIR", userDir)
	t.Setenv("GTA_MODEL", "")
	t.Setenv("GTA_EFFORT", "")
	os.WriteFile(filepath.Join(userDir, "config.json"), []byte(`{"effort":"max","web_search":true}`), 0o600)
	os.MkdirAll(filepath.Join(root, ".gta"), 0o755)
	os.WriteFile(filepath.Join(root, ".gta", "config.json"), []byte(`{"permission_mode":"auto-edit"}`), 0o600)

	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Effort != "max" || !cfg.WebSearch || cfg.PermissionMode != "auto-edit" || cfg.Model != DefaultModel {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	t.Setenv("GTA_MODEL", "claude-sonnet-5-5")
	cfg, _ = Load(root)
	if cfg.Model != "claude-sonnet-5-5" {
		t.Fatalf("env override failed: %s", cfg.Model)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("GTA_CONFIG_DIR", userDir)
	os.WriteFile(filepath.Join(userDir, "config.json"), []byte(`{"effort":"extreme"}`), 0o600)
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("expected validation error")
	}
	os.WriteFile(filepath.Join(userDir, "config.json"), []byte(`{"unknown_key":1}`), 0o600)
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("expected unknown key error")
	}
}

func TestProjectHooksNeedUserTrust(t *testing.T) {
	user := t.TempDir()
	root := t.TempDir()
	t.Setenv("GTA_CONFIG_DIR", user)
	os.MkdirAll(filepath.Join(root, ".gta"), 0o755)
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, ".gta", "config.json"), `{"trust_project_hooks": true, "hooks": {"stop": [{"command": "evil"}]}}`)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hooks.Count() != 0 || cfg.TrustProjectHooks || !cfg.IgnoredProjectHooks {
		t.Fatalf("untrusted project hooks were loaded: %+v", cfg.Hooks)
	}
	write(filepath.Join(user, "config.json"), `{"trust_project_hooks": true, "hooks": {"stop": [{"command": "mine"}]}}`)
	cfg, err = Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hooks.Count() != 2 || cfg.IgnoredProjectHooks {
		t.Fatalf("trusted hooks = %+v", cfg.Hooks)
	}
}

func TestProviderValidation(t *testing.T) {
	c := Default()
	c.Provider = "nvidia"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Provider = "nope"
	if err := c.Validate(); err == nil {
		t.Fatal("unknown provider accepted")
	}
}
