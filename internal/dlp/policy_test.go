package dlp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePolicyValid(t *testing.T) {
	p, err := ParsePolicy([]byte(`{
		"version": 1, "name": "corp", "min_confidence": 0.6, "context_window": 128,
		"default_action": "redact", "mask_char": "•", "keep_last": 4, "block_score": 90,
		"block_label": "restricted",
		"detectors": {"CREDIT_CARD": {"action": "block", "label": "Restricted", "frameworks": ["PCI-DSS"]},
		              "IP_ADDRESS": {"enabled": false}},
		"ip": {"include_private": true},
		"allow_values": ["4111111111111111"], "allow_email_domains": ["corp.com"],
		"custom_regex": [{"name": "EMP_ID", "pattern": "EMP-\\d+", "confidence": 0.8}],
		"dictionaries": [{"name": "CODENAME", "terms": ["bluebird"]}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if *p.BlockLabel != Restricted || p.Detectors[CreditCard].Action != ActionBlock {
		t.Fatalf("%+v", p)
	}
	if _, err := New(p); err != nil {
		t.Fatal(err)
	}
}

func TestParsePolicyInvalid(t *testing.T) {
	cases := map[string]string{
		`{"version":2}`:                                                         "version",
		`{"version":1,"bogus":1}`:                                               "unknown field",
		`{"version":1,"name":"a","name":"b"}`:                                   "duplicate key",
		`{"version":1,"detectors":{"EMAIL":{},"EMAIL":{}}}`:                     "duplicate key",
		`{"version":1} {}`:                                                      "after the JSON",
		`{"version":1,"default_action":"nuke"}`:                                 "unknown action",
		`{"version":1,"block_label":"Secret"}`:                                  "unknown label",
		`{"version":1,"min_confidence":1.5}`:                                    "min_confidence",
		`{"version":1,"context_window":5000}`:                                   "context_window",
		`{"version":1,"mask_char":"**"}`:                                        "mask_char",
		`{"version":1,"keep_last":9}`:                                           "keep_last",
		`{"version":1,"block_score":101}`:                                       "block_score",
		`{"version":1,"detectors":{"SSN":{}}}`:                                  "unknown detector",
		`{"version":1,"detectors":{"EMAIL":{"frameworks":["bad framework"]}}}`:  "framework",
		`{"version":1,"custom_regex":[{"name":"X_Y","pattern":"("}]}`:           "missing closing",
		`{"version":1,"custom_regex":[{"name":"X_Y","pattern":"a*"}]}`:          "empty string",
		`{"version":1,"custom_regex":[{"name":"X_Y","pattern":"a","group":2}]}`: "group",
		`{"version":1,"custom_regex":[{"name":"EMAIL","pattern":"a"}]}`:         "built-in",
		`{"version":1,"custom_regex":[{"name":"lower","pattern":"a"}]}`:         "must match",
		`{"version":1,"custom_regex":[{"name":"AB","pattern":"a"}],"dictionaries":[{"name":"AB","terms":["x"]}]}`: "already used",
		`{"version":1,"dictionaries":[{"name":"AB","terms":[]}]}`:                                                 "no terms",
		`{"version":1,"dictionaries":[{"name":"AB","terms":[" "]}]}`:                                              "empty",
		`{"version":1,"allow_email_domains":["@corp.com"]}`:                                                       "not a domain",
		`{"version":1,"ip":{"disable_v4":true,"disable_v6":true}}`:                                                "both address families",
		`[1]`:  "cannot unmarshal",
		"\xff": "UTF-8",
	}
	for in, want := range cases {
		_, err := ParsePolicy([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParsePolicy(%s): err = %v, want containing %q", in, err, want)
		}
	}
	// Validation reports every problem at once.
	_, err := ParsePolicy([]byte(`{"version":3,"keep_last":-1,"block_score":-2}`))
	if err == nil || strings.Count(err.Error(), "\n") < 2 {
		t.Fatalf("want joined errors, got %v", err)
	}
	// New validates hand-built policies too.
	if _, err := New(&Policy{Version: 1, DefaultAction: "zap"}); err == nil {
		t.Fatal("New accepted an invalid policy")
	}
}

func TestLoadPolicyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dlp.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"name":"file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicyFile(path)
	if err != nil || p.Name != "file" {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := LoadPolicyFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
	os.WriteFile(path, []byte(`{"version":9}`), 0o600)
	if _, err := LoadPolicyFile(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err %v", err)
	}
}
