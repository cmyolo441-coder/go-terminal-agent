// Package prompt builds the system prompt: who the agent is, the
// environment it runs in, and project instructions from memory files.
package prompt

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
)

// MemoryFiles are read from the workspace root, in order, as project
// instructions.
var MemoryFiles = []string{"AGENTS.md", "GTA.md", "CLAUDE.md"}

const maxMemoryBytes = 40_000

const base = `You are GTA, an expert software engineering agent working in the user's terminal, inside their repository. You read code, run commands, and edit files to get real work done: fixing bugs, building features, refactoring, reviewing, and explaining code.

How you work:
- Understand before you change. Find the relevant code with grep, glob and list_dir, and read files before editing them. Follow the conventions, libraries and style already in the codebase.
- Make the change the task needs, completely, with no placeholders or stubbed logic. Keep diffs focused; do not reformat unrelated code.
- Verify your work. Build, run the tests, or run the program when that is possible, and fix what fails. Say plainly what you could not verify.
- For work with several steps, keep a task list with todo_write so the user can follow along.
- Independent reads and searches can be issued together in one turn.
- Never run destructive commands (deleting data, force-pushing, rewriting history) unless the user asked for exactly that. Never commit, push or publish unless asked.
- Never print, log or commit secrets.

How you communicate:
- The user reads you in a terminal. Be direct and concise; lead with the answer or the result. Use Markdown sparingly: short paragraphs, lists for parallel items, fenced code blocks with a language for code.
- Refer to code as path:line so the user can jump to it.
- When you finish, say what changed and how you verified it, in a few lines.`

// Build assembles the full system prompt for a workspace.
func Build(root string) string {
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\n# Environment\n")
	fmt.Fprintf(&b, "- Workspace root: %s\n", root)
	fmt.Fprintf(&b, "- Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "- Date: %s\n", time.Now().Format("2006-01-02"))
	if branch := gitBranch(root); branch != "" {
		fmt.Fprintf(&b, "- Git branch: %s\n", branch)
	} else {
		b.WriteString("- Not a git repository\n")
	}
	if mem := Memory(root); mem != "" {
		b.WriteString("\n# Project instructions\nThese come from the user's memory files. Follow them; they override the defaults above.\n\n")
		b.WriteString(mem)
	}
	return b.String()
}

// Memory returns the combined contents of the user and project memory files.
func Memory(root string) string {
	var parts []string
	paths := []string{filepath.Join(config.UserDir(), "AGENTS.md")}
	for _, f := range MemoryFiles {
		paths = append(paths, filepath.Join(root, f))
	}
	total := 0
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil || len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		if total+len(data) > maxMemoryBytes {
			data = data[:max(0, maxMemoryBytes-total)]
		}
		total += len(data)
		parts = append(parts, fmt.Sprintf("<file path=%q>\n%s\n</file>", p, strings.TrimSpace(string(data))))
		if total >= maxMemoryBytes {
			break
		}
	}
	return strings.Join(parts, "\n\n")
}

// LoadedMemoryFiles lists the memory files present for a workspace.
func LoadedMemoryFiles(root string) []string {
	var out []string
	for _, f := range MemoryFiles {
		if st, err := os.Stat(filepath.Join(root, f)); err == nil && st.Size() > 0 {
			out = append(out, f)
		}
	}
	return out
}

func gitBranch(root string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// InitPrompt asks the agent to write an AGENTS.md for the repository.
const InitPrompt = `Explore this repository and write an AGENTS.md file at the workspace root for future coding agents. Cover: what the project is, how it is laid out, how to build, test, lint and run it (exact commands you verified), coding conventions you observed, and anything surprising. Keep it under 150 lines and factual. If AGENTS.md already exists, improve it rather than replacing useful content.`
