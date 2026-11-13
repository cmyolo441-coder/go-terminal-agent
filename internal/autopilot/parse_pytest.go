package autopilot

import (
	"regexp"
	"strings"
)

var (
	pyBannerRe  = regexp.MustCompile(`^={3,} ?(.*?) ?={3,}$`)
	pySectionRe = regexp.MustCompile(`^_{3,} (.+?) _{3,}$`)
	pyLocRe     = regexp.MustCompile(`^([\w.\-/\\]+\.py):(\d+):(?: (.*))?$`)
	pySummaryRe = regexp.MustCompile(`^(FAILED|ERROR) (.+?)(?: - (.*))?$`)
)

type pySection struct {
	name  string
	lines []string
}

// parsePytest parses pytest output: the FAILURES and ERRORS sections for
// locations and assertion text, and the short test summary for node ids.
func parsePytest(out string) []Failure {
	var (
		sections []pySection
		cur      *pySection
		inBlock  bool // inside a FAILURES or ERRORS banner
		summary  []Failure
	)
	flush := func() {
		if cur != nil {
			sections = append(sections, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if m := pyBannerRe.FindStringSubmatch(line); m != nil {
			flush()
			title := strings.ToUpper(strings.TrimSpace(m[1]))
			inBlock = title == "FAILURES" || title == "ERRORS"
			continue
		}
		if m := pySummaryRe.FindStringSubmatch(line); m != nil && !inBlock {
			summary = append(summary, pySummaryFailure(m[1], m[2], m[3]))
			continue
		}
		if !inBlock {
			continue
		}
		if m := pySectionRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &pySection{name: m[1]}
			continue
		}
		if cur != nil {
			cur.lines = append(cur.lines, line)
		}
	}
	flush()

	var out2 []Failure
	byName := map[string]int{}
	for _, s := range sections {
		f := pySectionFailure(s)
		key := f.Test
		if key == "" {
			key = "collecting " + f.File
		}
		byName[key] = len(out2)
		out2 = append(out2, f)
	}
	for _, sf := range summary {
		key := sf.Test
		if sf.Kind == KindError && sf.Test == "" {
			key = "collecting " + sf.File
		}
		if i, ok := matchPySection(byName, key); ok {
			f := &out2[i]
			f.Kind = sf.Kind
			f.Test = sf.Test
			f.Suite = sf.Suite
			if f.File == "" {
				f.File = sf.File
			}
			if f.Message == "" {
				f.Message = sf.Message
			}
			continue
		}
		out2 = append(out2, sf)
	}
	for i := range out2 {
		out2[i].Framework = FrameworkPytest
	}
	return out2
}

// pySummaryFailure turns "FAILED tests/test_x.py::TestC::test_y[1] - msg".
func pySummaryFailure(kind, nodeid, msg string) Failure {
	f := Failure{Kind: KindTest, Message: strings.TrimSpace(msg)}
	if kind == "ERROR" {
		f.Kind = KindError
	}
	file, rest, hasTest := strings.Cut(nodeid, "::")
	f.File = file
	f.Suite = file
	if hasTest {
		f.Test = strings.ReplaceAll(rest, "::", ".")
	}
	return f
}

// pySectionFailure extracts a failure from one "____ name ____" section.
func pySectionFailure(s pySection) Failure {
	f := Failure{Kind: KindTest, Test: s.name}
	switch {
	case strings.HasPrefix(s.name, "ERROR collecting "):
		f.Kind = KindError
		f.File = strings.TrimSpace(strings.TrimPrefix(s.name, "ERROR collecting "))
		f.Test = ""
	case strings.HasPrefix(s.name, "ERROR at setup of "), strings.HasPrefix(s.name, "ERROR at teardown of "):
		f.Kind = KindError
		f.Test = s.name[strings.LastIndex(s.name, " of ")+4:]
	}
	var errs []string
	for _, l := range s.lines {
		if m := pyLocRe.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			// The last location is the innermost frame: where it failed.
			f.File, f.Line = m[1], atoi(m[2])
		}
		if strings.HasPrefix(l, "E ") || l == "E" {
			errs = append(errs, strings.TrimPrefix(strings.TrimPrefix(l, "E"), " "))
		}
	}
	if len(errs) > 0 {
		f.Message = strings.Join(dedent(errs), "\n")
	} else {
		f.Message = tailLines(strings.Join(s.lines, "\n"), 5)
	}
	if f.Kind == KindTest {
		f.Suite = f.File
	}
	return f
}

// matchPySection finds the section for a summary test name. Sections are
// named "test_y" or "TestC.test_y"; summary names are "TestC.test_y[1]".
func matchPySection(byName map[string]int, name string) (int, bool) {
	if i, ok := byName[name]; ok {
		return i, true
	}
	for n, i := range byName {
		if strings.HasSuffix(name, "."+n) || strings.HasSuffix(n, "."+name) {
			return i, true
		}
	}
	return 0, false
}
