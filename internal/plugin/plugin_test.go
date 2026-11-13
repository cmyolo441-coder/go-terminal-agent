package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func needUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin executables in these tests are shell scripts")
	}
}

// writePlugin creates a plugin directory with a manifest and scripts.
func writePlugin(t *testing.T, root, dir string, manifest any, scripts map[string]string) string {
	t.Helper()
	p := filepath.Join(root, dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	var data []byte
	switch m := manifest.(type) {
	case string:
		data = []byte(m)
	default:
		var err error
		if data, err = json.MarshalIndent(m, "", "  "); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(p, ManifestFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range scripts {
		f := filepath.Join(p, name)
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

const echoTool = `input=$(cat | sed 's/"/\\"/g')
printf '{"output": "got %s in %s as %s/%s secret=[%s] pass=[%s] static=[%s]", "is_error": false}' "$input" "$PWD" "$GTA_PLUGIN_NAME" "$GTA_TOOL_NAME" "$SECRET_TOKEN" "$PASSED" "$STATIC"
`

func basicManifest() map[string]any {
	return map[string]any{
		"name": "demo", "version": "1.2.3", "description": "Demo plugin",
		"pass_env": []string{"PASSED"},
		"env":      map[string]string{"STATIC": "${PLUGIN_DIR}/data"},
		"tools": []map[string]any{{
			"name": "echo", "description": "Echo input", "kind": "read", "command": "./bin/echo",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"msg":   map[string]any{"type": "string", "minLength": 1},
					"count": map[string]any{"type": "integer", "minimum": 1, "maximum": 5},
					"mode":  map[string]any{"enum": []string{"fast", "slow"}},
				},
				"required":             []string{"msg"},
				"additionalProperties": false,
			},
		}},
		"commands": []map[string]any{
			{"name": "review", "description": "Review a file", "argument_hint": "<file> [focus]", "prompt": "Review {{1}} focusing on {{2}}. Full request: {{args}}"},
			{"name": "plain", "description": "Plain prompt", "prompt": "Summarise the repo."},
			{"name": "status", "description": "Show status", "command": "./bin/status", "args": []string{"--fixed"}},
		},
	}
}

func testEnviron() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=/home/test", "SECRET_TOKEN=leak", "PASSED=yes", "LC_ALL=C"}
}

func loadDemo(t *testing.T) (*Plugin, string) {
	t.Helper()
	needUnix(t)
	root := t.TempDir()
	ws := t.TempDir()
	dir := writePlugin(t, root, "demo", basicManifest(), map[string]string{
		"bin/echo":   echoTool,
		"bin/status": `echo "status $* args=[$GTA_ARGS]"`,
	})
	p, err := LoadDir(dir, LoadOptions{Source: SourceUser, Workspace: ws, GTAVersion: "1.0.0", Environ: testEnviron})
	if err != nil {
		t.Fatal(err)
	}
	return p, ws
}

func TestToolRunsWithScrubbedEnv(t *testing.T) {
	p, ws := loadDemo(t)
	if len(p.Tools) != 1 || len(p.Commands) != 3 {
		t.Fatalf("plugin: %+v", p)
	}
	tool := p.Tools[0]
	spec := tool.Spec()
	if spec.Name != "plugin__demo__echo" || tool.Kind() != tools.KindRead || !strings.Contains(spec.Description, `plugin "demo"`) {
		t.Fatalf("spec: %+v kind %v", spec, tool.Kind())
	}
	if len(spec.Required) != 1 || spec.Required[0] != "msg" || spec.Properties["count"].(map[string]any)["maximum"] != int64(5) {
		t.Fatalf("schema passthrough: %+v", spec)
	}
	env, _ := tools.NewEnv(ws)
	call, err := tool.Prepare(env, json.RawMessage(`{ "msg": "hi", "count": 2 }`))
	if err != nil {
		t.Fatal(err)
	}
	if call.Kind != tools.KindRead || call.Command != "plugin__demo__echo" || !strings.Contains(call.Preview, `"msg": "hi"`) {
		t.Fatalf("call: %+v", call)
	}
	out, err := call.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	realWS, _ := filepath.EvalSymlinks(ws)
	want := `got {"msg":"hi","count":2} in ` + realWS + ` as demo/echo secret=[] pass=[yes] static=[` + p.Dir + `/data]`
	if out != want {
		t.Fatalf("output:\n got %s\nwant %s", out, want)
	}
}

func TestToolInputValidatedBeforeRun(t *testing.T) {
	p, _ := loadDemo(t)
	tool := p.Tools[0]
	cases := map[string]string{
		`{}`:                           `"msg" is required`,
		`{"msg": ""}`:                  "at least 1 characters",
		`{"msg": "x", "count": 2.5}`:   "count must be integer",
		`{"msg": "x", "count": 9}`:     "count must be <= 5",
		`{"msg": "x", "mode": "warp"}`: `mode must be one of "fast", "slow"`,
		`{"msg": "x", "extra": 1}`:     `unknown property "extra"`,
		`[1,2]`:                        "must be a JSON object",
		`{"msg": "x"} {}`:              "trailing data",
		`{"msg": 5}`:                   "msg must be string, got number",
	}
	for in, want := range cases {
		_, err := tool.Prepare(nil, json.RawMessage(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", in, want, err)
		}
	}
	if _, err := tool.Prepare(nil, json.RawMessage(`{"msg":"x","count":3.0,"mode":"fast"}`)); err != nil {
		t.Fatalf("3.0 is an integer: %v", err)
	}
}

func TestToolResultProtocol(t *testing.T) {
	needUnix(t)
	root := t.TempDir()
	scripts := map[string]string{
		"err":      `echo '{"output": "bad thing", "is_error": true}'`,
		"exit":     `echo "boom" >&2; exit 3`,
		"exitjson": `echo '{"output": "partial"}'; exit 2`,
		"garbage":  `echo 'hello world'`,
		"empty":    `true`,
		"blank":    `echo '{"is_error": false}'`,
		"big":      `head -c 5000 /dev/zero | tr '\0' 'a'`,
		"slow":     `sleep 30 & echo $! > "$GTA_WORKSPACE/child.pid"; sleep 30`,
		"long":     `printf '{"output": "%s"}' "$(head -c 300 /dev/zero | tr '\0' 'b')"`,
	}
	var toolList []map[string]any
	for name := range scripts {
		tm := map[string]any{"name": name, "description": "t", "command": "./" + name}
		switch name {
		case "big":
			tm["max_output_bytes"] = 1000
		case "slow":
			tm["timeout_sec"] = 1
		}
		toolList = append(toolList, tm)
	}
	dir := writePlugin(t, root, "proto", map[string]any{"name": "proto", "version": "0.1.0", "description": "d", "tools": toolList}, scripts)
	ws := t.TempDir()
	p, err := LoadDir(dir, LoadOptions{Workspace: ws, Environ: testEnviron})
	if err != nil {
		t.Fatal(err)
	}
	env, _ := tools.NewEnv(ws)
	env.OutputLimit = 120
	run := func(name string) (string, error) {
		for _, tl := range p.Tools {
			if tl.LocalName() == name {
				c, err := tl.Prepare(env, nil)
				if err != nil {
					t.Fatal(err)
				}
				return c.Run(context.Background())
			}
		}
		t.Fatalf("no tool %s", name)
		return "", nil
	}
	if out, err := run("err"); out != "bad thing" || err == nil || !strings.Contains(err.Error(), "reported an error") {
		t.Errorf("is_error: %q %v", out, err)
	}
	if _, err := run("exit"); err == nil || !strings.Contains(err.Error(), "status 3") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("exit: %v", err)
	}
	if out, err := run("exitjson"); out != "partial" || err == nil || !strings.Contains(err.Error(), "status 2") {
		t.Errorf("exit with json: %q %v", out, err)
	}
	if _, err := run("garbage"); err == nil || !strings.Contains(err.Error(), "did not print a JSON result") {
		t.Errorf("garbage: %v", err)
	}
	if _, err := run("empty"); err == nil || !strings.Contains(err.Error(), "no output") {
		t.Errorf("empty: %v", err)
	}
	if out, err := run("blank"); err != nil || out != "(tool returned no output)" {
		t.Errorf("blank: %q %v", out, err)
	}
	if _, err := run("big"); err == nil || !strings.Contains(err.Error(), "more than 1000 bytes") {
		t.Errorf("overflow: %v", err)
	}
	if out, err := run("long"); err != nil || !strings.Contains(out, "characters omitted") {
		t.Errorf("env.OutputLimit truncation: %q %v", out, err)
	}
	start := time.Now()
	_, err = run("slow")
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout did not stop the process promptly")
	}
	pidData, err := os.ReadFile(filepath.Join(ws, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("background child %d survived the process-group kill", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestToolCancellation(t *testing.T) {
	needUnix(t)
	root := t.TempDir()
	dir := writePlugin(t, root, "c", map[string]any{"name": "c", "version": "1.0.0", "description": "d",
		"tools": []map[string]any{{"name": "wait", "description": "w", "command": "./wait"}}}, map[string]string{"wait": "sleep 30"})
	p, err := LoadDir(dir, LoadOptions{Workspace: root, Environ: testEnviron})
	if err != nil {
		t.Fatal(err)
	}
	call, _ := p.Tools[0].Prepare(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := call.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestToolKindsDrivePermissions(t *testing.T) {
	needUnix(t)
	root := t.TempDir()
	dir := writePlugin(t, root, "k", map[string]any{"name": "k", "version": "1.0.0", "description": "d",
		"tools": []map[string]any{
			{"name": "r", "description": "r", "kind": "read", "command": "./x"},
			{"name": "w", "description": "w", "kind": "write", "command": "./x"},
			{"name": "e", "description": "e", "command": "./x"},
		}}, map[string]string{"x": `echo '{"output":"ok"}'`})
	p, err := LoadDir(dir, LoadOptions{Workspace: root, Environ: testEnviron})
	if err != nil {
		t.Fatal(err)
	}
	gate := permission.NewGate(permission.ModeAsk, nil)
	plan := permission.NewGate(permission.ModePlan, nil)
	want := map[string][2]permission.Verdict{"r": {permission.Allow, permission.Allow}, "w": {permission.Ask, permission.Deny}, "e": {permission.Ask, permission.Deny}}
	for _, tl := range p.Tools {
		c, err := tl.Prepare(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		v1, _ := gate.Check(c)
		v2, _ := plan.Check(c)
		if w := want[tl.LocalName()]; v1 != w[0] || v2 != w[1] {
			t.Errorf("%s: ask=%v plan=%v", tl.LocalName(), v1, v2)
		}
	}
}

func TestCommands(t *testing.T) {
	p, _ := loadDemo(t)
	byName := map[string]*Command{}
	for _, c := range p.Commands {
		byName[c.LocalName()] = c
	}
	review := byName["review"]
	if !review.IsPrompt() || review.ArgumentHint != "<file> [focus]" || review.QualifiedName() != "demo:review" {
		t.Fatalf("review: %+v", review)
	}
	got, err := review.Expand(`"main file.go" errors`)
	if err != nil || got != `Review main file.go focusing on errors. Full request: "main file.go" errors` {
		t.Fatalf("expand: %q %v", got, err)
	}
	if got, _ := review.Expand(""); got != "Review  focusing on . Full request: " {
		t.Fatalf("expand empty: %q", got)
	}
	if _, err := review.Expand(`"unterminated`); err == nil {
		t.Fatal("bad quoting must fail")
	}
	if got, _ := byName["plain"].Expand("in detail"); got != "Summarise the repo.\n\nin detail" {
		t.Fatalf("append: %q", got)
	}
	if _, err := review.Run(context.Background(), ""); err == nil {
		t.Fatal("prompt commands cannot run")
	}
	status := byName["status"]
	if status.IsPrompt() {
		t.Fatal("status is executable")
	}
	out, err := status.Run(context.Background(), `a 'b c'`)
	if err != nil || out != "status --fixed a b c args=[a 'b c']" {
		t.Fatalf("run: %q %v", out, err)
	}
	if _, err := status.Expand("x"); err == nil {
		t.Fatal("executable commands have no prompt")
	}
}

func TestSplitWords(t *testing.T) {
	cases := map[string][]string{
		``:                      nil,
		`a  b`:                  {"a", "b"},
		`'it''s'`:               {"its"},
		`"a \"q\" \\ b" c`:      {`a "q" \ b`, "c"},
		`x\ y`:                  {"x y"},
		`''`:                    {""},
		"tab\tsep\nline":        {"tab", "sep", "line"},
		`"$HOME" '$(rm -rf /)'`: {"$HOME", "$(rm -rf /)"},
	}
	for in, want := range cases {
		got, err := splitWords(in)
		if err != nil || strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := splitWords(`"open`); err == nil {
		t.Fatal("unterminated quote")
	}
}

func TestBuildEnv(t *testing.T) {
	env := buildEnv([]string{"PATH=/bin", "AWS_SECRET=x", "LC_CTYPE=UTF-8", "TOKEN=t", "junk"}, []string{"TOKEN"},
		map[string]string{"A": "1", "PATH": "/custom"}, map[string]string{"GTA_X": "y"})
	got := strings.Join(env, " ")
	if got != "PATH=/custom LC_CTYPE=UTF-8 TOKEN=t A=1 GTA_X=y" {
		t.Fatalf("env: %s", got)
	}
}
