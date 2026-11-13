package gitx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// CommitOptions selects what Commit records.
type CommitOptions struct {
	// Message is the commit message; it must contain non-blank text.
	Message string
	// All stages every change in the work tree (new, modified and deleted
	// files, honouring .gitignore) before committing.
	All bool
	// Paths stages and commits only these files or directories (relative
	// to the root passed to Commit, or absolute). Other staged changes are
	// left staged and not committed. Ignored when All is set.
	Paths []string
}

// CommitResult describes a new commit.
type CommitResult struct {
	// SHA is the full commit id and Short its abbreviation.
	SHA, Short string
	// Branch is the branch the commit was made on, empty when detached.
	Branch string
	// Subject is the first line of the message.
	Subject string
	// FilesChanged, Insertions and Deletions summarise the commit.
	FilesChanged, Insertions, Deletions int
}

// Commit stages and commits changes in the repository containing root and
// returns the new commit. With neither opts.All nor opts.Paths it commits
// whatever is already staged. It returns ErrNothingToCommit, without
// creating a commit, when the selection has no changes. Repository hooks
// run as usual; a failing hook aborts the commit with git's message.
func Commit(ctx context.Context, root string, opts CommitOptions) (*CommitResult, error) {
	msg := strings.TrimSpace(opts.Message)
	if msg == "" {
		return nil, errors.New("gitx: commit message is empty")
	}
	top, err := TopLevel(ctx, root)
	if err != nil {
		return nil, err
	}
	var paths []string
	if !opts.All && len(opts.Paths) > 0 {
		if paths, err = repoPaths(root, top, opts.Paths); err != nil {
			return nil, err
		}
		if len(paths) == 0 {
			return nil, errors.New("gitx: no paths given to commit")
		}
	}

	switch {
	case opts.All:
		if _, err := run(ctx, top, runOpts{}, "add", "--all"); err != nil {
			return nil, err
		}
	case len(paths) > 0:
		args := append([]string{"add", "--all", "--"}, paths...)
		if _, err := run(ctx, top, runOpts{literal: true}, args...); err != nil {
			return nil, err
		}
	}

	base, err := baseRev(ctx, top)
	if err != nil {
		return nil, err
	}
	check := append([]string{"diff", "--cached", "--quiet", "--no-ext-diff", base, "--"}, paths...)
	res, err := run(ctx, top, runOpts{literal: true, allowExit: []int{1}}, check...)
	if err != nil {
		return nil, err
	}
	if res.exitCode == 0 {
		return nil, ErrNothingToCommit
	}

	args := []string{"commit", "--file=-", "--cleanup=whitespace", "--quiet"}
	if len(paths) > 0 {
		// --only commits just these paths and leaves the rest of the
		// index staged.
		args = append(args, "--only", "--")
		args = append(args, paths...)
	}
	if _, err := run(ctx, top, runOpts{stdin: []byte(msg + "\n"), literal: true}, args...); err != nil {
		var ge *Error
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "Please tell me who you are") {
			return nil, fmt.Errorf("gitx: git does not know who you are; set user.name and user.email (git config user.name \"Your Name\"; git config user.email you@example.com): %w", err)
		}
		return nil, err
	}

	out, err := output(ctx, top, "log", "-1", "--format=%H%x00%h%x00%s", "HEAD")
	if err != nil {
		return nil, err
	}
	f := strings.SplitN(out, "\x00", 3)
	if len(f) != 3 {
		return nil, fmt.Errorf("gitx: unexpected log output %q", out)
	}
	cr := &CommitResult{SHA: f[0], Short: f[1], Subject: f[2]}
	if b, err := output(ctx, top, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		cr.Branch = b
	}
	stat, err := output(ctx, top, "show", "--shortstat", "--format=", "--no-ext-diff", "-M", "HEAD")
	if err == nil {
		cr.FilesChanged, cr.Insertions, cr.Deletions = parseShortstat(stat)
	}
	return cr, nil
}

// parseShortstat parses " 3 files changed, 10 insertions(+), 2 deletions(-)".
func parseShortstat(s string) (files, ins, del int) {
	for _, part := range strings.Split(s, ",") {
		f := strings.Fields(strings.TrimSpace(part))
		if len(f) < 2 {
			continue
		}
		n, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(f[1], "file"):
			files = n
		case strings.HasPrefix(f[1], "insertion"):
			ins = n
		case strings.HasPrefix(f[1], "deletion"):
			del = n
		}
	}
	return files, ins, del
}
