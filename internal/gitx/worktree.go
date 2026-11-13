package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// WorktreeDir is where managed worktrees live, relative to the main work
// tree.
const WorktreeDir = ".gta/worktrees"

// BranchPrefix prefixes the branch created for each managed worktree.
const BranchPrefix = "gta/"

// ErrWorktreeExists is returned when a worktree or its branch already exists.
var ErrWorktreeExists = errors.New("worktree already exists")

// ErrWorktreeNotFound is returned when no managed worktree has the name.
var ErrWorktreeNotFound = errors.New("worktree not found")

// ErrWorktreeDirty is returned when removing a worktree with uncommitted
// changes without force.
var ErrWorktreeDirty = errors.New("worktree has uncommitted changes; commit them or remove with force")

// Worktree is one entry of `git worktree list`.
type Worktree struct {
	// Name is the managed worktree name, empty for unmanaged worktrees.
	Name string
	// Path is the absolute work tree directory.
	Path string
	// Branch is the checked-out branch without "refs/heads/", empty when
	// detached or bare.
	Branch string
	// Head is the full sha of the checked-out commit.
	Head string
	// Main is true for the repository's main work tree.
	Main bool
	// Managed is true for worktrees created by CreateWorktree.
	Managed  bool
	Detached bool
	Bare     bool
	// Locked and Prunable carry git's reason text when set; LockedSet and
	// PrunableSet say whether the flag is present at all.
	Locked, Prunable       string
	LockedSet, PrunableSet bool
}

// RemovedWorktree reports what RemoveWorktree did.
type RemovedWorktree struct {
	Path   string
	Branch string
	// BranchDeleted is false when the branch was kept because it has
	// commits not merged into HEAD (and force was not set).
	BranchDeleted bool
	// BranchNote explains why the branch was kept.
	BranchNote string
}

var worktreeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateWorktreeName checks that name is safe both as a directory name and
// as part of a branch name: 1-64 characters of letters, digits, '.', '_'
// and '-', starting with a letter or digit, without "..", and not ending in
// "." or ".lock".
func ValidateWorktreeName(name string) error {
	switch {
	case !worktreeName.MatchString(name):
		return fmt.Errorf("invalid worktree name %q: use 1-64 letters, digits, '.', '_' or '-', starting with a letter or digit", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("invalid worktree name %q: must not contain \"..\"", name)
	case strings.HasSuffix(name, ".") || strings.HasSuffix(strings.ToLower(name), ".lock"):
		return fmt.Errorf("invalid worktree name %q: must not end in \".\" or \".lock\"", name)
	}
	return nil
}

// ListWorktrees lists every worktree of the repository containing root,
// main work tree first.
func ListWorktrees(ctx context.Context, root string) ([]Worktree, error) {
	if err := checkRoot(root); err != nil {
		return nil, err
	}
	out, err := output(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(out), nil
}

func parseWorktrees(out string) []Worktree {
	var (
		list []Worktree
		cur  *Worktree
	)
	flush := func() {
		if cur != nil {
			list = append(list, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			flush()
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		if key == "worktree" {
			flush()
			cur = &Worktree{Path: val}
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "HEAD":
			cur.Head = val
		case "branch":
			cur.Branch = strings.TrimPrefix(val, "refs/heads/")
		case "detached":
			cur.Detached = true
		case "bare":
			cur.Bare = true
		case "locked":
			cur.LockedSet, cur.Locked = true, val
		case "prunable":
			cur.PrunableSet, cur.Prunable = true, val
		}
	}
	flush()
	if len(list) == 0 {
		return nil
	}
	list[0].Main = true
	managedRoot := filepath.Join(list[0].Path, filepath.FromSlash(WorktreeDir))
	for i := range list[1:] {
		w := &list[i+1]
		if filepath.Dir(w.Path) == managedRoot {
			name := filepath.Base(w.Path)
			if ValidateWorktreeName(name) == nil {
				w.Name, w.Managed = name, true
			}
		}
	}
	return list
}

// mainWorktree returns the main work tree's path, refusing bare
// repositories.
func mainWorktree(ctx context.Context, root string) (string, error) {
	list, err := ListWorktrees(ctx, root)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", fmt.Errorf("%s: %w", root, ErrNotRepo)
	}
	if list[0].Bare {
		return "", errors.New("gitx: managed worktrees are not supported in a bare repository")
	}
	return list[0].Path, nil
}

// CreateWorktree creates a worktree at <main work tree>/.gta/worktrees/<name>
// on a new branch gta/<name> starting at HEAD, so an agent can work in
// isolation from the user's checkout. The worktree directory is added to the
// repository's local exclude file so it never shows up as untracked.
func CreateWorktree(ctx context.Context, root, name string) (*Worktree, error) {
	if err := ValidateWorktreeName(name); err != nil {
		return nil, err
	}
	main, err := mainWorktree(ctx, root)
	if err != nil {
		return nil, err
	}
	head, err := headCommit(ctx, main)
	if err != nil {
		return nil, err
	}
	if head == "" {
		return nil, fmt.Errorf("gitx: cannot create a worktree: %w", ErrNoCommits)
	}
	branch := BranchPrefix + name
	path := filepath.Join(main, filepath.FromSlash(WorktreeDir), name)
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrWorktreeExists, path)
	}
	res, err := run(ctx, main, runOpts{allowExit: []int{1}}, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return nil, err
	}
	if res.exitCode == 0 {
		return nil, fmt.Errorf("%w: branch %s already exists", ErrWorktreeExists, branch)
	}
	if err := ensureExcluded(ctx, main); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("gitx: %w", err)
	}
	if _, err := run(ctx, main, runOpts{}, "worktree", "add", "--quiet", "-b", branch, path, head); err != nil {
		return nil, err
	}
	list, err := ListWorktrees(ctx, main)
	if err != nil {
		return nil, err
	}
	for _, w := range list {
		if w.Managed && w.Name == name {
			return &w, nil
		}
	}
	// git may report a symlink-resolved path; fall back to what we know.
	return &Worktree{Name: name, Path: path, Branch: branch, Head: head, Managed: true}, nil
}

// ensureExcluded adds the managed worktree directory to .git/info/exclude
// (shared by all worktrees) unless it is already listed.
func ensureExcluded(ctx context.Context, main string) error {
	common, err := output(ctx, main, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(main, common)
	}
	path := filepath.Join(common, "info", "exclude")
	const pattern = "/" + WorktreeDir + "/"
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("gitx: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("gitx: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("gitx: %w", err)
	}
	add := "# GTA agent worktrees\n" + pattern + "\n"
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		add = "\n" + add
	}
	if _, err := f.WriteString(add); err != nil {
		f.Close()
		return fmt.Errorf("gitx: %w", err)
	}
	return f.Close()
}

// RemoveWorktree removes the managed worktree name and deletes its gta/<name>
// branch. Without force it refuses a worktree with uncommitted changes
// (ErrWorktreeDirty) and keeps a branch whose commits are not merged into
// the main work tree's HEAD; with force both are discarded.
func RemoveWorktree(ctx context.Context, root, name string, force bool) (*RemovedWorktree, error) {
	if err := ValidateWorktreeName(name); err != nil {
		return nil, err
	}
	list, err := ListWorktrees(ctx, root)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 || list[0].Bare {
		return nil, errors.New("gitx: managed worktrees are not supported in a bare repository")
	}
	main := list[0].Path
	var wt *Worktree
	for i := range list {
		if list[i].Managed && list[i].Name == name {
			wt = &list[i]
			break
		}
	}
	if wt == nil {
		return nil, fmt.Errorf("%w: %s", ErrWorktreeNotFound, name)
	}
	out := &RemovedWorktree{Path: wt.Path, Branch: wt.Branch}

	if _, statErr := os.Stat(wt.Path); errors.Is(statErr, os.ErrNotExist) {
		// Deleted by hand: only git's bookkeeping is left.
		if _, err := run(ctx, main, runOpts{}, "worktree", "prune"); err != nil {
			return nil, err
		}
	} else {
		args := []string{"worktree", "remove"}
		if force {
			// Twice also removes a locked worktree.
			args = append(args, "--force", "--force")
		}
		args = append(args, wt.Path)
		if _, err := run(ctx, main, runOpts{}, args...); err != nil {
			var ge *Error
			if errors.As(err, &ge) && (strings.Contains(ge.Stderr, "modified or untracked files") || strings.Contains(ge.Stderr, "use --force")) {
				return nil, fmt.Errorf("%w (%s)", ErrWorktreeDirty, wt.Path)
			}
			return nil, err
		}
	}

	if wt.Branch == "" {
		return out, nil
	}
	flag := "-d"
	if force {
		flag = "-D"
	}
	if _, err := run(ctx, main, runOpts{}, "branch", flag, "--", wt.Branch); err != nil {
		var ge *Error
		if !force && errors.As(err, &ge) && strings.Contains(ge.Stderr, "not fully merged") {
			out.BranchNote = fmt.Sprintf("branch %s kept: it has commits not merged into HEAD (remove with force to delete it)", wt.Branch)
			return out, nil
		}
		return out, err
	}
	out.BranchDeleted = true
	return out, nil
}
