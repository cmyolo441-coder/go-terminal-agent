package autopilot

import (
	"regexp"
	"strings"
)

var (
	mvnBlockRe     = regexp.MustCompile(`^\[ERROR\]\s+(Failures|Errors):\s*$`)
	mvnEntryRe     = regexp.MustCompile(`^\[ERROR\]\s{2,}(\S+?)(?::(\d+))?(?:->\S+)?\s+(.*)$`)
	mvnEndRe       = regexp.MustCompile(`^\[(INFO|WARNING)\]|^\[ERROR\]\s*$|^\[ERROR\] Tests run:`)
	mvnCompileRe   = regexp.MustCompile(`^\[ERROR\] (.+?\.(?:java|kt|scala|groovy)):\[(\d+),(\d+)\] (.+)$`)
	gradleFailRe   = regexp.MustCompile(`^(\S+) > (.+?) FAILED$`)
	gradleAtRe     = regexp.MustCompile(`^\s+(\S.*?)\s+at\s+([\w$.\-]+\.(?:java|kt|scala|groovy)):(\d+)\s*$`)
	javacErrRe     = regexp.MustCompile(`^((?:[A-Za-z]:)?[^\s:]+\.java):(\d+): error: (.+)$`)
	kotlincErrRe   = regexp.MustCompile(`^e: (?:file://)?((?:[A-Za-z]:)?[^\s:]+\.kts?):(\d+):(\d+)?:? (.+)$`)
	jvmClassMethod = regexp.MustCompile(`^(.+)\.([\w$]+)$`)
)

// parseJVM parses Maven Surefire and Gradle test output plus javac and
// kotlinc compile errors.
func parseJVM(out string, fw Framework) []Failure {
	if fw == FrameworkAuto {
		fw = FrameworkMaven
		if strings.Contains(out, "> Task :") || gradleFailRe.MatchString(firstMatchLine(out, " FAILED")) {
			fw = FrameworkGradle
		}
	}
	lines := strings.Split(out, "\n")
	var res []Failure
	inBlock := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if mvnBlockRe.MatchString(line) {
			inBlock = true
			continue
		}
		if inBlock {
			if m := mvnEntryRe.FindStringSubmatch(line); m != nil {
				f := Failure{Framework: fw, Kind: KindTest, Line: atoi(m[2]), Message: strings.TrimSpace(m[3])}
				if cm := jvmClassMethod.FindStringSubmatch(m[1]); cm != nil {
					f.Suite, f.Test = cm[1], cm[2]
					f.File = jvmFileOf(cm[1])
				} else {
					f.Test = m[1]
				}
				res = append(res, f)
				continue
			}
			if mvnEndRe.MatchString(line) {
				inBlock = false
			}
		}
		if m := mvnCompileRe.FindStringSubmatch(line); m != nil {
			res = append(res, Failure{Framework: fw, Kind: KindBuild, File: m[1], Line: atoi(m[2]), Message: m[4]})
			continue
		}
		if m := javacErrRe.FindStringSubmatch(line); m != nil {
			res = append(res, Failure{Framework: fw, Kind: KindBuild, File: m[1], Line: atoi(m[2]), Message: m[3]})
			continue
		}
		if m := kotlincErrRe.FindStringSubmatch(line); m != nil {
			res = append(res, Failure{Framework: fw, Kind: KindBuild, File: m[1], Line: atoi(m[2]), Message: m[4]})
			continue
		}
		if m := gradleFailRe.FindStringSubmatch(line); m != nil {
			f := Failure{Framework: fw, Kind: KindTest, Suite: m[1], Test: m[2], File: jvmFileOf(m[1])}
			var msg []string
			for j := i + 1; j < len(lines) && j <= i+12; j++ {
				l := lines[j]
				if strings.TrimSpace(l) == "" || !strings.HasPrefix(l, " ") {
					break
				}
				if am := gradleAtRe.FindStringSubmatch(l); am != nil {
					if f.Line == 0 {
						f.File, f.Line = am[2], atoi(am[3])
					}
					msg = append(msg, am[1])
					continue
				}
				msg = append(msg, strings.TrimSpace(l))
			}
			f.Message = strings.Join(msg, "\n")
			res = append(res, f)
		}
	}
	return res
}

// jvmFileOf guesses the source file name of a fully qualified class.
func jvmFileOf(class string) string {
	simple := class
	if i := strings.LastIndexByte(simple, '.'); i >= 0 {
		simple = simple[i+1:]
	}
	if i := strings.IndexByte(simple, '$'); i >= 0 {
		simple = simple[:i]
	}
	if simple == "" {
		return ""
	}
	return simple + ".java"
}

func firstMatchLine(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}
