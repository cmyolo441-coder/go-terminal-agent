package autopilot

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func TestExtractObjectPrefersLastJSONBlock(t *testing.T) {
	text := "Example:\n```json\n{\"goal\": \"example\"}\n```\nFinal:\n```json\n{\"goal\": \"real\", \"steps\": []}\n```\n"
	raw, err := extractObject(text, "goal", "steps")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "real") {
		t.Fatalf("picked the wrong block: %s", raw)
	}
}

func TestExtractObjectFallbacks(t *testing.T) {
	cases := map[string]string{
		"bare object":         `{"verdict": "approve"}`,
		"object in prose":     `Here you go: {"verdict": "approve"} — done.`,
		"unlabelled fence":    "```\n{\"verdict\": \"approve\"}\n```",
		"trailing commas":     "```json\n{\"verdict\": \"approve\", \"issues\": [1, 2,],}\n```",
		"line comments":       "```json\n{\n  // the verdict\n  \"verdict\": \"approve\", \"url\": \"http://x\"\n}\n```",
		"four-backtick fence": "````json\n{\"verdict\": \"approve\"}\n````",
		"unterminated fence":  "```json\n{\"verdict\": \"approve\"}",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := extractObject(text, "verdict"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtractObjectErrorsAreActionable(t *testing.T) {
	cases := []struct{ text, want string }{
		{"", "empty"},
		{"I could not finish.", "no ```json fenced block"},
		{"```json\n{\"verdict\": \"approve\"\n\"x\": 1}\n```", "line 2"},
		{"```json\n{\"verdict\": [\n```", "cut off"},
		{"```json\n{\"summary\": \"x\"}\n```", `missing the required field(s) "verdict"`},
		{"```json\n[1, 2]\n```", "must be an object"},
		{"```json\n{\"a\": 1} {\"b\": 2}\n```", "unexpected data"},
	}
	for _, c := range cases {
		_, err := extractObject(c.text, "verdict")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("extractObject(%q) error %v, want it to mention %q", c.text, err, c.want)
		}
	}
}

func TestRelaxJSONKeepsStrings(t *testing.T) {
	in := `{"a": "x,}", "b": "// not a comment", "c": [1,],}`
	got := relaxJSON(in)
	if got != `{"a": "x,}", "b": "// not a comment", "c": [1]}` {
		t.Fatalf("relaxJSON = %s", got)
	}
}

const validPlan = "Approach: small change.\n```json\n" + `{
  "goal": "  Add a greeting  ",
  "steps": [
    {"title": "Create   hello.txt", "details": "write hi", "files": ["./hello.txt", "hello.txt"]},
    {"title": "Document it", "files": ["docs/README.md"]}
  ],
  "files": [{"path": "hello.txt", "action": "add", "reason": "new file"}, "notes.md"],
  "risks": ["none really", {"risk": "overwrite", "mitigation": "check first"}, {"risk": "  "}],
  "verification": "cat hello.txt prints hi"
}` + "\n```"

func TestParsePlanNormalizes(t *testing.T) {
	root := t.TempDir()
	p, err := ParsePlan(validPlan, root)
	if err != nil {
		t.Fatal(err)
	}
	if p.Goal != "Add a greeting" {
		t.Errorf("goal %q", p.Goal)
	}
	if p.Steps[0].ID != 1 || p.Steps[1].ID != 2 || p.Steps[0].Title != "Create hello.txt" {
		t.Errorf("steps not normalized: %+v", p.Steps)
	}
	if len(p.Steps[0].Files) != 1 || p.Steps[0].Files[0] != "hello.txt" {
		t.Errorf("step files not deduplicated: %v", p.Steps[0].Files)
	}
	wantFiles := []FileTouch{
		{Path: "hello.txt", Action: ActionCreate, Reason: "new file"},
		{Path: "notes.md", Action: ActionModify},
		{Path: "docs/README.md", Action: ActionModify, Reason: "referenced by step 2"},
	}
	if len(p.Files) != len(wantFiles) {
		t.Fatalf("files %+v", p.Files)
	}
	for i, f := range wantFiles {
		if p.Files[i] != f {
			t.Errorf("file %d = %+v, want %+v", i, p.Files[i], f)
		}
	}
	if len(p.Risks) != 2 || p.Risks[0].Risk != "none really" || p.Risks[1].Mitigation != "check first" {
		t.Errorf("risks %+v", p.Risks)
	}
	if len(p.Verification.Checks) != 1 {
		t.Errorf("verification %+v", p.Verification)
	}
	md := p.Markdown()
	for _, s := range []string{"**Goal:** Add a greeting", "1. Create hello.txt", "create `hello.txt` — new file", "mitigation: check first", "cat hello.txt prints hi"} {
		if !strings.Contains(md, s) {
			t.Errorf("markdown lacks %q:\n%s", s, md)
		}
	}
	todos := p.Todos()
	if len(todos) != 2 || todos[1].Content != "2. Document it" || todos[0].Status != StatusPending {
		t.Errorf("todos %+v", todos)
	}
	// JSON round-trips through the parser.
	if _, err := ParsePlan("```json\n"+p.JSON()+"\n```", root); err != nil {
		t.Errorf("rendered JSON does not re-parse: %v", err)
	}
}

func TestParsePlanPaths(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "pkg", "a.go")
	p, err := ParsePlan(planWithFile(abs), root)
	if err != nil {
		t.Fatal(err)
	}
	if p.Files[0].Path != "pkg/a.go" {
		t.Errorf("absolute path inside root not made relative: %q", p.Files[0].Path)
	}
	for _, bad := range []string{"../escape.go", filepath.Join(filepath.Dir(root), "other", "x.go"), "a/../../b.go"} {
		if _, err := ParsePlan(planWithFile(bad), root); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
			t.Errorf("path %q: error %v, want outside-workspace error", bad, err)
		}
	}
}

func planWithFile(path string) string {
	return "```json\n" + `{"goal": "g", "steps": [{"id": 1, "title": "t"}], "files": [{"path": ` + jsonString(path) + `, "action": "modify"}], "verification": {"commands": ["go test ./..."]}}` + "\n```"
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func TestParsePlanRejectsInvalidPlans(t *testing.T) {
	steps40 := strings.Repeat(`{"title": "x"},`, MaxPlanSteps+1)
	cases := []struct{ name, json, want string }{
		{"no goal", `{"goal": " ", "steps": [{"title": "a"}], "verification": "v"}`, `"goal" is required`},
		{"no steps", `{"goal": "g", "steps": [], "verification": "v"}`, "at least one step"},
		{"too many", `{"goal": "g", "steps": [` + strings.TrimSuffix(steps40, ",") + `], "verification": "v"}`, "at most 40"},
		{"bad numbering", `{"goal": "g", "steps": [{"id": 1, "title": "a"}, {"id": 3, "title": "b"}], "verification": "v"}`, "numbered 1 to 2"},
		{"empty title", `{"goal": "g", "steps": [{"title": "  "}], "verification": "v"}`, "step 1 has no title"},
		{"no verification", `{"goal": "g", "steps": [{"title": "a"}], "verification": {"commands": [" "]}}`, `"verification" must list`},
		{"bad action", `{"goal": "g", "steps": [{"title": "a"}], "files": [{"path": "x", "action": "explode"}], "verification": "v"}`, "unknown action"},
		{"wrong type", `{"goal": "g", "steps": "do it", "verification": "v"}`, "does not match the schema"},
		{"missing steps key", `{"goal": "g"}`, `"steps"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParsePlan("```json\n"+c.json+"\n```", t.TempDir())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestStepStatesFromTodos(t *testing.T) {
	p := &Plan{Steps: []Step{{ID: 1, Title: "a"}, {ID: 2, Title: "b"}, {ID: 3, Title: "c"}}}
	got := stepStates(p, []tools.Todo{
		{Content: "1. a", Status: StatusCompleted},
		{Content: "Step 3: c", Status: StatusInProgress},
		{Content: "extra item", Status: StatusCompleted},
	})
	if got[0].Status != StatusCompleted || got[1].Status != StatusPending || got[2].Status != StatusInProgress {
		t.Fatalf("by number: %+v", got)
	}
	got = stepStates(p, []tools.Todo{{Content: "x", Status: StatusCompleted}, {Content: "y", Status: StatusCompleted}, {Content: "z", Status: StatusPending}})
	if StepsDone(got) != 2 || got[2].Status != StatusPending {
		t.Fatalf("by position: %+v", got)
	}
	got = stepStates(p, []tools.Todo{{Content: "x", Status: StatusCompleted}})
	if StepsDone(got) != 0 {
		t.Fatalf("unrelated list must not map: %+v", got)
	}
	for in, want := range map[string]int{"1. a": 1, "12) b": 12, "step 4 - c": 4, "Step 5: d": 5, "7 x": 7} {
		if id, ok := todoStepID(in); !ok || id != want {
			t.Errorf("todoStepID(%q) = %d, %v; want %d", in, id, ok, want)
		}
	}
	for _, in := range []string{"a. b", "", "3", "1x", "step"} {
		if _, ok := todoStepID(in); ok {
			t.Errorf("todoStepID(%q) should not match", in)
		}
	}
}

func TestParseReview(t *testing.T) {
	r, err := ParseReview("```json\n{\"verdict\": \"Request_Changes\", \"summary\": \" s \", \"issues\": [{\"severity\": \"critical\", \"step\": 9, \"description\": \" d \"}, {\"severity\": \"nit\", \"step\": 1, \"description\": \"n\"}, {\"description\": \"m\"}]}\n```", 2)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != VerdictRevise || r.Summary != "s" || r.Approved() {
		t.Fatalf("review %+v", r)
	}
	if r.Issues[0].Severity != SeverityBlocker || r.Issues[0].Step != 0 || r.Issues[0].Description != "d" {
		t.Errorf("issue 0 not normalized: %+v", r.Issues[0])
	}
	if r.Issues[1].Severity != SeverityMinor || r.Issues[2].Severity != SeverityMajor {
		t.Errorf("severities: %+v", r.Issues)
	}
	if b := r.Blocking(); len(b) != 2 {
		t.Errorf("blocking issues %+v", b)
	}
	ok, err := ParseReview(`{"verdict": "LGTM", "issues": [{"severity": "minor", "description": "x"}]}`, 2)
	if err != nil || !ok.Approved() {
		t.Fatalf("approve with minor issues must pass: %+v %v", ok, err)
	}
	blocked, _ := ParseReview(`{"verdict": "approve", "issues": [{"severity": "blocker", "description": "x"}]}`, 2)
	if blocked.Approved() {
		t.Fatal("an approval that lists a blocker is not an approval")
	}
	for _, bad := range []string{`{"verdict": "maybe"}`, `{"verdict": "revise", "issues": []}`, `{"verdict": "revise", "issues": [{"description": " "}]}`} {
		if _, err := ParseReview(bad, 2); err == nil {
			t.Errorf("ParseReview(%s) should fail", bad)
		}
	}
}

func TestParseVerifyReport(t *testing.T) {
	v, err := ParseVerifyReport(`{"verdict": "incomplete", "gaps": [{"step": 2, "severity": "major", "description": "step 2 missing"}]}`, 2)
	if err != nil || v.Verdict != VerdictGaps || v.Passed() || v.Gaps[0].String() != "[major] step 2: step 2 missing" {
		t.Fatalf("%+v %v", v, err)
	}
	v, err = ParseVerifyReport(`{"verdict": "pass", "summary": "ok", "gaps": [{"severity": "minor", "description": "typo"}]}`, 2)
	if err != nil || !v.Passed() {
		t.Fatalf("pass with a minor gap: %+v %v", v, err)
	}
	v, _ = ParseVerifyReport(`{"verdict": "pass", "gaps": [{"description": "big hole"}]}`, 2)
	if v.Passed() {
		t.Fatal("a pass that lists a major gap is not a pass")
	}
	if _, err := ParseVerifyReport(`{"verdict": "gaps"}`, 2); err == nil {
		t.Fatal("gaps verdict without gaps must fail")
	}
}
