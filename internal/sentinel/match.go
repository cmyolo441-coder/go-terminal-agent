package sentinel

import (
	"bytes"
	"regexp"
	"strings"
)

// Input is what a Matcher sees for one event.
type Input struct {
	Kind  Kind
	Tool  string
	Flags EventFlags
	// Target is the event's raw target.
	Target string
	// Lower is Target with ASCII letters lowercased and backslashes turned
	// into slashes; Lower[i:j] corresponds to Target[i:j]. It is reused
	// between events: matchers must not retain it.
	Lower []byte

	sh     shellLine
	lexed  bool
	isFile bool
}

func (in *Input) reset(kind Kind, tool, target string, flags EventFlags, lower []byte) {
	in.Kind, in.Tool, in.Target, in.Flags, in.Lower = kind, tool, target, flags, lower
	in.lexed = false
	in.isFile = kind == KindFileWrite || kind == KindFileRead || kind == KindFileDelete
}

// shell returns the lexed command line, lexing on first use.
func (in *Input) shell() *shellLine {
	if !in.lexed {
		in.sh.lex(in.Lower)
		in.lexed = true
	}
	return &in.sh
}

// Matcher decides whether an event matches one step of a Pattern.
type Matcher interface {
	Match(in *Input) bool
}

// MatchFunc adapts a function to Matcher.
type MatchFunc func(in *Input) bool

// Match calls f.
func (f MatchFunc) Match(in *Input) bool { return f(in) }

func lowerAll(ss []string) [][]byte {
	out := make([][]byte, len(ss))
	for i, s := range ss {
		out[i] = []byte(strings.ToLower(strings.ReplaceAll(s, `\`, "/")))
	}
	return out
}

// Contains matches when the target contains any of subs (case-insensitive).
func Contains(subs ...string) Matcher {
	ls := lowerAll(subs)
	return MatchFunc(func(in *Input) bool {
		for _, s := range ls {
			if bytes.Contains(in.Lower, s) {
				return true
			}
		}
		return false
	})
}

// CmdWord matches when any pipeline segment of a command line runs one of
// the named commands (by basename, through sudo/env/xargs and the like).
func CmdWord(names ...string) Matcher {
	ls := make([]string, len(names))
	for i, n := range names {
		ls[i] = strings.ToLower(n)
	}
	return MatchFunc(func(in *Input) bool {
		sh := in.shell()
		return sh.segments(func(seg segment) bool { return sh.cmdIs(seg, ls...) })
	})
}

// Word matches when any word of the target equals one of words.
func Word(words ...string) Matcher {
	ls := lowerAll(words)
	return MatchFunc(func(in *Input) bool {
		return in.shell().anyWord(func(w []byte) bool {
			for _, s := range ls {
				if bytes.Equal(w, s) {
					return true
				}
			}
			return false
		})
	})
}

// All matches when every matcher does.
func All(ms ...Matcher) Matcher {
	return MatchFunc(func(in *Input) bool {
		for _, m := range ms {
			if !m.Match(in) {
				return false
			}
		}
		return true
	})
}

// Any matches when at least one matcher does.
func Any(ms ...Matcher) Matcher {
	return MatchFunc(func(in *Input) bool {
		for _, m := range ms {
			if m.Match(in) {
				return true
			}
		}
		return false
	})
}

// Not inverts m.
func Not(m Matcher) Matcher { return MatchFunc(func(in *Input) bool { return !m.Match(in) }) }

// Regexp matches the lowered target against re.
func Regexp(re *regexp.Regexp) Matcher {
	return MatchFunc(func(in *Input) bool { return re.Match(in.Lower) })
}

// PathPredicate tests a lowered, slash-separated path.
type PathPredicate func(p []byte) bool

// PathUnder matches paths with a directory component sequence, e.g.
// ".ssh" or ".github/workflows": the path starts with it or contains it
// between slashes.
func PathUnder(dirs ...string) PathPredicate {
	type pair struct{ lead, mid []byte }
	ps := make([]pair, len(dirs))
	for i, d := range dirs {
		d = strings.Trim(strings.ToLower(strings.ReplaceAll(d, `\`, "/")), "/")
		ps[i] = pair{lead: []byte(d + "/"), mid: []byte("/" + d + "/")}
	}
	return func(p []byte) bool {
		for _, x := range ps {
			if bytes.HasPrefix(p, x.lead) || bytes.Contains(p, x.mid) {
				return true
			}
		}
		return false
	}
}

// BaseIn matches paths whose basename is one of names.
func BaseIn(names ...string) PathPredicate {
	ls := lowerAll(names)
	return func(p []byte) bool {
		b := baseOf(p)
		for _, n := range ls {
			if bytes.Equal(b, n) {
				return true
			}
		}
		return false
	}
}

// BasePrefix matches paths whose basename starts with one of prefixes.
func BasePrefix(prefixes ...string) PathPredicate {
	ls := lowerAll(prefixes)
	return func(p []byte) bool {
		b := baseOf(p)
		for _, n := range ls {
			if bytes.HasPrefix(b, n) {
				return true
			}
		}
		return false
	}
}

// HasSuffix matches paths ending in one of suffixes.
func HasSuffix(suffixes ...string) PathPredicate {
	ls := lowerAll(suffixes)
	return func(p []byte) bool {
		for _, n := range ls {
			if bytes.HasSuffix(p, n) {
				return true
			}
		}
		return false
	}
}

// AnyPath matches when one of preds does.
func AnyPath(preds ...PathPredicate) PathPredicate {
	return func(p []byte) bool {
		for _, f := range preds {
			if f(p) {
				return true
			}
		}
		return false
	}
}

// ExceptPath matches p when pred does and none of except do.
func ExceptPath(pred PathPredicate, except ...PathPredicate) PathPredicate {
	return func(p []byte) bool {
		if !pred(p) {
			return false
		}
		for _, f := range except {
			if f(p) {
				return false
			}
		}
		return true
	}
}

// Touches matches a file event whose path satisfies pred, or a command or
// generic tool call any of whose words does.
func Touches(pred PathPredicate) Matcher {
	return MatchFunc(func(in *Input) bool {
		if in.isFile {
			return pred(in.Lower)
		}
		return in.shell().anyWord(func(w []byte) bool { return pred(w) || pred(fileOperand(w)) })
	})
}

// fileOperand strips the option syntax around a file name in a word:
// --data-binary=@.env, -d@secrets.json, -F f=@id_rsa, @file → the file
// name.
func fileOperand(w []byte) []byte {
	if i := bytes.LastIndexByte(w, '@'); i >= 0 {
		return w[i+1:] // -d@f, --data=@f, -F name=@f, @f
	}
	if i := bytes.IndexByte(w, '='); i >= 0 && w[0] == '-' {
		return w[i+1:] // --upload-file=f
	}
	return w
}

// Writes matches a file write or delete whose path satisfies pred, or a
// command that writes to such a path (redirection, tee, cp, mv, sed -i,
// chmod, ...).
func Writes(pred PathPredicate) Matcher {
	return MatchFunc(func(in *Input) bool {
		switch in.Kind {
		case KindFileWrite, KindFileDelete:
			return pred(in.Lower)
		case KindExec, KindToolCall:
			return in.shell().writeTargets(func(p []byte) bool { return pred(p) })
		}
		return false
	})
}

// ShellRule matches commands with one of the lexer's built-in analyses.
type ShellRule uint8

// Built-in shell analyses.
const (
	// ShellPipeToShell is a download piped or substituted into an interpreter.
	ShellPipeToShell ShellRule = iota + 1
	// ShellCatastrophicDelete is a recursive delete of a root-like path, a
	// find -delete from a root, or a disk-level overwrite.
	ShellCatastrophicDelete
	// ShellDownload is a command that saves a download to a file.
	ShellDownload
)

// Shell matches commands for which the rule holds.
func Shell(rule ShellRule) Matcher {
	return MatchFunc(func(in *Input) bool {
		if in.isFile {
			return false
		}
		sh := in.shell()
		switch rule {
		case ShellPipeToShell:
			return sh.pipeToShell()
		case ShellCatastrophicDelete:
			return sh.catastrophicDelete()
		case ShellDownload:
			_, _, ok := sh.downloadTarget()
			return ok
		}
		return false
	})
}

// Capturer extracts a value from an event for later steps to bind to. The
// returned string must be a substring of in.Target (never of in.Lower).
type Capturer func(in *Input) (string, bool)

// CaptureDownload captures the basename of the file a download command
// saves, e.g. "install.sh" from `curl -fsSL -o /tmp/install.sh https://…`.
func CaptureDownload(in *Input) (string, bool) {
	s, e, ok := in.shell().downloadTarget()
	if !ok {
		return "", false
	}
	return in.Target[s:e], true
}

// CapturePathBase captures the basename of a file event's path.
func CapturePathBase(in *Input) (string, bool) {
	if !in.isFile {
		return "", false
	}
	b := baseOf(in.Lower)
	s := len(in.Lower) - len(b)
	if len(b) == 0 {
		return "", false
	}
	return in.Target[s : s+len(b)], true
}

// containsFold reports whether lower (already lowercased) contains needle
// under ASCII case folding.
func containsFold(lower []byte, needle string) bool {
	n := len(needle)
	if n == 0 {
		return true
	}
	first := foldByte(needle[0])
	for i := 0; i+n <= len(lower); i++ {
		if lower[i] != first {
			continue
		}
		j := 1
		for j < n && lower[i+j] == foldByte(needle[j]) {
			j++
		}
		if j == n {
			return true
		}
	}
	return false
}

func foldByte(c byte) byte {
	switch {
	case 'A' <= c && c <= 'Z':
		return c + ('a' - 'A')
	case c == '\\':
		return '/'
	}
	return c
}
