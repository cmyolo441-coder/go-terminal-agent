package policy

import (
	"path"
	"strings"
)

// A segment is one simple command found in a shell command line, after
// quote removal, with environment assignments and wrapper programs (sudo,
// env, nohup, timeout, ...) stripped and the program reduced to its base
// name. Policy rules match segments, so "ls; curl x", "FOO=1 /usr/bin/curl
// x", "sudo curl x", "echo $(curl x)" and "bash -c 'curl x'" are all
// recognised as running curl.
type segment struct {
	// prog is the program's base name.
	prog string
	// args are the arguments after quote removal.
	args []string
	// redirIn and redirOut are files read with < and written with >.
	redirIn, redirOut []string
}

// text is the normalised command: program and arguments joined by spaces.
func (s segment) text() string {
	if len(s.args) == 0 {
		return s.prog
	}
	return s.prog + " " + strings.Join(s.args, " ")
}

// maxShellDepth bounds recursion into $(...), backticks and sh -c.
const maxShellDepth = 8

// parseShell splits a command line into segments. It is deliberately
// permissive: anything it cannot parse exactly still yields words, so a
// deny rule errs towards matching.
func parseShell(cmd string) []segment {
	var out []segment
	parseShellInto(cmd, 0, &out)
	return out
}

type word struct {
	text string
	// op marks operator tokens: "<", ">" (redirections).
	op bool
}

func parseShellInto(cmd string, depth int, out *[]segment) {
	if depth > maxShellDepth {
		return
	}
	var (
		words []word
		cur   strings.Builder
		inTok bool
	)
	endWord := func() {
		if inTok {
			words = append(words, word{text: cur.String()})
			cur.Reset()
			inTok = false
		}
	}
	endSegment := func() {
		endWord()
		if seg, ok := buildSegment(words, depth, out); ok {
			*out = append(*out, seg)
		}
		words = nil
	}
	// sub extracts a $(...) or `...` body starting after the opener and
	// returns the index after the closer.
	sub := func(i int, open, close byte) int {
		level, j := 1, i
		for j < len(cmd) {
			c := cmd[j]
			switch {
			case c == '\\' && j+1 < len(cmd):
				j += 2
				continue
			case c == '\'' && open != '`':
				if k := strings.IndexByte(cmd[j+1:], '\''); k >= 0 {
					j += k + 2
					continue
				}
			case open != close && c == open:
				level++
			case c == close:
				level--
				if level == 0 {
					parseShellInto(cmd[i:j], depth+1, out)
					return j + 1
				}
			}
			j++
		}
		parseShellInto(cmd[i:], depth+1, out)
		return len(cmd)
	}
	for i := 0; i < len(cmd); {
		c := cmd[i]
		switch {
		case c == '\\':
			if i+1 < len(cmd) {
				if cmd[i+1] != '\n' {
					cur.WriteByte(cmd[i+1])
					inTok = true
				}
				i += 2
			} else {
				i++
			}
		case c == '\'':
			inTok = true
			k := strings.IndexByte(cmd[i+1:], '\'')
			if k < 0 {
				cur.WriteString(cmd[i+1:])
				i = len(cmd)
			} else {
				cur.WriteString(cmd[i+1 : i+1+k])
				i += k + 2
			}
		case c == '"':
			inTok = true
			i++
			for i < len(cmd) && cmd[i] != '"' {
				switch {
				case cmd[i] == '\\' && i+1 < len(cmd) && strings.IndexByte("\"\\$`\n", cmd[i+1]) >= 0:
					if cmd[i+1] != '\n' {
						cur.WriteByte(cmd[i+1])
					}
					i += 2
				case cmd[i] == '$' && i+1 < len(cmd) && cmd[i+1] == '(':
					i = sub(i+2, '(', ')')
					cur.WriteString("$(…)")
				case cmd[i] == '`':
					i = sub(i+1, '`', '`')
					cur.WriteString("$(…)")
				default:
					cur.WriteByte(cmd[i])
					i++
				}
			}
			i++
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '\'':
			// ANSI-C quoting: $'cu\x72l' is curl.
			s, next := ansiCQuoted(cmd, i+2)
			cur.WriteString(s)
			inTok = true
			i = next
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '"':
			// Locale quoting: $"curl" is "curl".
			inTok = true
			i++
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '(':
			// Command substitution runs its own commands; the word keeps a
			// marker so the outer command is still seen.
			i = sub(i+2, '(', ')')
			cur.WriteString("$(…)")
			inTok = true
		case c == '`':
			i = sub(i+1, '`', '`')
			cur.WriteString("$(…)")
			inTok = true
		case (c == '<' || c == '>') && i+1 < len(cmd) && cmd[i+1] == '(':
			// Process substitution.
			endWord()
			i = sub(i+2, '(', ')')
		case c == ';' || c == '\n' || c == '|' || c == '&' || c == '(' || c == ')':
			// Command separators and grouping. "2>&1" and "&>" keep their
			// redirection meaning.
			if c == '&' && i > 0 && cmd[i-1] == '>' {
				i++
				continue
			}
			if c == '&' && i+1 < len(cmd) && cmd[i+1] == '>' {
				endWord()
				words = append(words, word{text: ">", op: true})
				i += 2
				if i < len(cmd) && cmd[i] == '>' {
					i++
				}
				continue
			}
			endSegment()
			i++
		case c == '<' || c == '>':
			// A file descriptor number before > belongs to the operator.
			if inTok && isDigits(cur.String()) {
				cur.Reset()
				inTok = false
			}
			endWord()
			op := string(c)
			i++
			for i < len(cmd) && (cmd[i] == '>' || cmd[i] == '<' || cmd[i] == '|') {
				i++
			}
			if i < len(cmd) && cmd[i] == '&' {
				// >&2: duplicating a descriptor, not a file.
				i++
				for i < len(cmd) && (cmd[i] >= '0' && cmd[i] <= '9' || cmd[i] == '-') {
					i++
				}
				continue
			}
			words = append(words, word{text: op, op: true})
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
			i++
		case c == '#' && !inTok:
			// A comment runs to the end of the line.
			k := strings.IndexByte(cmd[i:], '\n')
			if k < 0 {
				i = len(cmd)
			} else {
				i += k
			}
		default:
			cur.WriteByte(c)
			inTok = true
			i++
		}
	}
	endSegment()
}

// ansiCQuoted decodes the body of a $'...' string starting at i (just after
// the opening quote) as bash does, and returns it with the index after the
// closing quote. An unterminated string runs to the end of cmd.
func ansiCQuoted(cmd string, i int) (string, int) {
	var b strings.Builder
	hexVal := func(c byte) (int, bool) {
		switch {
		case c >= '0' && c <= '9':
			return int(c - '0'), true
		case c >= 'a' && c <= 'f':
			return int(c-'a') + 10, true
		case c >= 'A' && c <= 'F':
			return int(c-'A') + 10, true
		}
		return 0, false
	}
	// digits reads up to max digits in base from cmd[j:].
	digits := func(j, max, base int) (int, int) {
		v, n := 0, 0
		for n < max && j+n < len(cmd) {
			d, ok := hexVal(cmd[j+n])
			if !ok || d >= base {
				break
			}
			v = v*base + d
			n++
		}
		return v, n
	}
	for i < len(cmd) {
		c := cmd[i]
		if c == '\'' {
			return b.String(), i + 1
		}
		if c != '\\' || i+1 >= len(cmd) {
			b.WriteByte(c)
			i++
			continue
		}
		e := cmd[i+1]
		i += 2
		switch e {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'e', 'E':
			b.WriteByte(0x1b)
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\', '\'', '"', '?':
			b.WriteByte(e)
		case 'c':
			if i < len(cmd) {
				b.WriteByte(cmd[i] & 0x1f)
				i++
			}
		case 'x':
			v, n := digits(i, 2, 16)
			if n == 0 {
				b.WriteString(`\x`)
				break
			}
			b.WriteByte(byte(v))
			i += n
		case 'u', 'U':
			max := 4
			if e == 'U' {
				max = 8
			}
			v, n := digits(i, max, 16)
			if n == 0 {
				b.WriteByte('\\')
				b.WriteByte(e)
				break
			}
			b.WriteRune(rune(v))
			i += n
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v, n := digits(i-1, 3, 8)
			b.WriteByte(byte(v))
			i += n - 1
		default:
			b.WriteByte('\\')
			b.WriteByte(e)
		}
	}
	return b.String(), len(cmd)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// wrappers run the command given in their arguments. The value lists
// flags that take a separate argument.
var wrappers = map[string]string{
	"sudo": "ugpCDhrtUT", "doas": "uC", "env": "uSC", "command": "", "builtin": "", "exec": "a",
	"nohup": "", "time": "fo", "nice": "n", "ionice": "cnpP", "timeout": "sk", "stdbuf": "ioe",
	"xargs": "aEeIiLlnPsd", "setsid": "", "unbuffer": "", "chronic": "", "chrt": "", "taskset": "",
	"flock": "wE", "caffeinate": "tw", "watch": "nd", "strace": "eoOpsSuE", "ltrace": "eoOpsSuE",
	"busybox": "", "torsocks": "", "proxychains": "f", "proxychains4": "f", "firejail": "", "su": "cgGs",
}

var reserved = map[string]bool{"{": true, "}": true, "!": true, "if": true, "then": true, "else": true, "elif": true,
	"fi": true, "do": true, "done": true, "while": true, "until": true, "esac": true, "coproc": true}

// shells interpret their -c argument as a command line.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "ash": true, "fish": true, "csh": true, "tcsh": true}

// buildSegment turns words into a segment, unwrapping wrappers and
// recursing into sh -c / eval strings.
func buildSegment(words []word, depth int, out *[]segment) (segment, bool) {
	var seg segment
	var rest []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w.op {
			if i+1 < len(words) && !words[i+1].op {
				if w.text == "<" {
					seg.redirIn = append(seg.redirIn, words[i+1].text)
				} else {
					seg.redirOut = append(seg.redirOut, words[i+1].text)
				}
				i++
			}
			continue
		}
		rest = append(rest, w.text)
	}
	// Leading NAME=value assignments and shell reserved words, so that
	// "if curl x", "do curl x" and "{ curl x" are seen as curl.
	for len(rest) > 0 && (isAssignment(rest[0]) || reserved[rest[0]]) {
		rest = rest[1:]
	}
	for len(rest) > 0 {
		prog := path.Base(strings.ReplaceAll(rest[0], "\\", "/"))
		argFlags, isWrapper := wrappers[prog]
		if !isWrapper {
			break
		}
		if prog == "su" {
			// su [opts] [user] -c "cmd"
			for i := 1; i < len(rest)-1; i++ {
				if rest[i] == "-c" || rest[i] == "--command" {
					parseShellInto(rest[i+1], depth+1, out)
				}
			}
			break
		}
		if prog == "busybox" && len(rest) > 1 {
			rest = rest[1:]
			continue
		}
		rest = rest[1:]
		for len(rest) > 0 {
			a := rest[0]
			switch {
			case a == "--":
				rest = rest[1:]
				goto unwrapped
			case strings.HasPrefix(a, "-") && len(a) > 1:
				rest = rest[1:]
				if !strings.HasPrefix(a, "--") && len(a) == 2 && strings.IndexByte(argFlags, a[1]) >= 0 && len(rest) > 0 {
					rest = rest[1:]
				}
			case isAssignment(a) && (prog == "env" || prog == "sudo"):
				rest = rest[1:]
			case (prog == "timeout" || prog == "chrt" || prog == "taskset" || prog == "flock") && looksLikeOperand(a):
				// timeout DURATION cmd, chrt PRIO cmd, taskset MASK cmd,
				// flock FILE cmd.
				rest = rest[1:]
				goto unwrapped
			default:
				goto unwrapped
			}
		}
	unwrapped:
	}
	if len(rest) == 0 {
		if len(seg.redirIn)+len(seg.redirOut) == 0 {
			return seg, false
		}
		seg.prog = ""
		return seg, true
	}
	seg.prog = path.Base(strings.ReplaceAll(rest[0], "\\", "/"))
	seg.args = rest[1:]
	// sh -c 'cmd', bash -lc 'cmd', eval 'cmd'.
	if shells[seg.prog] {
		for i, a := range seg.args {
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(seg.args) {
				parseShellInto(seg.args[i+1], depth+1, out)
				break
			}
		}
	}
	if seg.prog == "eval" && len(seg.args) > 0 {
		parseShellInto(strings.Join(seg.args, " "), depth+1, out)
	}
	return seg, true
}

func looksLikeOperand(a string) bool {
	if a == "" {
		return false
	}
	c := a[0]
	return (c >= '0' && c <= '9') || c == '/' || c == '.' || c == '~'
}

func isAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := w[i]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
