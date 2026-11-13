package autopilot

import (
	"regexp"
	"strings"
)

var (
	cargoTestRe    = regexp.MustCompile(`^test (\S+) \.\.\. FAILED$`)
	cargoSectionRe = regexp.MustCompile(`^---- (\S+) (?:stdout|stderr) ----$`)
	cargoPanicRe   = regexp.MustCompile(`^thread '([^']*)' (?:\(\d+\) )?panicked at (?:'(.*)', )?((?:[A-Za-z]:)?[^\s:]+\.rs):(\d+):(\d+):?$`)
	cargoErrRe     = regexp.MustCompile(`^error(?:\[(E\d+)\])?: (.+)$`)
	cargoArrowRe   = regexp.MustCompile(`^\s*--> ((?:[A-Za-z]:)?[^\s:]+):(\d+):(\d+)$`)
	cargoRunningRe = regexp.MustCompile(`^\s*Running (?:unittests |tests? )?(\S+)`)
	cargoEndRe     = regexp.MustCompile(`^(failures:|test result:|---- )`)
)

// parseCargo parses `cargo test` output: compile errors, failing tests and
// their panic messages.
func parseCargo(out string) []Failure {
	lines := strings.Split(out, "\n")
	var res []Failure
	index := map[string]int{}
	suite := ""
	get := func(name string) *Failure {
		if i, ok := index[name]; ok {
			return &res[i]
		}
		res = append(res, Failure{Framework: FrameworkCargo, Kind: KindTest, Suite: suite, Test: name})
		index[name] = len(res) - 1
		return &res[len(res)-1]
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := cargoRunningRe.FindStringSubmatch(line); m != nil {
			suite = m[1]
			continue
		}
		if m := cargoTestRe.FindStringSubmatch(line); m != nil {
			get(m[1])
			continue
		}
		if m := cargoSectionRe.FindStringSubmatch(line); m != nil {
			f := get(m[1])
			var body []string
			j := i + 1
			for ; j < len(lines); j++ {
				if cargoEndRe.MatchString(lines[j]) {
					break
				}
				body = append(body, lines[j])
			}
			cargoFillSection(f, body)
			i = j - 1
			continue
		}
		if m := cargoErrRe.FindStringSubmatch(line); m != nil {
			// A compile error is only reported when it has a location;
			// summaries like "could not compile" do not.
			for j := i + 1; j < len(lines) && j <= i+4; j++ {
				if cargoErrRe.MatchString(lines[j]) {
					break
				}
				if a := cargoArrowRe.FindStringSubmatch(lines[j]); a != nil {
					msg := m[2]
					if m[1] != "" {
						msg = m[1] + ": " + msg
					}
					res = append(res, Failure{Framework: FrameworkCargo, Kind: KindBuild, File: a[1], Line: atoi(a[2]), Message: msg})
					break
				}
			}
		}
	}
	return res
}

// cargoFillSection reads a "---- name stdout ----" section.
func cargoFillSection(f *Failure, body []string) {
	var msg []string
	inMsg := false
	for _, l := range body {
		if m := cargoPanicRe.FindStringSubmatch(l); m != nil {
			f.File, f.Line = m[3], atoi(m[4])
			if m[2] != "" {
				msg = append(msg, m[2])
				inMsg = false
			} else {
				inMsg = true
			}
			continue
		}
		if inMsg {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "note:") || strings.HasPrefix(t, "stack backtrace:") {
				inMsg = false
				continue
			}
			msg = append(msg, l)
		}
	}
	if len(msg) == 0 {
		msg = []string{tailLines(strings.Join(body, "\n"), 5)}
	}
	f.Message = strings.Join(msg, "\n")
}
