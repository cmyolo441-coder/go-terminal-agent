package sentinel

import "bytes"

// This file is a deliberately small, allocation-free shell lexer. It is not
// a POSIX parser: it splits a command line into words and operators, honours
// quotes well enough that `echo "a; b"` is one command, and identifies the
// command word of each pipeline segment. Detection rules built on it err on
// the side of matching; the cost of a false positive is a prompt, not a
// broken command.

const maxSpans = 96

type spanKind uint8

const (
	spanWord     spanKind = iota
	spanSep               // ; & && || ( ) newline: ends a command
	spanPipe              // |
	spanRedirOut          // > >> &> >|
	spanRedirIn           // < <<
)

type span struct {
	s, e int32 // word bounds in the lowered input, quotes stripped
	kind spanKind
}

const maxSegs = 32

type shellLine struct {
	b        []byte
	spans    [maxSpans]span
	n        int
	segs     [maxSegs]segment
	nseg     int
	segsDone bool
}

// lex splits b into spans. Words beyond maxSpans are dropped.
func (l *shellLine) lex(b []byte) {
	l.b = b
	l.n = 0
	l.nseg = 0
	l.segsDone = false
	i := 0
	for i < len(b) && l.n < maxSpans {
		c := b[i]
		switch {
		case isSpace(c) && c != '\n':
			i++
		case c == '\n' || c == ';' || c == '(' || c == ')':
			l.push(i, i+1, spanSep)
			i++
		case c == '&':
			switch {
			case i+1 < len(b) && b[i+1] == '&':
				l.push(i, i+2, spanSep)
				i += 2
			case i+1 < len(b) && b[i+1] == '>':
				j := i + 2
				if j < len(b) && b[j] == '>' {
					j++
				}
				l.push(i, j, spanRedirOut)
				i = j
			default:
				l.push(i, i+1, spanSep)
				i++
			}
		case c == '|':
			if i+1 < len(b) && b[i+1] == '|' {
				l.push(i, i+2, spanSep)
				i += 2
			} else {
				l.push(i, i+1, spanPipe)
				i++
			}
		case c == '>':
			j := i + 1
			if j < len(b) && (b[j] == '>' || b[j] == '|') {
				j++
			}
			if j < len(b) && b[j] == '&' {
				// 2>&1 style duplication, not a file.
				j++
				for j < len(b) && ('0' <= b[j] && b[j] <= '9' || b[j] == '-') {
					j++
				}
				i = j
				continue
			}
			l.push(i, j, spanRedirOut)
			i = j
		case c == '<':
			j := i + 1
			if j < len(b) && b[j] == '<' {
				j++
			}
			if j < len(b) && b[j] == '(' {
				// Process substitution: treat as a command boundary.
				l.push(i, j+1, spanSep)
				i = j + 1
				continue
			}
			l.push(i, j, spanRedirIn)
			i = j
		default:
			i = l.word(i)
		}
	}
}

func (l *shellLine) push(s, e int, k spanKind) {
	if l.n < maxSpans {
		l.spans[l.n] = span{s: int32(s), e: int32(e), kind: k}
		l.n++
	}
}

// word scans one word starting at i and returns the index after it. A
// numeric file-descriptor prefix of a redirection (the 2 of 2>) is dropped.
func (l *shellLine) word(i int) int {
	b := l.b
	start := i
	var quote byte
	for i < len(b) {
		c := b[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			i++
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			quote = c
			i++
			continue
		}
		if isSpace(c) || c == ';' || c == '|' || c == '&' || c == '>' || c == '<' || c == '(' || c == ')' {
			break
		}
		i++
	}
	if i < len(b) && b[i] == '>' && allDigits(b[start:i]) {
		return i
	}
	s, e := start, i
	for e-s >= 2 && (b[s] == '"' || b[s] == '\'') && b[e-1] == b[s] {
		s++
		e--
	}
	if e > s && (b[s] == '"' || b[s] == '\'') {
		s++ // unbalanced leading quote
	}
	if e > s && (b[e-1] == '"' || b[e-1] == '\'') {
		e--
	}
	if e > s {
		l.push(s, e, spanWord)
	}
	return i
}

// isCount reports a number with an optional duration suffix (timeout 5s).
func isCount(b []byte) bool {
	if n := len(b); n > 1 && (b[n-1] == 's' || b[n-1] == 'm' || b[n-1] == 'h') {
		b = b[:n-1]
	}
	return allDigits(b)
}

func allDigits(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (l *shellLine) text(sp span) []byte { return l.b[sp.s:sp.e] }

// segment is a range [from, to) of spans forming one simple command.
type segment struct {
	from, to int
	cmd      int    // span index of the command word, -1 if none
	base     []byte // basename of the command word
	pipedIn  bool // preceded by |
	pipedOut bool // followed by |
}

// wrappers are words that run the next word as the command.
var wrappers = [...]string{"sudo", "doas", "env", "command", "exec", "nohup", "time", "nice", "ionice", "builtin", "xargs", "busybox", "timeout", "stdbuf"}

// segments calls fn for each simple command in the line, stopping early
// when fn returns true, and reports whether fn did.
func (l *shellLine) segments(fn func(seg segment) bool) bool {
	if !l.segsDone {
		l.splitSegments()
	}
	for i := 0; i < l.nseg; i++ {
		if fn(l.segs[i]) {
			return true
		}
	}
	return false
}

// splitSegments computes the simple commands once per line; every matcher
// then reuses them.
func (l *shellLine) splitSegments() {
	l.segsDone = true
	l.nseg = 0
	seg := segment{cmd: -1}
	flush := func(to int, pipedOut bool) {
		seg.to = to
		seg.pipedOut = pipedOut
		if seg.to > seg.from && l.nseg < maxSegs {
			seg.cmd = l.commandWord(seg.from, seg.to)
			if seg.cmd >= 0 {
				seg.base = baseOf(l.text(l.spans[seg.cmd]))
			}
			l.segs[l.nseg] = seg
			l.nseg++
		}
	}
	for i := 0; i < l.n; i++ {
		switch l.spans[i].kind {
		case spanSep:
			flush(i, false)
			seg = segment{from: i + 1, cmd: -1}
		case spanPipe:
			flush(i, true)
			seg = segment{from: i + 1, cmd: -1, pipedIn: true}
		}
	}
	flush(l.n, false)
}

// commandWord finds the command word in spans [from, to), skipping
// variable assignments, redirections and wrapper commands with their flags.
func (l *shellLine) commandWord(from, to int) int {
	for i := from; i < to; i++ {
		sp := l.spans[i]
		if sp.kind == spanRedirOut || sp.kind == spanRedirIn {
			i++ // skip the redirection target
			continue
		}
		if sp.kind != spanWord {
			continue
		}
		w := l.text(sp)
		if bytes.IndexByte(w, '=') > 0 && w[0] != '-' && !bytes.ContainsRune(w[:bytes.IndexByte(w, '=')], '/') {
			continue // VAR=value
		}
		base := baseOf(w)
		wrapped := false
		for _, wr := range wrappers {
			if string(base) == wr {
				wrapped = true
				break
			}
		}
		if wrapped {
			// Skip the wrapper's own flags (sudo -u root, timeout 5).
			for i+1 < to && l.spans[i+1].kind == spanWord {
				nx := l.text(l.spans[i+1])
				if len(nx) == 0 || !(nx[0] == '-' || isCount(nx)) {
					break
				}
				i++
				if (string(nx) == "-u" || string(nx) == "-g") && i+1 < to {
					i++ // sudo -u USER
				}
			}
			continue
		}
		return i
	}
	return -1
}

// cmdIs reports whether the segment's command word has one of the bases.
func (l *shellLine) cmdIs(seg segment, names ...string) bool {
	if seg.cmd < 0 {
		return false
	}
	for _, n := range names {
		if string(seg.base) == n {
			return true
		}
	}
	return false
}

// args calls fn for each word after the command word, stopping when fn
// returns true. Redirection targets are not arguments.
func (l *shellLine) args(seg segment, fn func(i int, w []byte) bool) bool {
	if seg.cmd < 0 {
		return false
	}
	for i := seg.cmd + 1; i < seg.to; i++ {
		sp := l.spans[i]
		if sp.kind == spanRedirOut || sp.kind == spanRedirIn {
			i++
			continue
		}
		if sp.kind == spanWord && fn(i, l.text(sp)) {
			return true
		}
	}
	return false
}

// lastOperand returns the last non-flag argument of the segment.
func (l *shellLine) lastOperand(seg segment) []byte {
	var last []byte
	l.args(seg, func(_ int, w []byte) bool {
		if len(w) > 0 && w[0] != '-' {
			last = w
		}
		return false
	})
	return last
}

func (l *shellLine) hasFlag(seg segment, short byte, long string) bool {
	return l.args(seg, func(_ int, w []byte) bool {
		if len(w) >= 2 && w[0] == '-' && w[1] != '-' && short != 0 {
			return bytes.IndexByte(w[1:], short) >= 0
		}
		return long != "" && string(w) == long
	})
}

// writeTargets calls fn with every path the line writes to: redirection
// targets, and the destinations of tee, cp, mv, install, ln, dd, sed -i,
// truncate, touch, chmod and chown. It stops when fn returns true.
func (l *shellLine) writeTargets(fn func(p []byte) bool) bool {
	for i := 0; i+1 < l.n; i++ {
		if l.spans[i].kind == spanRedirOut && l.spans[i+1].kind == spanWord {
			if fn(l.text(l.spans[i+1])) {
				return true
			}
		}
	}
	return l.segments(func(seg segment) bool {
		switch {
		case l.cmdIs(seg, "tee", "touch", "truncate", "chmod", "chown", "chattr", "setfacl"):
			return l.args(seg, func(_ int, w []byte) bool { return len(w) > 0 && w[0] != '-' && fn(w) })
		case l.cmdIs(seg, "cp", "mv", "install", "ln", "rsync", "copy", "move", "copy-item", "move-item", "set-content", "add-content", "out-file"):
			if p := l.lastOperand(seg); p != nil {
				return fn(p)
			}
		case l.cmdIs(seg, "dd"):
			return l.args(seg, func(_ int, w []byte) bool { return bytes.HasPrefix(w, []byte("of=")) && fn(w[3:]) })
		case l.cmdIs(seg, "sed", "perl", "ruby"):
			if l.hasFlag(seg, 'i', "--in-place") {
				if p := l.lastOperand(seg); p != nil {
					return fn(p)
				}
			}
		}
		return false
	})
}

// anyWord calls fn on every word, stopping when it returns true.
func (l *shellLine) anyWord(fn func(w []byte) bool) bool {
	for i := 0; i < l.n; i++ {
		if l.spans[i].kind == spanWord && fn(l.text(l.spans[i])) {
			return true
		}
	}
	return false
}

// baseOf returns the last path element of p, ignoring a trailing slash.
func baseOf(p []byte) []byte {
	for len(p) > 1 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	if i := bytes.LastIndexByte(p, '/'); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	return p
}

var deleteCmds = [...]string{"rm", "rmdir", "unlink", "shred", "del", "erase", "rd", "remove-item", "ri"}

// deletionWeight estimates how many files a command line deletes: one per
// operand of rm-like commands, wildcard for globs, find -delete and git
// clean. It returns zero for lines that delete nothing.
func (l *shellLine) deletionWeight(wildcard float64) float64 {
	total := 0.0
	l.segments(func(seg segment) bool {
		switch {
		case l.cmdIs(seg, deleteCmds[:]...):
			l.args(seg, func(_ int, w []byte) bool {
				if len(w) > 0 && w[0] != '-' {
					total += operandWeight(w, wildcard)
				}
				return false
			})
		case l.cmdIs(seg, "git"):
			sub := 0
			l.args(seg, func(_ int, w []byte) bool {
				if sub == 0 && len(w) > 0 && w[0] != '-' {
					switch string(w) {
					case "rm":
						sub = 1
					case "clean":
						sub = 2
					default:
						sub = -1
					}
					return false
				}
				if sub == 1 && len(w) > 0 && w[0] != '-' {
					total += operandWeight(w, wildcard)
				}
				return sub < 0
			})
			if sub == 2 && l.hasFlag(seg, 'f', "--force") {
				total += wildcard
			}
		case l.cmdIs(seg, "find"):
			if l.args(seg, func(_ int, w []byte) bool { return string(w) == "-delete" }) {
				total += wildcard
			}
		}
		return false
	})
	return total
}

func operandWeight(w []byte, wildcard float64) float64 {
	if bytes.ContainsAny(w, "*?[") {
		return wildcard
	}
	return 1
}

// dangerousRoots are operands that make a recursive delete catastrophic.
var dangerousRoots = [...]string{
	"/", "/*", "~", "~/", "~/*", "$home", "${home}", "$home/", "$home/*", "${home}/", "${home}/*",
	"*", ".", "./", "./*", "..", "../", "../*", ".*", "/home", "/users", "/root", "/etc", "/usr", "/var",
	"/bin", "/sbin", "/lib", "/lib64", "/boot", "/opt", "/srv", "c:", "c:/", "c:/*", "%userprofile%",
}

// catastrophicDelete reports a recursive delete of a root-like path, a
// find -delete from a root, or a disk-level overwrite.
func (l *shellLine) catastrophicDelete() bool {
	return l.segments(func(seg segment) bool {
		switch {
		case l.cmdIs(seg, "rm", "remove-item", "ri", "rd", "rmdir"):
			if !l.hasFlag(seg, 'r', "--recursive") && !l.hasFlag(seg, 'R', "-recurse") && !l.hasFlag(seg, 's', "/s") {
				return false
			}
			return l.args(seg, func(_ int, w []byte) bool { return isDangerousRoot(w) })
		case l.cmdIs(seg, "find"):
			root := []byte(nil)
			del := false
			l.args(seg, func(_ int, w []byte) bool {
				if root == nil && len(w) > 0 && w[0] != '-' {
					root = w
				}
				if string(w) == "-delete" {
					del = true
				}
				return false
			})
			return del && root != nil && isDangerousRoot(root) && string(root) != "." && string(root) != "./"
		case l.cmdIs(seg, "mkfs", "mkfs.ext4", "mkfs.xfs", "mkfs.vfat", "wipefs", "format"):
			return true
		case l.cmdIs(seg, "dd"):
			return l.args(seg, func(_ int, w []byte) bool {
				return bytes.HasPrefix(w, []byte("of=/dev/")) && !bytes.Equal(w, []byte("of=/dev/null"))
			})
		}
		return false
	})
}

func isDangerousRoot(w []byte) bool {
	for _, r := range dangerousRoots {
		if string(w) == r {
			return true
		}
	}
	return false
}

// shells are interpreters that execute what is piped into them.
var shells = [...]string{"sh", "bash", "zsh", "dash", "ksh", "fish", "python", "python3", "perl", "ruby", "node", "php", "iex", "invoke-expression", "pwsh", "powershell", "cmd", "source", "."}

var downloaders = [...]string{"curl", "wget", "fetch", "aria2c", "iwr", "irm", "invoke-webrequest", "invoke-restmethod", "http", "https"}

// pipeToShell reports a download piped into an interpreter, or a shell run
// on the output of a download via command or process substitution.
func (l *shellLine) pipeToShell() bool {
	prevDownload := false
	if l.segments(func(seg segment) bool {
		if seg.pipedIn && prevDownload && l.cmdIs(seg, shells[:]...) {
			return true
		}
		if !seg.pipedIn {
			prevDownload = false
		}
		if l.cmdIs(seg, downloaders[:]...) {
			prevDownload = true
		}
		return false
	}) {
		return true
	}
	for _, sub := range [...]string{"$(curl", "$(wget", "`curl", "`wget", "<(curl", "<(wget", "(iwr", "(irm", "(invoke-webrequest", "(new-object net.webclient"} {
		if bytes.Contains(l.b, []byte(sub)) {
			return true
		}
	}
	return false
}

// downloadTarget returns the [s, e) offsets of the basename of the file a
// curl or wget command writes, or ok=false.
func (l *shellLine) downloadTarget() (s, e int, ok bool) {
	l.segments(func(seg segment) bool {
		if !l.cmdIs(seg, "curl", "wget", "aria2c", "fetch", "iwr", "invoke-webrequest") {
			return false
		}
		var url span
		haveURL := false
		remoteName := false
		for i := seg.cmd + 1; i < seg.to; i++ {
			sp := l.spans[i]
			if sp.kind == spanRedirOut && i+1 < l.n && l.spans[i+1].kind == spanWord {
				s, e, ok = baseSpan(l.b, l.spans[i+1])
				return ok
			}
			if sp.kind != spanWord {
				continue
			}
			w := l.text(sp)
			switch {
			case string(w) == "-o" || string(w) == "--output" || string(w) == "--output-document" || string(w) == "-outfile" || string(w) == "--out":
				if i+1 < seg.to && l.spans[i+1].kind == spanWord {
					s, e, ok = baseSpan(l.b, l.spans[i+1])
					return ok
				}
			case bytes.HasPrefix(w, []byte("--output=")) || bytes.HasPrefix(w, []byte("--output-document=")) || bytes.HasPrefix(w, []byte("--out=")):
				eq := bytes.IndexByte(w, '=')
				s, e, ok = baseSpan(l.b, span{s: sp.s + int32(eq) + 1, e: sp.e})
				return ok
			case len(w) > 2 && w[0] == '-' && w[1] == 'o' && !bytes.HasPrefix(w, []byte("-out")):
				s, e, ok = baseSpan(l.b, span{s: sp.s + 2, e: sp.e})
				return ok
			case string(w) == "--remote-name" || string(w) == "--remote-name-all":
				remoteName = true
			case bytes.Contains(w, []byte("://")):
				url, haveURL = sp, true
			}
		}
		// wget (and curl -O) save under the URL's last path element.
		if haveURL && (remoteName || l.cmdIs(seg, "wget", "aria2c", "fetch")) {
			u := url
			if q := bytes.IndexAny(l.b[u.s:u.e], "?#"); q >= 0 {
				u.e = u.s + int32(q)
			}
			if p := bytes.Index(l.b[u.s:u.e], []byte("://")); p >= 0 && bytes.IndexByte(l.b[u.s+int32(p)+3:u.e], '/') >= 0 {
				s, e, ok = baseSpan(l.b, u)
				return ok
			}
		}
		return false
	})
	return s, e, ok
}

// baseSpan narrows sp to its basename.
func baseSpan(b []byte, sp span) (int, int, bool) {
	s, e := int(sp.s), int(sp.e)
	for e > s && b[e-1] == '/' {
		e--
	}
	if i := bytes.LastIndexByte(b[s:e], '/'); i >= 0 {
		s += i + 1
	}
	return s, e, e > s && !(e-s == 1 && b[s] == '-')
}
