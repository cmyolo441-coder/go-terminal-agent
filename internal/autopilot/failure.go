package autopilot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Framework identifies a test runner, which selects the output parser.
type Framework string

const (
	FrameworkGo     Framework = "go"
	FrameworkPytest Framework = "pytest"
	FrameworkJest   Framework = "jest"
	FrameworkVitest Framework = "vitest"
	FrameworkCargo  Framework = "cargo"
	FrameworkMaven  Framework = "maven"
	FrameworkGradle Framework = "gradle"
	// FrameworkAuto tries every parser, for runners whose output format is
	// not known in advance (make test, a custom npm script).
	FrameworkAuto Framework = "auto"
)

// FailureKind classifies a failure.
type FailureKind string

const (
	// KindTest is a failing test case.
	KindTest FailureKind = "test"
	// KindBuild is a compile, type-check or vet error that keeps tests from
	// running at all.
	KindBuild FailureKind = "build"
	// KindError is a suite-level error: a collection error, a suite that
	// failed to load, a package that failed without a failing test, or
	// output that could not be attributed to a test.
	KindError FailureKind = "error"
	// KindTimeout is reported when the test command exceeded its timeout.
	KindTimeout FailureKind = "timeout"
)

// Failure is one structured problem found in test output.
type Failure struct {
	Framework Framework   `json:"framework"`
	Kind      FailureKind `json:"kind"`
	// Suite is the package, module, file or class the failure belongs to.
	Suite string `json:"suite,omitempty"`
	// Test is the test's name, e.g. "TestAdd/negative" or "sum › adds".
	Test string `json:"test,omitempty"`
	// File and Line locate the failure as precisely as the output allows.
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	// Message is the assertion or error text, possibly several lines.
	Message string `json:"message,omitempty"`
}

// Location renders file:line, or just the file, or "".
func (f Failure) Location() string {
	switch {
	case f.File == "":
		return ""
	case f.Line > 0:
		return f.File + ":" + strconv.Itoa(f.Line)
	default:
		return f.File
	}
}

// Key identifies a failure across runs, so the fix loop can tell whether a
// change made progress. It deliberately ignores line numbers of test
// failures (they move as code is edited) and volatile numbers in messages.
func (f Failure) Key() string {
	switch {
	case f.Kind == KindTimeout:
		return "timeout"
	case f.Test != "":
		return string(f.Kind) + "|" + f.Suite + "|" + f.Test
	case f.Kind == KindBuild:
		return "build|" + f.File + "|" + normalizeVolatile(firstLine(f.Message))
	default:
		sum := sha256.Sum256([]byte(normalizeVolatile(f.Message)))
		return string(f.Kind) + "|" + f.Suite + "|" + f.File + "|" + hex.EncodeToString(sum[:6])
	}
}

// Title is a one-line label for the failure.
func (f Failure) Title() string {
	var b strings.Builder
	switch f.Kind {
	case KindBuild:
		b.WriteString("[build] ")
	case KindError:
		b.WriteString("[error] ")
	case KindTimeout:
		b.WriteString("[timeout] ")
	}
	name := f.Test
	if name == "" {
		name = firstLine(f.Message)
		if loc := f.Location(); loc != "" && f.Kind == KindBuild {
			return b.String() + loc + ": " + clipRunes(name, 200)
		}
	}
	b.WriteString(clipRunes(name, 200))
	if f.Suite != "" && f.Test != "" {
		b.WriteString(" (" + f.Suite + ")")
	}
	if loc := f.Location(); loc != "" {
		b.WriteString(" at " + loc)
	}
	return b.String()
}

var volatileRe = regexp.MustCompile(`0x[0-9a-fA-F]+|\d+(\.\d+)?`)

// normalizeVolatile replaces numbers (durations, addresses, counters) so
// the same failure hashes the same across runs.
func normalizeVolatile(s string) string {
	return volatileRe.ReplaceAllString(strings.TrimSpace(s), "#")
}

// maxParsedFailures bounds how many failures one parse keeps.
const maxParsedFailures = 500

// maxMessageLines bounds each failure's message as parsed.
const maxMessageLines = 30

// ParseFailures extracts structured failures from test output. The output
// should already be cleaned of terminal escapes (RunTests does this).
// Unknown frameworks and FrameworkAuto try every parser.
func ParseFailures(fw Framework, output string) []Failure {
	output = cleanOutput(output)
	var out []Failure
	switch fw {
	case FrameworkGo:
		out = parseGo(output)
	case FrameworkPytest:
		out = parsePytest(output)
	case FrameworkJest, FrameworkVitest:
		out = parseJS(output, fw)
	case FrameworkCargo:
		out = parseCargo(output)
	case FrameworkMaven, FrameworkGradle:
		out = parseJVM(output, fw)
	default:
		out = append(out, parseGo(output)...)
		out = append(out, parsePytest(output)...)
		out = append(out, parseJS(output, FrameworkAuto)...)
		out = append(out, parseCargo(output)...)
		out = append(out, parseJVM(output, FrameworkAuto)...)
	}
	return finishFailures(out)
}

// finishFailures trims messages, drops duplicates and caps the count.
func finishFailures(in []Failure) []Failure {
	seen := map[string]bool{}
	out := make([]Failure, 0, len(in))
	for _, f := range in {
		f.Message = trimMessage(f.Message, maxMessageLines)
		f.Test = strings.TrimSpace(f.Test)
		f.Suite = strings.TrimSpace(f.Suite)
		f.File = strings.TrimSpace(f.File)
		k := f.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
		if len(out) == maxParsedFailures {
			break
		}
	}
	return out
}

// relevance orders failures for the model: build errors block everything,
// then suite errors and timeouts, then located test failures, then the rest.
func relevance(f Failure) int {
	switch {
	case f.Kind == KindBuild:
		return 0
	case f.Kind == KindError && f.File != "":
		return 1
	case f.Kind == KindTimeout:
		return 2
	case f.Kind == KindTest && f.File != "" && f.Message != "":
		return 3
	case f.Kind == KindTest && (f.File != "" || f.Message != ""):
		return 4
	case f.Kind == KindError:
		return 5
	default:
		return 6
	}
}

// SortByRelevance returns the failures ordered most relevant first; ties
// keep their order of appearance.
func SortByRelevance(fs []Failure) []Failure {
	out := slices.Clone(fs)
	slices.SortStableFunc(out, func(a, b Failure) int { return relevance(a) - relevance(b) })
	return out
}

// FormatFailures renders failures for the model, most relevant first,
// within budget bytes. Failures that do not fit are counted, not shown.
func FormatFailures(fs []Failure, budget int) string {
	if len(fs) == 0 {
		return ""
	}
	if budget <= 0 {
		budget = DefaultFailureBudget
	}
	sorted := SortByRelevance(fs)
	var b strings.Builder
	shown := 0
	for i, f := range sorted {
		entry := formatFailure(i+1, f)
		if b.Len()+len(entry) > budget {
			// Always show at least one failure, cut to the budget.
			if shown == 0 {
				b.WriteString(clipBytes(entry, budget) + "\n")
				shown++
			}
			break
		}
		b.WriteString(entry)
		shown++
	}
	if rest := len(sorted) - shown; rest > 0 {
		fmt.Fprintf(&b, "… and %d more failure%s not shown.\n", rest, plural(rest))
	}
	return b.String()
}

func formatFailure(n int, f Failure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d. %s\n", n, f.Title())
	msg := f.Message
	if f.Kind == KindBuild && f.Test == "" {
		// The first line is already the title.
		_, rest, _ := strings.Cut(msg, "\n")
		msg = rest
	}
	for _, line := range strings.Split(trimMessage(msg, 12), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString("   " + clipRunes(line, 300) + "\n")
	}
	return b.String()
}

// DefaultFailureBudget is the default byte budget for failure summaries
// sent to the model.
const DefaultFailureBudget = 6000

// --- small text helpers shared by the parsers ---

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][A-Za-z0-9]|\x1b[=>]`)
)

// cleanOutput removes terminal escapes and resolves carriage-return
// overwrites so progress bars do not pollute parsing.
func cleanOutput(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.Contains(s, "\r") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if j := strings.LastIndexByte(strings.TrimRight(l, "\r"), '\r'); j >= 0 {
			lines[i] = l[j+1:]
		} else {
			lines[i] = strings.TrimRight(l, "\r")
		}
	}
	return strings.Join(lines, "\n")
}

// trimMessage trims blank edges and keeps at most n lines.
func trimMessage(s string, n int) string {
	s = strings.Trim(s, "\n")
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		extra := len(lines) - n
		lines = append(lines[:n], fmt.Sprintf("… (%d more line%s)", extra, plural(extra)))
	}
	return strings.Join(lines, "\n")
}

// dedent removes the common leading whitespace of the non-blank lines.
func dedent(lines []string) []string {
	common := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if common < 0 || n < common {
			common = n
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if len(l) >= common && common > 0 {
			out[i] = l[common:]
		} else {
			out[i] = strings.TrimLeft(l, " \t")
		}
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// clipBytes cuts s to at most n bytes without splitting a UTF-8 sequence.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// tailLines returns up to n trailing non-blank lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var keep []string
	for i := len(lines) - 1; i >= 0 && len(keep) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			keep = append(keep, lines[i])
		}
	}
	slices.Reverse(keep)
	return strings.Join(keep, "\n")
}
