package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// FileStatus says how a file changed in a diff.
type FileStatus string

const (
	StatusAdded    FileStatus = "added"
	StatusModified FileStatus = "modified"
	StatusDeleted  FileStatus = "deleted"
	StatusRenamed  FileStatus = "renamed"
	StatusCopied   FileStatus = "copied"
)

// Letter returns the one-letter code git uses for the status (A, M, D, R, C).
func (s FileStatus) Letter() string {
	switch s {
	case StatusAdded:
		return "A"
	case StatusDeleted:
		return "D"
	case StatusRenamed:
		return "R"
	case StatusCopied:
		return "C"
	default:
		return "M"
	}
}

// Hunk is one "@@" section of a unified diff.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	// Section is the function or heading context git prints after "@@".
	Section string
	// Lines holds the hunk body; each line keeps its ' ', '+', '-' or '\'
	// prefix and has no trailing newline.
	Lines []string

	header string // the raw "@@ ... @@" line when parsed from git output
}

// HeaderLine renders the hunk's "@@ -a,b +c,d @@ section" line.
func (h Hunk) HeaderLine() string {
	if h.header != "" {
		return h.header
	}
	rng := func(start, n int) string {
		if n == 1 {
			return strconv.Itoa(start)
		}
		return fmt.Sprintf("%d,%d", start, n)
	}
	s := fmt.Sprintf("@@ -%s +%s @@", rng(h.OldStart, h.OldLines), rng(h.NewStart, h.NewLines))
	if h.Section != "" {
		s += " " + h.Section
	}
	return s
}

// FileDiff is the parsed diff of a single file.
type FileDiff struct {
	// Path is the file's path after the change, relative to the
	// repository top level (for deletions, the deleted path).
	Path string
	// OldPath is the source path of a rename or copy, otherwise empty.
	OldPath string
	Status  FileStatus
	// OldMode and NewMode are git file modes such as "100644" when known.
	OldMode, NewMode string
	// Similarity is the rename or copy similarity percentage.
	Similarity int
	// Binary is true when git (or the untracked-file renderer) reported
	// binary content, so there are no hunks.
	Binary bool
	// Untracked is true for files git does not track yet, rendered as
	// additions.
	Untracked bool
	// Added and Deleted count changed lines across all hunks, including
	// hunks dropped by the size budget.
	Added, Deleted int
	// Hunks holds the parsed hunks kept within the size budget.
	Hunks []Hunk
	// Omitted is true when some or all hunks were dropped by the budget.
	Omitted bool
	// Note explains special cases, e.g. a large untracked file whose
	// content was not rendered.
	Note string
	// Patch is the unified diff text for this file (header and kept hunks).
	Patch string

	header string // raw extended header lines, each ending in "\n"
}

// DiffOptions selects what Diff compares.
type DiffOptions struct {
	// Staged compares the index with HEAD instead of the work tree with
	// HEAD. Untracked files are never included in a staged diff.
	Staged bool
	// Paths limits the diff to these files or directories, relative to the
	// root passed to Diff or absolute. Paths are literal, not glob patterns.
	Paths []string
	// NoUntracked leaves untracked files out of a work tree diff.
	NoUntracked bool
	// Context is the number of context lines; zero or less means 3.
	Context int
	// MaxBytes caps the rendered diff text; zero or less means
	// DefaultMaxDiffBytes. Line counts always cover the full diff.
	MaxBytes int
}

// DefaultMaxDiffBytes is the default budget for DiffResult.Text.
const DefaultMaxDiffBytes = 256 << 10

// maxUntrackedBytes is the largest untracked file whose content is rendered.
const maxUntrackedBytes = 1 << 20

// DiffResult is a rendered and parsed diff.
type DiffResult struct {
	// Root is the repository top level that paths are relative to.
	Root string
	// Base is what the diff compares against: "HEAD", or "empty tree"
	// before the first commit.
	Base string
	// Text is the unified diff, cut to the size budget.
	Text string
	// Files lists every changed file, in git's order, then untracked files.
	Files []FileDiff
	// Added and Deleted total the line counts of all files.
	Added, Deleted int
	// Truncated is true when hunks were dropped from Text and Files.
	Truncated bool
}

// Empty reports whether nothing changed.
func (r *DiffResult) Empty() bool { return len(r.Files) == 0 }

// Stat renders a per-file summary similar to `git diff --stat`:
//
//	M  internal/app.go          +12  -3
//	A  docs/new.md (untracked)  +40  -0
//	2 files changed, 52 insertions(+), 3 deletions(-)
func (r *DiffResult) Stat() string {
	if r.Empty() {
		return "no changes"
	}
	names := make([]string, len(r.Files))
	width := 0
	for i, f := range r.Files {
		n := f.Path
		if f.OldPath != "" {
			n = f.OldPath + " → " + f.Path
		}
		if f.Untracked {
			n += " (untracked)"
		}
		names[i] = n
		width = max(width, len([]rune(n)))
	}
	var b strings.Builder
	for i, f := range r.Files {
		pad := strings.Repeat(" ", width-len([]rune(names[i])))
		if f.Binary {
			fmt.Fprintf(&b, "%s  %s%s  binary\n", f.Status.Letter(), names[i], pad)
			continue
		}
		fmt.Fprintf(&b, "%s  %s%s  +%d -%d\n", f.Status.Letter(), names[i], pad, f.Added, f.Deleted)
	}
	files := "files"
	if len(r.Files) == 1 {
		files = "file"
	}
	fmt.Fprintf(&b, "%d %s changed, %d insertions(+), %d deletions(-)", len(r.Files), files, r.Added, r.Deleted)
	return b.String()
}

// Diff returns the changes in the repository containing root: the work tree
// against HEAD (staged and unstaged together, plus untracked files rendered
// as additions), or with opts.Staged only what is staged. Before the first
// commit the comparison base is the empty tree.
func Diff(ctx context.Context, root string, opts DiffOptions) (*DiffResult, error) {
	top, err := TopLevel(ctx, root)
	if err != nil {
		return nil, err
	}
	paths, err := repoPaths(root, top, opts.Paths)
	if err != nil {
		return nil, err
	}
	base, err := baseRev(ctx, top)
	if err != nil {
		return nil, err
	}
	nctx := opts.Context
	if nctx <= 0 {
		nctx = 3
	}
	args := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv",
		"--src-prefix=a/", "--dst-prefix=b/", "-M", fmt.Sprintf("-U%d", nctx)}
	if opts.Staged {
		args = append(args, "--cached")
	}
	args = append(args, base, "--")
	args = append(args, paths...)
	res, err := run(ctx, top, runOpts{literal: true}, args...)
	if err != nil {
		return nil, err
	}
	out := &DiffResult{Root: top, Base: "HEAD"}
	if base != "HEAD" {
		out.Base = "empty tree"
	}
	files, err := parsePatch(string(res.stdout))
	if err != nil {
		return nil, err
	}
	if !opts.Staged && !opts.NoUntracked {
		untracked, err := untrackedDiffs(ctx, top, paths)
		if err != nil {
			return nil, err
		}
		files = append(files, untracked...)
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxDiffBytes
	}
	out.Files = files
	out.applyBudget(maxBytes)
	return out, nil
}

// applyBudget renders Text and each Patch, dropping whole hunks once the
// budget is spent. File headers are always kept so every change is visible.
func (r *DiffResult) applyBudget(maxBytes int) {
	var text strings.Builder
	omittedHunks, omittedFiles := 0, 0
	for i := range r.Files {
		f := &r.Files[i]
		r.Added += f.Added
		r.Deleted += f.Deleted
		var p strings.Builder
		p.WriteString(f.header)
		kept := f.Hunks[:0:0]
		for _, h := range f.Hunks {
			body := renderHunk(h)
			if text.Len()+p.Len()+len(body) > maxBytes {
				f.Omitted = true
				omittedHunks++
				continue
			}
			p.WriteString(body)
			kept = append(kept, h)
		}
		if f.Omitted {
			omittedFiles++
			f.Hunks = kept
		}
		f.Patch = p.String()
		text.WriteString(f.Patch)
	}
	if omittedHunks > 0 {
		r.Truncated = true
		fmt.Fprintf(&text, "# diff truncated: %d hunk(s) in %d file(s) omitted to stay under %d bytes; narrow the paths to see them\n",
			omittedHunks, omittedFiles, maxBytes)
	}
	r.Text = text.String()
}

func renderHunk(h Hunk) string {
	var b strings.Builder
	b.WriteString(h.HeaderLine())
	b.WriteByte('\n')
	for _, l := range h.Lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// repoPaths converts caller paths (relative to root, or absolute) into
// paths relative to the repository top level.
func repoPaths(root, top string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	realRoot := absRoot
	if r, err := filepath.EvalSymlinks(absRoot); err == nil {
		realRoot = r
	}
	realTop := top
	if r, err := filepath.EvalSymlinks(top); err == nil {
		realTop = r
	}
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(absRoot, p)
		}
		abs = filepath.Clean(abs)
		candidates := []string{abs}
		if rest, ok := strings.CutPrefix(abs, absRoot); ok && (rest == "" || os.IsPathSeparator(rest[0])) {
			candidates = append(candidates, realRoot+rest)
		}
		found := false
		for _, c := range candidates {
			for _, t := range []string{top, realTop} {
				rel, err := filepath.Rel(t, c)
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					continue
				}
				out = append(out, filepath.ToSlash(rel))
				found = true
				break
			}
			if found {
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("gitx: path %s is outside the repository %s", p, top)
		}
	}
	return out, nil
}

// untrackedDiffs renders untracked files (respecting .gitignore) as
// additions.
func untrackedDiffs(ctx context.Context, top string, paths []string) ([]FileDiff, error) {
	args := append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--full-name", "--"}, paths...)
	res, err := run(ctx, top, runOpts{literal: true}, args...)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range bytes.Split(res.stdout, []byte{0}) {
		// Nested repositories are listed as "dir/"; they are not files.
		if len(n) == 0 || n[len(n)-1] == '/' {
			continue
		}
		names = append(names, string(n))
	}
	sort.Strings(names)
	out := make([]FileDiff, 0, len(names))
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fd, err := untrackedDiff(top, n)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed between listing and reading
			}
			return nil, err
		}
		out = append(out, fd)
	}
	return out, nil
}

func untrackedDiff(top, name string) (FileDiff, error) {
	abs := filepath.Join(top, filepath.FromSlash(name))
	st, err := os.Lstat(abs)
	if err != nil {
		return FileDiff{}, err
	}
	fd := FileDiff{Path: name, Status: StatusAdded, Untracked: true}
	var content []byte
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		fd.NewMode = "120000"
		target, err := os.Readlink(abs)
		if err != nil {
			return FileDiff{}, err
		}
		content = []byte(target)
	case st.Mode().IsRegular():
		fd.NewMode = "100644"
		if st.Mode().Perm()&0o111 != 0 {
			fd.NewMode = "100755"
		}
		if st.Size() > maxUntrackedBytes {
			fd.Note = fmt.Sprintf("untracked file of %d bytes; content not shown", st.Size())
			fd.header = fmt.Sprintf("diff --git %s %s\nnew file mode %s\n", quotePath("a/"+name), quotePath("b/"+name), fd.NewMode)
			return fd, nil
		}
		content, err = os.ReadFile(abs)
		if err != nil {
			return FileDiff{}, err
		}
	default:
		fd.Note = "not a regular file"
		fd.header = fmt.Sprintf("diff --git %s %s\n", quotePath("a/"+name), quotePath("b/"+name))
		return fd, nil
	}

	var h strings.Builder
	fmt.Fprintf(&h, "diff --git %s %s\nnew file mode %s\n", quotePath("a/"+name), quotePath("b/"+name), fd.NewMode)
	if len(content) == 0 {
		fd.header = h.String()
		return fd, nil
	}
	if bytes.IndexByte(content[:min(len(content), 8000)], 0) >= 0 {
		fd.Binary = true
		fmt.Fprintf(&h, "Binary files /dev/null and %s differ\n", quotePath("b/"+name))
		fd.header = h.String()
		return fd, nil
	}
	fmt.Fprintf(&h, "--- /dev/null\n+++ %s\n", quotePath("b/"+name))
	fd.header = h.String()

	text := string(content)
	noEOL := !strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	hunk := Hunk{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: len(lines)}
	hunk.Lines = make([]string, 0, len(lines)+1)
	for _, l := range lines {
		hunk.Lines = append(hunk.Lines, "+"+l)
	}
	if noEOL {
		hunk.Lines = append(hunk.Lines, `\ No newline at end of file`)
	}
	// git prints "@@ -0,0 +1,N @@"; HeaderLine would shorten "-0,0".
	hunk.header = fmt.Sprintf("@@ -0,0 +%s @@", hunkRange(1, len(lines)))
	fd.Hunks = []Hunk{hunk}
	fd.Added = len(lines)
	return fd, nil
}

func hunkRange(start, n int) string {
	if n == 1 {
		return strconv.Itoa(start)
	}
	return fmt.Sprintf("%d,%d", start, n)
}

// parsePatch parses `git diff` output into per-file diffs. It follows hunk
// line counts, so content lines that look like headers ("--- x") are never
// misread.
func parsePatch(text string) ([]FileDiff, error) {
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	var (
		files          []FileDiff
		cur            *FileDiff
		hdr            strings.Builder
		remOld, remNew int
	)
	finish := func() {
		if cur == nil {
			return
		}
		cur.header = hdr.String()
		if cur.Status != StatusRenamed && cur.Status != StatusCopied {
			cur.OldPath = ""
		}
		files = append(files, *cur)
		cur = nil
		hdr.Reset()
	}
	for _, line := range lines {
		if cur != nil && (remOld > 0 || remNew > 0) {
			h := &cur.Hunks[len(cur.Hunks)-1]
			switch {
			case strings.HasPrefix(line, "+"):
				remNew--
				cur.Added++
			case strings.HasPrefix(line, "-"):
				remOld--
				cur.Deleted++
			case strings.HasPrefix(line, `\`):
			default:
				// Context line; an empty line is context whose leading
				// space was stripped by an editor or transport.
				if line == "" {
					line = " "
				}
				remOld--
				remNew--
			}
			h.Lines = append(h.Lines, line)
			continue
		}
		if strings.HasPrefix(line, "diff --git ") {
			finish()
			oldP, newP := parseGitHeader(strings.TrimPrefix(line, "diff --git "))
			cur = &FileDiff{Path: newP, OldPath: oldP, Status: StatusModified}
			hdr.WriteString(line + "\n")
			continue
		}
		if cur == nil {
			continue // preamble such as warnings
		}
		if strings.HasPrefix(line, `\`) && len(cur.Hunks) > 0 {
			h := &cur.Hunks[len(cur.Hunks)-1]
			h.Lines = append(h.Lines, line)
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			h, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			cur.Hunks = append(cur.Hunks, h)
			remOld, remNew = h.OldLines, h.NewLines
			continue
		}
		hdr.WriteString(line + "\n")
		switch {
		case strings.HasPrefix(line, "new file mode "):
			cur.Status = StatusAdded
			cur.NewMode = strings.TrimPrefix(line, "new file mode ")
		case strings.HasPrefix(line, "deleted file mode "):
			cur.Status = StatusDeleted
			cur.OldMode = strings.TrimPrefix(line, "deleted file mode ")
		case strings.HasPrefix(line, "old mode "):
			cur.OldMode = strings.TrimPrefix(line, "old mode ")
		case strings.HasPrefix(line, "new mode "):
			cur.NewMode = strings.TrimPrefix(line, "new mode ")
		case strings.HasPrefix(line, "rename from "):
			cur.Status = StatusRenamed
			cur.OldPath = unquotePath(strings.TrimPrefix(line, "rename from "))
		case strings.HasPrefix(line, "rename to "):
			cur.Status = StatusRenamed
			cur.Path = unquotePath(strings.TrimPrefix(line, "rename to "))
		case strings.HasPrefix(line, "copy from "):
			cur.Status = StatusCopied
			cur.OldPath = unquotePath(strings.TrimPrefix(line, "copy from "))
		case strings.HasPrefix(line, "copy to "):
			cur.Status = StatusCopied
			cur.Path = unquotePath(strings.TrimPrefix(line, "copy to "))
		case strings.HasPrefix(line, "similarity index "):
			cur.Similarity, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line, "similarity index "), "%"))
		case strings.HasPrefix(line, "index "):
			// "index abc..def 100644" carries the mode when unchanged.
			if f := strings.Fields(line); len(f) == 3 {
				if cur.OldMode == "" {
					cur.OldMode = f[2]
				}
				if cur.NewMode == "" {
					cur.NewMode = f[2]
				}
			}
		case strings.HasPrefix(line, "--- "):
			// git appends a tab to names that contain a space.
			if p := strings.TrimSuffix(strings.TrimPrefix(line, "--- "), "\t"); p != "/dev/null" {
				cur.OldPath = stripPrefix(unquotePath(p), "a/")
			}
		case strings.HasPrefix(line, "+++ "):
			if p := strings.TrimSuffix(strings.TrimPrefix(line, "+++ "), "\t"); p != "/dev/null" {
				cur.Path = stripPrefix(unquotePath(p), "b/")
			}
		case strings.HasPrefix(line, "Binary files "), line == "GIT binary patch":
			cur.Binary = true
		}
	}
	if remOld > 0 || remNew > 0 {
		return nil, fmt.Errorf("gitx: diff output ended inside a hunk")
	}
	finish()
	return files, nil
}

// parseHunkHeader parses "@@ -a[,b] +c[,d] @@ section".
func parseHunkHeader(line string) (Hunk, error) {
	bad := fmt.Errorf("gitx: malformed hunk header %q", line)
	rest := strings.TrimPrefix(line, "@@ ")
	end := strings.Index(rest, " @@")
	if end < 0 {
		return Hunk{}, bad
	}
	f := strings.Fields(rest[:end])
	if len(f) != 2 || !strings.HasPrefix(f[0], "-") || !strings.HasPrefix(f[1], "+") {
		return Hunk{}, bad
	}
	parse := func(s string) (int, int, error) {
		a, b, found := strings.Cut(s, ",")
		start, err := strconv.Atoi(a)
		if err != nil {
			return 0, 0, err
		}
		n := 1
		if found {
			if n, err = strconv.Atoi(b); err != nil {
				return 0, 0, err
			}
		}
		return start, n, nil
	}
	h := Hunk{header: line, Section: strings.TrimSpace(rest[end+3:])}
	var err error
	if h.OldStart, h.OldLines, err = parse(f[0][1:]); err != nil {
		return Hunk{}, bad
	}
	if h.NewStart, h.NewLines, err = parse(f[1][1:]); err != nil {
		return Hunk{}, bad
	}
	return h, nil
}

// parseGitHeader extracts both paths from the part of a "diff --git" line
// after the command. Paths may be C-quoted. When unquoted paths contain
// spaces the line is ambiguous; for the common case of identical paths the
// split is exact, and later headers (---, +++, rename) refine the result.
func parseGitHeader(s string) (oldPath, newPath string) {
	if strings.HasPrefix(s, `"`) {
		first, rest := splitQuoted(s)
		rest = strings.TrimPrefix(rest, " ")
		return stripPrefix(first, "a/"), stripPrefix(unquotePath(rest), "b/")
	}
	if strings.HasSuffix(s, `"`) {
		if i := strings.LastIndex(s[:len(s)-1], ` "`); i >= 0 {
			return stripPrefix(s[:i], "a/"), stripPrefix(unquotePath(s[i+1:]), "b/")
		}
	}
	if n := len(s) - 5; n > 0 && n%2 == 0 && strings.HasPrefix(s, "a/") {
		half := n / 2
		if s[2+half:5+half] == " b/" && s[2:2+half] == s[5+half:] {
			p := s[2 : 2+half]
			return p, p
		}
	}
	if i := strings.LastIndex(s, " b/"); i >= 0 {
		return stripPrefix(s[:i], "a/"), s[i+3:]
	}
	return s, s
}

func stripPrefix(p, prefix string) string { return strings.TrimPrefix(p, prefix) }

// splitQuoted returns the unquoted leading C-quoted token of s and the rest.
func splitQuoted(s string) (string, string) {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return unquotePath(s[:i+1]), s[i+1:]
		}
	}
	return unquotePath(s), ""
}

// unquotePath decodes git's C-style path quoting; unquoted input is
// returned unchanged.
func unquotePath(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	s = s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch e := s[i]; e {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'v':
			b.WriteByte('\v')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		case '0', '1', '2', '3':
			if i+2 < len(s) {
				if v, err := strconv.ParseUint(s[i:i+3], 8, 8); err == nil {
					b.WriteByte(byte(v))
					i += 2
					continue
				}
			}
			b.WriteByte(e)
		default:
			b.WriteByte(e)
		}
	}
	return b.String()
}

// quotePath applies git's C-style quoting when a path needs it.
func quotePath(p string) string {
	need := false
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x20 || c == '"' || c == '\\' || c == 0x7f {
			need = true
			break
		}
	}
	if !need {
		return p
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, `\%03o`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
