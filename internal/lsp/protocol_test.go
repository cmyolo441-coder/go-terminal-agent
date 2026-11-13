package lsp

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestURIRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix paths")
	}
	for _, p := range []string{"/tmp/a.go", "/home/u/my project/ü/x#1?.ts", "/a/b/../c.py"} {
		uri := PathToURI(p)
		if !strings.HasPrefix(uri, "file:///") {
			t.Errorf("%s -> %s", p, uri)
		}
		back, err := URIToPath(uri)
		if err != nil || back != filepath.Clean(p) {
			t.Errorf("%s -> %s -> %s (%v)", p, uri, back, err)
		}
	}
	if got := PathToURI("/a b/c"); got != "file:///a%20b/c" {
		t.Errorf("space escaped as %s", got)
	}
	if _, err := URIToPath("untitled:Untitled-1"); err == nil {
		t.Error("non-file uri accepted")
	}
	if _, err := URIToPath("file://%zz"); err == nil {
		t.Error("bad uri accepted")
	}
	if !sameFileURI("file:///a/b%2Ec", "file:///a/b.c") || sameFileURI("file:///a", "file:///b") {
		t.Error("sameFileURI wrong")
	}
}

func TestPositionEncodings(t *testing.T) {
	line := "aé😀b"
	// rune columns 0..4 in each unit
	want := map[string][]int{
		EncodingUTF8:  {0, 1, 3, 7, 8},
		EncodingUTF16: {0, 1, 2, 4, 5},
		EncodingUTF32: {0, 1, 2, 3, 4},
	}
	for enc, offs := range want {
		for col, off := range offs {
			if got := toProtocolChar(line, col, enc); got != off {
				t.Errorf("%s: toProtocolChar(%d) = %d, want %d", enc, col, got, off)
			}
			if got := fromProtocolChar(line, off, enc); got != col {
				t.Errorf("%s: fromProtocolChar(%d) = %d, want %d", enc, off, got, col)
			}
		}
		// Past the end clamps.
		if got := toProtocolChar(line, 99, enc); got != offs[len(offs)-1] {
			t.Errorf("%s: clamp to = %d", enc, got)
		}
		if got := fromProtocolChar(line, 99, enc); got != 4 {
			t.Errorf("%s: clamp from = %d", enc, got)
		}
		if toProtocolChar(line, -1, enc) != 0 || fromProtocolChar(line, -1, enc) != 0 {
			t.Errorf("%s: negative not clamped", enc)
		}
	}
	// A UTF-16 offset inside a surrogate pair lands after the rune.
	if got := fromProtocolChar(line, 3, EncodingUTF16); got != 3 {
		t.Errorf("mid-surrogate = %d", got)
	}
}

func TestHoverText(t *testing.T) {
	cases := map[string]string{
		`null`:                                     "",
		`"plain"`:                                  "plain",
		`{"kind":"markdown","value":" **x** "}`:    "**x**",
		`{"language":"go","value":"func f()"}`:     "```go\nfunc f()\n```",
		`["a", {"language":"go","value":"b"}, ""]`: "a\n\n```go\nb\n```",
		`42`: "",
		`{"kind":"plaintext","value":"multi\nline"}`: "multi\nline",
	}
	for in, want := range cases {
		if got := hoverText(json.RawMessage(in)); got != want {
			t.Errorf("hoverText(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeLocations(t *testing.T) {
	r := Range{Start: Position{1, 2}, End: Position{1, 5}}
	loc := Location{URI: "file:///a.go", Range: r}
	cases := map[string][]Location{
		`null`: nil,
		``:     nil,
		`{"uri":"file:///a.go","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}}`:                                                                                                        {loc},
		`[{"uri":"file:///a.go","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}}]`:                                                                                                      {loc},
		`[{"targetUri":"file:///a.go","targetRange":{"start":{"line":0,"character":0},"end":{"line":9,"character":0}},"targetSelectionRange":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}}]`: {loc},
		`{"targetUri":"file:///a.go","targetSelectionRange":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}}`:                                                                                   {loc},
		`[]`: {},
		`{}`: nil,
	}
	for in, want := range cases {
		got, err := decodeLocations(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
			t.Errorf("%s: got %+v, want %+v", in, got, want)
		}
	}
	if _, err := decodeLocations(json.RawMessage(`"x"`)); err == nil {
		t.Error("string accepted")
	}
}

func TestParseCaps(t *testing.T) {
	c := parseCaps(json.RawMessage(`{}`))
	if !c.openClose || c.change != 1 || c.encoding != EncodingUTF16 || c.pull || c.definition {
		t.Errorf("defaults %+v", c)
	}
	c = parseCaps(json.RawMessage(`{"positionEncoding":"utf-8","textDocumentSync":2,"diagnosticProvider":{"interFileDependencies":true},"definitionProvider":true,"referencesProvider":{},"hoverProvider":false}`))
	if c.encoding != EncodingUTF8 || c.change != 2 || !c.openClose || !c.pull || !c.definition || !c.references || c.hover {
		t.Errorf("parsed %+v", c)
	}
	c = parseCaps(json.RawMessage(`{"positionEncoding":"utf-7","textDocumentSync":{"openClose":true,"change":0,"save":{"includeText":true}},"hoverProvider":null}`))
	if c.encoding != EncodingUTF16 || c.change != 0 || !c.save || !c.saveText || c.hover {
		t.Errorf("parsed %+v", c)
	}
	c = parseCaps(json.RawMessage(`{"textDocumentSync":{"save":true}}`))
	if !c.save || c.saveText || c.change != 1 {
		t.Errorf("parsed %+v", c)
	}
	c = parseCaps(json.RawMessage(`{"textDocumentSync":0}`))
	if c.openClose {
		t.Errorf("sync none %+v", c)
	}
	if c := parseCaps(json.RawMessage(`[`)); c.change != 1 {
		t.Errorf("garbage %+v", c)
	}
}

func TestSeverityAndCodes(t *testing.T) {
	for in, want := range map[string]Severity{"error": SeverityError, "Warnings": SeverityWarning, "info": SeverityInformation, " hint ": SeverityHint, "all": SeverityHint} {
		if got, ok := ParseSeverity(in); !ok || got != want {
			t.Errorf("ParseSeverity(%q) = %v %v", in, got, ok)
		}
	}
	if _, ok := ParseSeverity("fatal"); ok {
		t.Error("unknown severity accepted")
	}
	if (Diagnostic{}).EffectiveSeverity() != SeverityError || (Diagnostic{Severity: 9}).EffectiveSeverity() != SeverityError {
		t.Error("missing severity should be an error")
	}
	if Severity(7).String() != "error" || SeverityHint.String() != "hint" {
		t.Error("severity names")
	}
	for raw, want := range map[string]string{``: "", `null`: "", `"E12"`: "E12", `123`: "123"} {
		if got := (Diagnostic{Code: json.RawMessage(raw)}).CodeString(); got != want {
			t.Errorf("code %s = %q", raw, got)
		}
	}
}

func TestSettingsSection(t *testing.T) {
	c := &Client{cfg: ServerConfig{Settings: json.RawMessage(`{"gopls":{"staticcheck":true},"python.analysis":{"typeCheckingMode":"strict"},"a":{"b":{"c":3}}}`)}}
	cases := map[string]string{
		"gopls":             `{"staticcheck":true}`,
		"gopls.staticcheck": `true`,
		"python.analysis":   `{"typeCheckingMode":"strict"}`,
		"a.b.c":             `3`,
		"a.x":               `null`,
		"missing":           `null`,
	}
	for section, want := range cases {
		got, _ := json.Marshal(c.settingsSection(section))
		if string(got) != want {
			t.Errorf("section %q = %s, want %s", section, got, want)
		}
	}
	if all, _ := json.Marshal(c.settingsSection("")); !strings.Contains(string(all), "staticcheck") {
		t.Errorf("whole settings = %s", all)
	}
	empty := &Client{}
	if empty.settingsSection("x") != nil {
		t.Error("no settings should answer null")
	}
}

func TestLanguageID(t *testing.T) {
	if languageID("/a/b.TSX", nil) != "typescriptreact" || languageID("x.go", nil) != "go" || languageID("x.weird", nil) != "weird" {
		t.Error("language ids")
	}
	if languageID("x.h", map[string]string{".h": "cpp"}) != "cpp" {
		t.Error("override ignored")
	}
}

func TestFormatDiagnostics(t *testing.T) {
	res := &FileDiagnostics{
		Encoding: EncodingUTF16,
		Lines:    []string{"package x", "\tvar 😀 = bad", ""},
		Diagnostics: []Diagnostic{
			{Range: Range{Start: Position{1, 0}}, Severity: SeverityWarning, Message: "unused"},
			{Range: Range{Start: Position{1, 8}}, Severity: SeverityError, Message: "undefined: bad\nmore detail", Source: "compiler", Code: json.RawMessage(`"UndeclaredName"`)},
			{Range: Range{Start: Position{9, 3}}, Message: "out of range line"},
		},
	}
	out := FormatDiagnostics(res, 0)
	want := "L2:8 error: undefined: bad … [compiler UndeclaredName]\n" +
		"        var 😀 = bad\n" +
		"L10:4 error: out of range line\n" +
		"L2:1 warning: unused\n" +
		"        var 😀 = bad\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	if out := FormatDiagnostics(res, 1); !strings.HasSuffix(out, "… 2 more not shown\n") {
		t.Errorf("limit:\n%s", out)
	}
	// The input order is left alone.
	if res.Diagnostics[0].Message != "unused" {
		t.Error("FormatDiagnostics reordered its input")
	}
	if clipLine("héllo", 2) != "h…" || clipLine("abc", 5) != "abc" {
		t.Error("clipLine")
	}
}

func TestSymbolColumn(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, filepath.Join(dir, "a.go"), "x := 1\n\t😀 helper2 := helper(xhelper, helper)\n")
	cases := []struct {
		symbol string
		want   int
		err    string
	}{
		{"helper", 15, ""},
		{"helper2", 4, ""},
		{"pkg.helper", 15, ""},
		{"xhelper", 22, ""},
		{"nothere", 0, "does not occur"},
	}
	for _, tc := range cases {
		got, err := symbolColumn(p, 2, tc.symbol)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err %v", tc.symbol, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: col %d err %v, want %d", tc.symbol, got, err, tc.want)
		}
	}
	if _, err := symbolColumn(p, 10, "x"); err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Errorf("past end: %v", err)
	}
}
