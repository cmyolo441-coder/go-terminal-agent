package plugin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseManifestErrors(t *testing.T) {
	cases := map[string][]string{
		`{"name": "x", "version": "1.0.0", "description": "d", "tools": [], "bogus": 1}`: {`unknown field "bogus"`},
		"{\n  \"name\": \"x\",\n  \"version\": 1\n}":                                     {"line 3", "cannot unmarshal number"},
		"{\n  \"name\": \"x\",,\n}":                                                      {"invalid JSON at line 2"},
		`{} {}`:                                                                          {"unexpected data"},
		`{}`:                                                                             {`"name" is required`, `"version" is required`, `"description" is required`, `no "tools" and no "commands"`},
		`{"name": "Bad Name", "version": "1.0", "description": "d", "min_gta_version": "x", "commands": [{"name": "c", "description": "d", "prompt": "p"}]}`: {
			`"name" "Bad Name" must be`, `"version": "1.0" is not a semantic version`, `"min_gta_version"`},
		`{"name": "mcp", "version": "1.0.0", "description": "d", "commands": [{"name": "c", "description": "d", "prompt": "p"}]}`: {`"mcp" is reserved`},
		`{"name": "p", "version": "1.0.0", "description": "d", "env": {"GTA_X": "1", "1BAD": "2"}, "pass_env": ["A-B"], "tools": [
			{"name": "t", "description": "", "kind": "root", "command": "", "timeout_sec": 9999, "max_output_bytes": -1, "input_schema": {"type": "array"}},
			{"name": "T", "description": "dup", "command": "./x"},
			{"name": "bad name", "description": "d", "command": "./x", "input_schema": {"type": "object", "properties": {"a": {"$ref": "#/defs/a"}}}}
		], "commands": [
			{"name": "both", "description": "d", "prompt": "p", "command": "./x"},
			{"name": "none", "description": "d"},
			{"name": "ph", "description": "d", "prompt": "{{nope}} {{10}} {{ args }}"},
			{"name": "pa", "description": "d", "prompt": "p", "args": ["x"]}
		]}`: {
			`"GTA_X" uses the reserved GTA_ prefix`, `"1BAD" is not a valid variable name`, `"pass_env": "A-B"`,
			`tool "t": "description" is required`, `"kind" "root"`, `tool "t": "command" is required`, `"timeout_sec" must be between`,
			`"max_output_bytes" must be between`, `the top level must be`, `tool "T" is declared twice`, `tool "bad name": "name" must be`,
			`"$ref" is not supported`, `set either "prompt" or "command"`, `set "prompt" (a template) or "command"`,
			`unknown placeholder {{nope}}`, `unknown placeholder {{10}}`, `only apply to executable commands`},
	}
	for src, wants := range cases {
		_, err := ParseManifest("plugin.json", strings.NewReader(src))
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: want ValidationError, got %v", src, err)
			continue
		}
		for _, w := range wants {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("missing %q in:\n%v", w, err)
			}
		}
	}
	big := `{"name":"x","description":"` + strings.Repeat("a", maxManifestBytes) + `"}`
	if _, err := ParseManifest("p.json", strings.NewReader(big)); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("size cap: %v", err)
	}
}

func TestVersions(t *testing.T) {
	v, err := ParseVersion("v1.10.2-rc.1+build.5")
	if err != nil || v.String() != "1.10.2-rc.1" {
		t.Fatalf("%v %v", v, err)
	}
	for _, bad := range []string{"1", "1.2", "01.2.3", "1.2.3.4", "a.b.c", ""} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	at := func(a, b string) bool {
		va, _ := ParseVersion(a)
		vb, _ := ParseVersion(b)
		return va.AtLeast(vb)
	}
	if !at("0.4.0-dev", "0.4.0") || !at("1.0.0", "0.9.9") || at("0.3.9", "0.4.0") || at("1.2.3", "1.3.0") || !at("2.0.0", "1.99.99") {
		t.Fatal("AtLeast")
	}
}

func TestLoadDirSafety(t *testing.T) {
	needUnix(t)
	root := t.TempDir()
	manifest := func(cmd string) map[string]any {
		return map[string]any{"name": "s", "version": "1.0.0", "description": "d",
			"tools": []map[string]any{{"name": "t", "description": "d", "command": cmd}}}
	}
	outside := filepath.Join(root, "outside.sh")
	os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755)

	dir := writePlugin(t, root, "escape", manifest("../outside.sh"), nil)
	if _, err := LoadDir(dir, LoadOptions{}); err == nil || !strings.Contains(err.Error(), "outside the plugin directory") {
		t.Errorf("dotdot escape: %v", err)
	}
	dir = writePlugin(t, root, "link", manifest("./bin/tool"), nil)
	os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	os.Symlink(outside, filepath.Join(dir, "bin", "tool"))
	if _, err := LoadDir(dir, LoadOptions{}); err == nil || !strings.Contains(err.Error(), "outside the plugin directory") {
		t.Errorf("symlink escape: %v", err)
	}
	dir = writePlugin(t, root, "missing", manifest("./nope"), nil)
	if _, err := LoadDir(dir, LoadOptions{}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing: %v", err)
	}
	dir = writePlugin(t, root, "noexec", manifest("./tool"), nil)
	os.WriteFile(filepath.Join(dir, "tool"), []byte("#!/bin/sh\n"), 0o644)
	if _, err := LoadDir(dir, LoadOptions{}); err == nil || !strings.Contains(err.Error(), "chmod +x") {
		t.Errorf("not executable: %v", err)
	}
	dir = writePlugin(t, root, "ww", manifest("./tool"), map[string]string{"tool": "true"})
	os.Chmod(filepath.Join(dir, "tool"), 0o777)
	if _, err := LoadDir(dir, LoadOptions{}); err == nil || !strings.Contains(err.Error(), "writable by every user") {
		t.Errorf("world-writable tool: %v", err)
	}
	dir = writePlugin(t, root, "wwm", manifest("sh"), nil)
	os.Chmod(filepath.Join(dir, ManifestFile), 0o666)
	if _, err := LoadDir(dir, LoadOptions{}); err == nil || !strings.Contains(err.Error(), "writable by every user") {
		t.Errorf("world-writable manifest: %v", err)
	}
	dir = writePlugin(t, root, "path", manifest("definitely-not-a-real-program-xyz"), nil)
	if _, err := LoadDir(dir, LoadOptions{}); err == nil || !strings.Contains(err.Error(), "not found in PATH") {
		t.Errorf("PATH lookup: %v", err)
	}
	m := manifest("sh")
	m["min_gta_version"] = "9.0.0"
	dir = writePlugin(t, root, "newer", m, nil)
	if _, err := LoadDir(dir, LoadOptions{GTAVersion: "0.5.0"}); err == nil || !strings.Contains(err.Error(), "needs GTA 9.0.0") {
		t.Errorf("min version: %v", err)
	}
	if _, err := LoadDir(filepath.Join(root, "absent"), LoadOptions{}); err == nil || !strings.Contains(err.Error(), "no plugin.json") {
		t.Errorf("absent manifest: %v", err)
	}
	// Absolute paths and PATH programs are fine.
	dir = writePlugin(t, root, "ok", manifest(outside), nil)
	if _, err := LoadDir(dir, LoadOptions{}); err != nil {
		t.Errorf("absolute: %v", err)
	}
}

func cmdManifest(name string, cmds ...string) map[string]any {
	var list []map[string]any
	for _, c := range cmds {
		list = append(list, map[string]any{"name": c, "description": "d", "prompt": name + " " + c + " {{args}}"})
	}
	return map[string]any{"name": name, "version": "1.0.0", "description": "d", "commands": list,
		"tools": []map[string]any{{"name": "t", "description": "d", "kind": "read", "command": "./t"}}}
}

func TestManager(t *testing.T) {
	needUnix(t)
	user, project := t.TempDir(), t.TempDir()
	tool := map[string]string{"t": `echo '{"output":"ok"}'`}
	writePlugin(t, user, "alpha", cmdManifest("alpha", "deploy", "help", "only-alpha"), tool)
	writePlugin(t, user, "beta", cmdManifest("beta", "deploy"), tool)
	writePlugin(t, user, "broken", `{"name": "broken"`, nil)
	writePlugin(t, user, ".hidden", `not even json`, nil)
	os.WriteFile(filepath.Join(user, "README.md"), []byte("not a plugin"), 0o644)
	writePlugin(t, project, "gamma", cmdManifest("gamma", "ship"), tool)
	writePlugin(t, project, "alpha-copy", cmdManifest("alpha", "x"), tool)

	m := NewManager(Options{UserDir: user, ProjectDir: project, Workspace: t.TempDir(), ReservedCommands: []string{"/help", "model"}, Environ: testEnviron})
	err := m.Load()
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("broken plugin must be reported: %v", err)
	}
	if names := pluginNames(m); names != "alpha,beta" {
		t.Fatalf("untrusted project plugins must not load: %s", names)
	}
	if len(m.Untrusted()) != 2 || len(m.Problems()) != 1 {
		t.Fatalf("untrusted %v problems %v", m.Untrusted(), m.Problems())
	}
	var cmdNames []string
	for _, c := range m.Commands() {
		cmdNames = append(cmdNames, c.Name)
	}
	if got := strings.Join(cmdNames, ","); got != "alpha:deploy,alpha:help,beta:deploy,only-alpha" {
		t.Fatalf("command names: %s", got)
	}
	if len(m.Warnings()) != 3 {
		t.Fatalf("warnings: %v", m.Warnings())
	}
	if c, ok := m.Command("/only-alpha"); !ok || c.Plugin().Name() != "alpha" {
		t.Fatal("lookup by name")
	}
	if c, ok := m.Command("ALPHA:only-alpha"); !ok || c.LocalName() != "only-alpha" {
		t.Fatal("lookup by qualified name")
	}
	if _, ok := m.Command("deploy"); ok {
		t.Fatal("an ambiguous bare name must not resolve")
	}
	ts := m.Tools()
	if len(ts) != 2 || ts[0].Spec().Name != "plugin__alpha__t" || ts[1].Spec().Name != "plugin__beta__t" {
		t.Fatalf("tools: %v", ts)
	}

	// Trusting the project loads its plugins; a duplicate name is refused.
	m = NewManager(Options{UserDir: user, ProjectDir: project, TrustProject: true, Disabled: []string{"beta"}, ReservedTools: []string{"plugin__gamma__t"}, Environ: testEnviron})
	err = m.Load()
	if err == nil || !strings.Contains(err.Error(), `plugin "alpha" is already loaded`) {
		t.Fatalf("duplicate: %v", err)
	}
	if names := pluginNames(m); names != "alpha,gamma" {
		t.Fatalf("trusted: %s", names)
	}
	if p, ok := m.Get("gamma"); !ok || p.Source != SourceProject || len(p.Tools) != 0 {
		t.Fatalf("gamma: %+v", p)
	}
	if strings.Join(m.Disabled(), ",") != "beta" {
		t.Fatalf("disabled: %v", m.Disabled())
	}
	if c, ok := m.Command("deploy"); !ok || c.Plugin().Name() != "alpha" {
		t.Fatal("deploy is unambiguous once beta is disabled")
	}

	// Reload picks up changes atomically.
	os.RemoveAll(filepath.Join(user, "broken"))
	writePlugin(t, user, "delta", cmdManifest("delta", "new"), tool)
	if err := m.Reload(); err == nil || !strings.Contains(err.Error(), "alpha-copy") {
		t.Fatalf("reload: %v", err)
	}
	if names := pluginNames(m); names != "alpha,delta,gamma" {
		t.Fatalf("after reload: %s", names)
	}
	// A missing directory is not an error.
	m = NewManager(Options{UserDir: filepath.Join(user, "nope")})
	if err := m.Load(); err != nil || len(m.List()) != 0 {
		t.Fatalf("missing dir: %v", err)
	}
}

func pluginNames(m *Manager) string {
	var names []string
	for _, p := range m.List() {
		names = append(names, p.Name())
	}
	return strings.Join(names, ",")
}

func TestSchemaValidation(t *testing.T) {
	sc, err := compileSchema(json.RawMessage(`{
		"type": "object",
		"properties": {
			"tags":  {"type": "array", "items": {"type": "string", "pattern": "^[a-z]+$"}, "minItems": 1, "maxItems": 2},
			"opt":   {"type": ["string", "null"]},
			"n":     {"type": "number", "exclusiveMinimum": 0, "exclusiveMaximum": 1},
			"kind":  {"const": "x"},
			"meta":  {"type": "object", "additionalProperties": {"type": "integer"}},
			"shape": {"oneOf": [{"type": "string"}, {"type": "integer"}]},
			"any":   {"anyOf": [{"type": "boolean"}, {"type": "string", "maxLength": 2}]},
			"nope":  {"not": {"type": "string"}},
			"both":  {"allOf": [{"type": "integer"}, {"minimum": 10}]}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	ok := []string{`{}`, `{"tags":["ab"],"opt":null,"n":0.5,"kind":"x","meta":{"a":1},"shape":3,"any":"ab","nope":1,"both":12}`}
	for _, in := range ok {
		if err := validateJSON(sc, in); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
	bad := map[string]string{
		`{"tags":[]}`:            "at least 1 items",
		`{"tags":["a","b","c"]}`: "at most 2 items",
		`{"tags":["A"]}`:         "tags[0] must match",
		`{"opt":1}`:              "string or null",
		`{"n":1}`:                "n must be < 1",
		`{"n":0}`:                "n must be > 0",
		`{"kind":"y"}`:           `kind must be "x"`,
		`{"meta":{"a":"b"}}`:     "meta.a must be integer",
		`{"shape":true}`:         "exactly one",
		`{"any":"abc"}`:          "none of the allowed forms",
		`{"nope":"s"}`:           "nope is not allowed",
		`{"both":5}`:             "both must be >= 10",
	}
	for in, want := range bad {
		if err := validateJSON(sc, in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", in, want, err)
		}
	}
	for _, s := range []string{`[]`, `{"type":"strin"}`, `{"type":"object","required":["a"]}`, `{"properties":{"a":{"pattern":"("}}}`,
		`{"properties":{"a":{"minLength":-1}}}`, `{"properties":{"a":{"enum":[]}}}`, `{"properties":{"a":{"anyOf":{}}}}`, `{"properties":{"a":1}}`} {
		if _, err := compileSchema(json.RawMessage(s)); err == nil {
			t.Errorf("schema %s should be rejected", s)
		}
	}
	empty, err := compileSchema(nil)
	if err != nil || len(empty.properties()) != 0 || validateJSON(empty, `{"x":1}`) != nil {
		t.Fatalf("empty schema: %v", err)
	}
}

func validateJSON(sc *schema, in string) error {
	dec := json.NewDecoder(strings.NewReader(in))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return err
	}
	return sc.validate(v, "")
}
