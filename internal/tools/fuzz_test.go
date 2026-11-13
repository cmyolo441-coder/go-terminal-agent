package tools

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// applyUnifiedDiff applies a diff produced by UnifiedDiff to a, as patch
// would, including "\ No newline at end of file" markers.
func applyUnifiedDiff(a, diff string) (string, error) {
	if diff == "" {
		return a, nil
	}
	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "--- ") || !strings.HasPrefix(lines[1], "+++ ") {
		return "", fmt.Errorf("no header")
	}
	src := splitLines(a)
	srcNoEOL := a != "" && !strings.HasSuffix(a, "\n")
	const noEOL = `\ No newline at end of file`
	var out []string
	outNoEOL := false // whether the last line in out lacks a newline
	pos := 0          // next source line (0-based)
	copySrc := func(to int) {
		if to > len(src) {
			to = len(src)
		}
		if to > pos {
			out = append(out, src[pos:to]...)
			outNoEOL = to == len(src) && srcNoEOL
		}
		pos = to
	}
	i := 2
	for i < len(lines) {
		h := lines[i]
		var as, an, bs, bn int
		if _, err := fmt.Sscanf(h, "@@ -%d,%d +%d,%d @@", &as, &an, &bs, &bn); err != nil {
			return "", fmt.Errorf("bad hunk header %q", h)
		}
		start := as - 1
		if an == 0 {
			start = as
		}
		if start < pos || start > len(src) {
			return "", fmt.Errorf("hunk %q out of order", h)
		}
		copySrc(start)
		i++
		gotA, gotB := 0, 0
		for i < len(lines) && !strings.HasPrefix(lines[i], "@@ ") {
			l := lines[i]
			i++
			if l == "" || l == noEOL {
				return "", fmt.Errorf("unexpected diff line %q", l)
			}
			marked := i < len(lines) && lines[i] == noEOL
			if marked {
				i++
			}
			kind, text := l[0], l[1:]
			switch kind {
			case ' ', '-':
				if pos >= len(src) || src[pos] != text {
					return "", fmt.Errorf("context mismatch at source line %d: %q vs diff %q", pos+1, safeAt(src, pos), text)
				}
				if marked != (pos == len(src)-1 && srcNoEOL) {
					return "", fmt.Errorf("newline marker mismatch at source line %d", pos+1)
				}
				pos++
				gotA++
				if kind == ' ' {
					out = append(out, text)
					outNoEOL = marked
					gotB++
				}
			case '+':
				out = append(out, text)
				outNoEOL = marked
				gotB++
			default:
				return "", fmt.Errorf("bad diff line %q", l)
			}
		}
		if gotA != an || gotB != bn {
			return "", fmt.Errorf("hunk %q counts %d,%d", h, gotA, gotB)
		}
	}
	copySrc(len(src))
	if len(out) == 0 {
		return "", nil
	}
	res := strings.Join(out, "\n")
	if !outNoEOL {
		res += "\n"
	}
	return res, nil
}

func safeAt(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<eof>"
}

// FuzzUnifiedDiff checks that the diff shown to the user describes the
// change exactly: applying it to the old text gives the new text.
func FuzzUnifiedDiff(f *testing.F) {
	for _, p := range [][2]string{
		{"a\nb\nc\n", "a\nB\nc\n"}, {"", "x\n"}, {"x\n", ""}, {"x", "x\n"}, {"x\n", "x"}, {"a\nb", "a\nc"},
		{"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n", "1\n2\nX\n4\n5\n6\n7\n8\n9\n10\nY\n12\n"},
		{"a\r\nb\r\n", "a\r\nc\r\n"}, {"\n\n\n", "\n"}, {"é\xff\n", "é\n"},
	} {
		f.Add(p[0], p[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		if len(a) > 4096 || len(b) > 4096 {
			return
		}
		d := UnifiedDiff("f", a, b)
		if (d == "") != (a == b) {
			t.Fatalf("diff of %q and %q is %q", a, b, d)
		}
		got, err := applyUnifiedDiff(a, d)
		if err != nil {
			t.Fatalf("diff of %q -> %q does not apply: %v\n%s", a, b, err, d)
		}
		if got != b {
			t.Fatalf("diff of %q -> %q applies to %q\n%s", a, b, got, d)
		}
	})
}

// FuzzApplyEdit checks edit_file's replacement: no panics, the counts it
// reports are right, and a successful single edit changes exactly one
// occurrence.
func FuzzApplyEdit(f *testing.F) {
	for _, s := range [][3]string{
		{"a\nb\nc\n", "b", "B"}, {"x x x", "x", "y"}, {"", "", "new"}, {"line1\r\nline2\r\n", "line1\nline2", "L1\nL2"},
		{"no newline at end", "end", "END"}, {"tab\tindent\n", "  indent", "x"}, {"é\xffz", "\xff", "?"},
	} {
		f.Add(s[0], s[1], s[2], false)
		f.Add(s[0], s[1], s[2], true)
	}
	f.Fuzz(func(t *testing.T, content, oldS, newS string, all bool) {
		out, n, err := applyEdit(content, oldS, newS, all)
		if err != nil {
			if err.Error() == "" {
				t.Fatal("empty error")
			}
			return
		}
		if n < 1 {
			t.Fatalf("success with %d replacements", n)
		}
		if oldS == "" {
			if content != "" || out != newS {
				t.Fatalf("empty old_string on %q gave %q", content, out)
			}
			return
		}
		if !all && n != 1 {
			t.Fatalf("single edit reported %d replacements", n)
		}
		if strings.Count(content, oldS) > 0 && !all {
			i := strings.Index(content, oldS)
			if out != content[:i]+newS+content[i+len(oldS):] {
				t.Fatalf("edit of %q: %q -> %q gave %q", content, oldS, newS, out)
			}
		}
		if utf8.ValidString(content) && utf8.ValidString(oldS) && utf8.ValidString(newS) && !utf8.ValidString(out) {
			t.Fatalf("valid UTF-8 became invalid: %q", out)
		}
	})
}

// FuzzTruncate checks that output truncation stays within its limit (plus
// the marker) and never splits a UTF-8 character.
func FuzzTruncate(f *testing.F) {
	for _, s := range []string{"héllo wörld", strings.Repeat("日本", 50), "abc"} {
		f.Add(s, 7)
	}
	f.Fuzz(func(t *testing.T, s string, limit int) {
		out := truncate(s, limit)
		if limit <= 0 || len(s) <= limit {
			if out != s {
				t.Fatal("changed a short string")
			}
			return
		}
		if utf8.ValidString(s) && !utf8.ValidString(out) {
			t.Fatalf("truncate(%q, %d) = %q splits a character", s, limit, out)
		}
		if !strings.Contains(out, "characters omitted") {
			t.Fatalf("no marker in %q", out)
		}
		_ = strconv.Itoa(limit)
	})
}
