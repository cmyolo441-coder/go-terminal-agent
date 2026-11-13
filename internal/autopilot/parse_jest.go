package autopilot

import (
	"regexp"
	"strings"
)

var (
	jsFailRe     = regexp.MustCompile(`^\s*FAIL\s+(.+?)\s*$`)
	jsPassRe     = regexp.MustCompile(`^\s*PASS\s+\S`)
	jsBulletRe   = regexp.MustCompile(`^\s*● (.+)$`)
	jsAtRe       = regexp.MustCompile(`^\s*at .*?\(?((?:[A-Za-z]:)?[^\s():]+\.[cm]?[jt]sx?):(\d+):(\d+)\)?\s*$`)
	jsFrameRe    = regexp.MustCompile(`^\s*>\s*(\d+)\s*\|`)
	jsCodeRe     = regexp.MustCompile(`^\s*>?\s*\d+\s*\|`)
	jsGutterRe   = regexp.MustCompile(`^\s*\|`)
	jsSummaryRe  = regexp.MustCompile(`^\s*(Tests?|Test Suites|Test Files|Snapshots|Time|Ran all|Duration|Start at):?\s`)
	vitArrowRe   = regexp.MustCompile(`^\s*❯ ((?:[A-Za-z]:)?[^\s:]+):(\d+):(\d+)`)
	vitFileRe    = regexp.MustCompile(`^\s*❯ (\S+) \(\d+ tests?`)
	vitCrossRe   = regexp.MustCompile(`^\s*[×✗✕] (.+?)(?: \d+m?s)?\s*$`)
	vitSepRe     = regexp.MustCompile(`^\s*⎯+`)
	vitSuiteErrR = regexp.MustCompile(`^(.+?) \[ (.+?) \]$`)
)

// parseJS parses Jest and Vitest default-reporter output.
func parseJS(out string, fw Framework) []Failure {
	lines := strings.Split(out, "\n")
	var (
		res     []Failure
		curFile string
		cur     *Failure
		body    []string
		crosses []Failure
		vitFile string
		fwOf    = func(vitest bool) Framework {
			if fw == FrameworkJest || fw == FrameworkVitest {
				return fw
			}
			if vitest {
				return FrameworkVitest
			}
			return FrameworkJest
		}
	)
	flush := func() {
		if cur == nil {
			return
		}
		jsFillBody(cur, body, curFile)
		res = append(res, *cur)
		cur, body = nil, nil
	}
	for _, line := range lines {
		if m := jsFailRe.FindStringSubmatch(line); m != nil {
			flush()
			target := m[1]
			if file, test, ok := strings.Cut(target, " > "); ok {
				// Vitest: "FAIL  src/a.test.ts > suite > test".
				cur = &Failure{Framework: fwOf(true), Kind: KindTest, Suite: file, File: file, Test: test}
				continue
			}
			if sm := vitSuiteErrR.FindStringSubmatch(target); sm != nil {
				// Vitest suite error: "FAIL  src/a.test.ts [ src/a.test.ts ]".
				cur = &Failure{Framework: fwOf(true), Kind: KindError, Suite: sm[1], File: sm[1]}
				continue
			}
			curFile = target
			continue
		}
		if jsPassRe.MatchString(line) || jsSummaryRe.MatchString(line) || vitSepRe.MatchString(line) {
			flush()
			if jsPassRe.MatchString(line) {
				curFile = ""
			}
			continue
		}
		if m := jsBulletRe.FindStringSubmatch(line); m != nil && curFile != "" {
			flush()
			name := strings.TrimSpace(m[1])
			f := Failure{Framework: fwOf(false), Kind: KindTest, Suite: curFile, File: curFile, Test: name}
			if name == "Test suite failed to run" {
				f.Kind, f.Test = KindError, ""
			}
			cur = &f
			continue
		}
		if m := vitFileRe.FindStringSubmatch(line); m != nil && cur == nil {
			vitFile = m[1]
			continue
		}
		if m := vitCrossRe.FindStringSubmatch(line); m != nil && cur == nil && vitFile != "" {
			// Listing line "× suite > test 8ms" under "❯ file (N tests | M failed)".
			crosses = append(crosses, Failure{Framework: fwOf(true), Kind: KindTest, Suite: vitFile, File: vitFile, Test: m[1]})
			continue
		}
		if cur != nil {
			body = append(body, line)
		}
	}
	flush()
	if len(res) == 0 {
		return crosses
	}
	return res
}

// jsFillBody extracts the message and location from a failure's body.
func jsFillBody(f *Failure, body []string, file string) {
	var msg []string
	msgDone := false
	located := false
	for _, l := range body {
		if m := vitArrowRe.FindStringSubmatch(l); m != nil {
			if !located {
				f.File, f.Line = m[1], atoi(m[2])
				located = true
			}
			msgDone = true
			continue
		}
		if m := jsAtRe.FindStringSubmatch(l); m != nil {
			path := m[1]
			if !located && !strings.Contains(path, "node_modules") && (file == "" || sameJSFile(path, file)) {
				f.File, f.Line = path, atoi(m[2])
				located = true
			}
			msgDone = true
			continue
		}
		if m := jsFrameRe.FindStringSubmatch(l); m != nil && !located && f.Line == 0 {
			// The ">" line of Jest's code frame is the failing line.
			f.Line = atoi(m[1])
		}
		if jsCodeRe.MatchString(l) || jsGutterRe.MatchString(l) {
			msgDone = true
			continue
		}
		if !msgDone {
			msg = append(msg, l)
		}
	}
	f.Message = strings.Join(dedent(msg), "\n")
}

func sameJSFile(a, b string) bool {
	a = strings.ReplaceAll(a, "\\", "/")
	b = strings.ReplaceAll(b, "\\", "/")
	return a == b || strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
}
