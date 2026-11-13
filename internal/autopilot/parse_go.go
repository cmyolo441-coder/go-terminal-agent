package autopilot

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	goFailRe     = regexp.MustCompile(`^(\s*)--- FAIL: (\S+) \(([\d.]+m?s)\)`)
	goEndRe      = regexp.MustCompile(`^\s*(--- (PASS|SKIP|FAIL):|=== (RUN|PAUSE|CONT|NAME)\s)`)
	goRunRe      = regexp.MustCompile(`^=== (?:RUN|CONT|NAME)\s+(\S+)`)
	goPkgFailRe  = regexp.MustCompile(`^FAIL\s+(\S+)\s+(?:[\d.]+s|\[(build failed|setup failed)\]|\(cached\))`)
	goPkgOkRe    = regexp.MustCompile(`^ok\s+(\S+)\s`)
	goLocRe      = regexp.MustCompile(`^([\w.\-/\\]+\.go):(\d+): ?(.*)$`)
	goBuildHdrRe = regexp.MustCompile(`^# (\S+)`)
	goBuildErrRe = regexp.MustCompile(`^((?:\.{1,2}/)?[\w.\-/\\]+\.go):(\d+)(?::(\d+))?: (.+)$`)
	goPanicRe    = regexp.MustCompile(`^panic: (.+)$`)
	goTimeoutRe  = regexp.MustCompile(`^panic: test timed out after (\S+)`)
	goFrameRe    = regexp.MustCompile(`^\t(.+\.go):(\d+)(?: \+0x[0-9a-f]+)?$`)
	goRunningRe  = regexp.MustCompile(`^\t+(Test\S*|Example\S*|Fuzz\S*) \(`)
)

// parseGo parses `go test` output, plain or -json.
func parseGo(out string) []Failure {
	if plain, ok := goJSONToPlain(out); ok {
		out = plain
	}
	p := &goParser{}
	for _, line := range strings.Split(out, "\n") {
		p.line(line)
	}
	p.flushPanic()
	p.closeTest()
	return p.finish()
}

// goJSONToPlain converts `go test -json` events back into plain output.
// It reports false when the output is not a JSON event stream.
func goJSONToPlain(out string) (string, bool) {
	var b strings.Builder
	events := 0
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "{") {
			if t != "" {
				b.WriteString(line + "\n")
			}
			continue
		}
		var ev struct {
			Action string
			Output string
		}
		if json.Unmarshal([]byte(t), &ev) != nil || ev.Action == "" {
			b.WriteString(line + "\n")
			continue
		}
		events++
		if ev.Action == "output" || ev.Action == "build-output" {
			b.WriteString(ev.Output)
			if !strings.HasSuffix(ev.Output, "\n") {
				b.WriteString("\n")
			}
		}
	}
	return b.String(), events > 0
}

type goParser struct {
	failures []Failure
	pending  []int // failures whose package is not known yet
	cur      int   // index of the open test failure, or -1 when none
	curInd   int   // indentation of the open failure's header
	open     bool
	buildPkg string
	lastRun  string
	pkgLines []string // recent lines of the current package, for unattributed failures

	// pkgFailures counts failures recorded since the last package result.
	pkgFailures int
	// buildErrs marks packages that already have parsed build errors.
	buildErrs map[string]bool
	// runOut holds verbose (-v, -json) output per test: there, a test's
	// log lines print before its "--- FAIL" line.
	runOut  map[string][]string
	runName string

	inPanic    bool
	panicIdx   int
	panicFound bool
	timeout    bool
}

func (p *goParser) add(f Failure) int {
	f.Framework = FrameworkGo
	p.failures = append(p.failures, f)
	idx := len(p.failures) - 1
	p.pkgFailures++
	if f.Suite == "" {
		p.pending = append(p.pending, idx)
	}
	return idx
}

func (p *goParser) closeTest() { p.open = false }

func (p *goParser) line(line string) {
	if p.inPanic {
		if p.panicLine(line) {
			return
		}
	}
	if m := goPkgFailRe.FindStringSubmatch(line); m != nil {
		p.flushPanic()
		p.closeTest()
		p.packageDone(m[1], m[2] != "")
		return
	}
	if m := goPkgOkRe.FindStringSubmatch(line); m != nil {
		p.closeTest()
		p.pkgLines = nil
		p.pending = nil
		p.pkgFailures = 0
		p.runName = ""
		p.runOut = nil
		return
	}
	p.remember(line)
	if m := goBuildHdrRe.FindStringSubmatch(line); m != nil {
		p.closeTest()
		p.buildPkg = m[1]
		return
	}
	if m := goTimeoutRe.FindStringSubmatch(line); m != nil {
		p.closeTest()
		p.inPanic, p.timeout, p.panicFound = true, true, false
		p.panicIdx = -1
		return
	}
	if m := goPanicRe.FindStringSubmatch(line); m != nil {
		idx := -1
		if p.open && p.cur >= 0 {
			idx = p.cur
		} else if p.lastRun != "" {
			idx = p.add(Failure{Kind: KindTest, Test: p.lastRun})
		} else {
			idx = p.add(Failure{Kind: KindError})
		}
		f := &p.failures[idx]
		if f.Message == "" {
			f.Message = "panic: " + m[1]
		} else {
			f.Message += "\npanic: " + m[1]
		}
		p.closeTest()
		p.inPanic, p.timeout, p.panicFound = true, false, false
		p.panicIdx = idx
		return
	}
	if m := goFailRe.FindStringSubmatch(line); m != nil {
		p.cur = p.add(Failure{Kind: KindTest, Test: m[2]})
		p.curInd = len(m[1])
		p.open = true
		p.prefill(m[2])
		return
	}
	if m := goRunRe.FindStringSubmatch(line); m != nil {
		p.lastRun = m[1]
		p.runName = m[1]
	}
	if goEndRe.MatchString(line) {
		p.closeTest()
		return
	}
	if p.open && p.testLine(line) {
		return
	}
	if p.runName != "" && strings.HasPrefix(line, " ") && strings.TrimSpace(line) != "" {
		if p.runOut == nil {
			p.runOut = map[string][]string{}
		}
		if len(p.runOut[p.runName]) < maxMessageLines {
			p.runOut[p.runName] = append(p.runOut[p.runName], line)
		}
		return
	}
	if m := goBuildErrRe.FindStringSubmatch(line); m != nil {
		if p.buildErrs == nil {
			p.buildErrs = map[string]bool{}
		}
		p.buildErrs[p.buildPkg] = true
		p.add(Failure{Kind: KindBuild, Suite: p.buildPkg, File: strings.TrimPrefix(m[1], "./"), Line: atoi(m[2]), Message: m[4]})
		return
	}
}

// prefill fills a new failure from the verbose output its test logged.
func (p *goParser) prefill(name string) {
	lines := p.runOut[name]
	if len(lines) == 0 {
		return
	}
	delete(p.runOut, name)
	f := &p.failures[p.cur]
	var msg []string
	for _, l := range dedent(lines) {
		body := strings.TrimSpace(l)
		if m := goLocRe.FindStringSubmatch(body); m != nil && !strings.HasPrefix(l, " ") {
			if f.File == "" {
				f.File, f.Line = m[1], atoi(m[2])
			}
			body = m[3]
		}
		msg = append(msg, body)
	}
	f.Message = strings.Join(msg, "\n")
}

// testLine handles output that belongs to the open failing test. It
// reports false when the line does not belong to it.
func (p *goParser) testLine(line string) bool {
	ind := len(line) - len(strings.TrimLeft(line, " \t"))
	if strings.TrimSpace(line) == "" {
		return true
	}
	if ind <= p.curInd {
		// Output at or left of the header no longer belongs to the test.
		p.closeTest()
		return false
	}
	f := &p.failures[p.cur]
	body := strings.TrimSpace(line)
	if m := goLocRe.FindStringSubmatch(body); m != nil && ind <= p.curInd+4 {
		if f.File == "" {
			f.File, f.Line = m[1], atoi(m[2])
		}
		body = m[3]
	}
	if f.Message == "" {
		f.Message = body
	} else {
		f.Message += "\n" + body
	}
	return true
}

// panicLine consumes goroutine dump lines after a panic. It reports false
// when the line ends the dump and should be parsed normally.
func (p *goParser) panicLine(line string) bool {
	if strings.HasPrefix(line, "FAIL") || strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "exit status") {
		p.flushPanic()
		return strings.HasPrefix(line, "exit status")
	}
	if p.timeout {
		if m := goRunningRe.FindStringSubmatch(line); m != nil {
			p.add(Failure{Kind: KindTest, Test: m[1], Message: "test timed out (the test binary's -timeout was exceeded); it may hang or deadlock"})
			p.panicFound = true
		}
		return true
	}
	if m := goFrameRe.FindStringSubmatch(line); m != nil && !p.panicFound && p.panicIdx >= 0 {
		path := m[1]
		if !isGoRuntimeFrame(path) {
			f := &p.failures[p.panicIdx]
			if f.File == "" || f.Line == 0 {
				f.File, f.Line = path, atoi(m[2])
			}
			p.panicFound = true
		}
	}
	return true
}

func (p *goParser) flushPanic() {
	if p.inPanic && p.timeout && !p.panicFound {
		p.add(Failure{Kind: KindTimeout, Message: "the Go test binary timed out"})
	}
	p.inPanic, p.timeout = false, false
}

func isGoRuntimeFrame(path string) bool {
	for _, s := range []string{"/src/runtime/", "/src/testing/", "/src/reflect/", "/src/internal/", "/src/sync/", "<autogenerated>"} {
		if strings.Contains(path, s) {
			return true
		}
	}
	return false
}

// remember keeps the recent lines of the current package for failures that
// no test claims (TestMain exits, init panics, missing test binaries).
func (p *goParser) remember(line string) {
	if strings.TrimSpace(line) == "" || line == "FAIL" || strings.HasPrefix(line, "=== ") || strings.HasPrefix(line, "--- PASS") {
		return
	}
	p.pkgLines = append(p.pkgLines, line)
	if len(p.pkgLines) > 40 {
		p.pkgLines = p.pkgLines[len(p.pkgLines)-40:]
	}
}

// packageDone assigns the package to pending failures, or records an
// unattributed package failure when none are pending.
func (p *goParser) packageDone(pkg string, build bool) {
	if build && p.buildErrs[pkg] {
		// Its build errors were already parsed (they print before this line).
	} else if p.pkgFailures == 0 {
		msg := "package failed without a failing test (TestMain, init or os.Exit)"
		if build {
			msg = "package failed to build"
		}
		if tail := strings.Join(p.pkgLines, "\n"); strings.TrimSpace(tail) != "" {
			msg += "\n" + tailLines(tail, 15)
		}
		kind := KindError
		if build {
			kind = KindBuild
		}
		p.failures = append(p.failures, Failure{Framework: FrameworkGo, Kind: kind, Suite: pkg, Message: msg})
	}
	for _, i := range p.pending {
		if p.failures[i].Suite == "" {
			p.failures[i].Suite = pkg
		}
	}
	p.pending = nil
	p.pkgLines = nil
	p.pkgFailures = 0
	p.buildPkg = ""
	p.lastRun = ""
	p.runName = ""
	p.runOut = nil
}

// finish drops parent tests that failed only because a subtest failed.
func (p *goParser) finish() []Failure {
	var out []Failure
	for i, f := range p.failures {
		if f.Kind == KindTest && f.Message == "" && f.File == "" && p.hasSubtest(i) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func (p *goParser) hasSubtest(i int) bool {
	parent := p.failures[i]
	for j, f := range p.failures {
		if j != i && f.Suite == parent.Suite && strings.HasPrefix(f.Test, parent.Test+"/") {
			return true
		}
	}
	return false
}
