package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permissions"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/session"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

type builtin struct{ name, args, help string }

var builtins = []builtin{
	{"help", "", "Show this help"},
	{"clear", "", "Start a fresh conversation (alias: /new)"},
	{"compact", "[instructions]", "Summarize the conversation to free up context"},
	{"context", "", "Show how full the context window is"},
	{"cost", "", "Show token usage and estimated cost"},
	{"status", "", "Show session, model and configuration details"},
	{"model", "[name]", "Show or switch the model (opus, sonnet, haiku, fable or a full id)"},
	{"effort", "[level]", "Show or set reasoning effort (low, medium, high, xhigh, max)"},
	{"mode", "[mode]", "Show or set the permission mode (default, acceptEdits, plan, bypassPermissions)"},
	{"plan", "", "Toggle plan mode (research only, no changes)"},
	{"permissions", "[allow|deny <rule>]", "Show or add permission rules"},
	{"tools", "", "List available tools"},
	{"mcp", "", "Show MCP servers and their tools"},
	{"todos", "", "Show the current task list"},
	{"bashes", "", "List background shells"},
	{"memory", "", "Show loaded instruction files"},
	{"init", "", "Analyze the project and write " + config.MemoryFile},
	{"review", "[what]", "Review the pending git changes"},
	{"diff", "", "Show git diff --stat"},
	{"resume", "[id]", "Resume an earlier session"},
	{"export", "[file]", "Save the conversation as markdown"},
	{"config", "", "Show settings files and merged values"},
	{"exit", "", "Quit (alias: /quit)"},
}

func (r *REPL) commandNames() []string {
	var out []string
	for _, b := range builtins {
		out = append(out, b.name)
	}
	for _, c := range r.app.Commands {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// expandCustom resolves a custom slash command into its prompt.
func (app *App) expandCustom(line string) (string, bool) {
	name, args, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	for _, c := range app.Commands {
		if c.Name == name {
			body := c.Body
			if strings.Contains(body, "$ARGUMENTS") {
				body = strings.ReplaceAll(body, "$ARGUMENTS", strings.TrimSpace(args))
			} else if strings.TrimSpace(args) != "" {
				body += "\n\n" + strings.TrimSpace(args)
			}
			return body, true
		}
	}
	return "", false
}

const initPrompt = `Analyze this codebase and create an ` + config.MemoryFile + ` file at the project root. Future sessions load it automatically, so it should contain what an engineer new to this repository needs and could not guess:

- The commands to build, lint and test, including how to run a single test.
- The high-level architecture: the main components and how they fit together, at the level that requires reading several files to understand.
- Project-specific conventions that differ from common defaults.

Read the README, build files and key source files first. If ` + config.MemoryFile + ` already exists, improve it instead of replacing it. Keep it concise, do not list every file, and do not invent information that is not in the repository.`

// command runs a slash command. It returns a prompt to send to the agent (if
// the command expands to one) and whether to quit.
func (r *REPL) command(line string) (prompt string, quit bool) {
	a := r.app.Agent
	t := r.term
	name, args, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	args = strings.TrimSpace(args)

	switch name {
	case "exit", "quit", "q":
		return "", true

	case "help", "?":
		t.Println(ui.Bold("Commands"))
		for _, b := range builtins {
			pad := strings.Repeat(" ", max(1, 30-len(b.name)-len(b.args)))
			t.Println("  " + ui.Cyan("/"+b.name) + " " + ui.Dim(b.args) + pad + b.help)
		}
		if len(r.app.Commands) > 0 {
			t.Println(ui.Bold("\nCustom commands"))
			for _, c := range r.app.Commands {
				t.Println("  " + ui.Cyan("/"+c.Name) + strings.Repeat(" ", max(1, 31-len(c.Name))) + c.Description)
			}
		}
		t.Println(ui.Bold("\nInput"))
		t.Println("  @path        attach a file, image or directory listing to the prompt")
		t.Println("  !command     run a shell command yourself; its output is shared with the agent")
		t.Println("  # note       append a note to " + config.MemoryFile)
		t.Println("  line \\       continue the prompt on the next line")
		t.Println("  ctrl+c       interrupt the current turn; twice at the prompt to exit")
		t.Println(ui.Dim("\nCustom commands live in " + config.DirName + "/commands/*.md; settings in " + config.DirName + "/settings.json"))

	case "clear", "new", "reset":
		a.Clear()
		r.pending = nil
		t.Println(ui.Dim("Conversation cleared."))

	case "compact":
		if len(a.Messages) == 0 {
			t.Println(ui.Dim("Nothing to compact yet."))
			break
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		before := a.LastContext
		err := a.Compact(ctx, args)
		stop()
		if err != nil {
			t.Println(ui.Red("Compact failed: " + err.Error()))
			break
		}
		t.Println(ui.Dim(fmt.Sprintf("Conversation compacted (%s → ~%s tokens).", ui.HumanTokens(before), ui.HumanTokens(a.LastContext))))

	case "context":
		w := agent.ContextWindow(a.Model)
		pct := float64(a.LastContext) / float64(w) * 100
		filled := min(int(pct/100*40+0.5), 40)
		bar := ui.Accent(strings.Repeat("█", filled)) + ui.Dim(strings.Repeat("░", 40-filled))
		t.Println(fmt.Sprintf("  %s  %s / %s tokens (%.1f%%)", bar, ui.HumanTokens(a.LastContext), ui.HumanTokens(w), pct))
		t.Println(ui.Dim(fmt.Sprintf("  %d messages · auto-compact at 85%%", len(a.Messages))))

	case "cost":
		in, out, cr, cc, reqs, _ := a.Usage.Snapshot()
		t.Println(fmt.Sprintf("  Estimated cost:   %s", a.Usage.CostString()))
		t.Println(fmt.Sprintf("  Requests:         %d", reqs))
		t.Println(fmt.Sprintf("  Input tokens:     %s (+ %s cache read, %s cache write)", ui.HumanTokens(in), ui.HumanTokens(cr), ui.HumanTokens(cc)))
		t.Println(fmt.Sprintf("  Output tokens:    %s", ui.HumanTokens(out)))

	case "status":
		t.Println(fmt.Sprintf("  %s v%s", config.AppName, config.Version))
		t.Println("  Session:     " + a.Session.ID)
		t.Println("  Transcript:  " + a.Session.Path)
		t.Println("  Directory:   " + r.app.Cwd)
		t.Println("  Model:       " + a.Model + ui.Dim(" (effort "+a.Effort+")"))
		t.Println("  Permissions: " + string(a.Perm.Mode()))
		t.Println(fmt.Sprintf("  Tools:       %d (%d MCP servers) · web search %s", len(a.Tools.List()), len(r.app.MCP), onOff(a.WebSearch)))
		if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" {
			t.Println("  API base:    " + base)
		}

	case "model":
		if args == "" {
			t.Println("  Current model: " + ui.Bold(a.Model))
			var keys []string
			for k := range agent.Aliases {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				t.Println(fmt.Sprintf("    %-8s %s", k, ui.Dim(agent.Aliases[k])))
			}
			t.Println(ui.Dim("  Switch with /model <alias or id>"))
			break
		}
		a.Model = agent.ResolveModel(args)
		t.Println(ui.Dim("Model set to " + a.Model))

	case "effort":
		if args == "" {
			t.Println("  Effort: " + ui.Bold(a.Effort) + ui.Dim("  (options: "+strings.Join(agent.Efforts, ", ")+")"))
			break
		}
		if !validEffort(args) {
			t.Println(ui.Red("Use one of: " + strings.Join(agent.Efforts, ", ")))
			break
		}
		a.Effort = args
		t.Println(ui.Dim("Effort set to " + args))

	case "plan":
		if a.Perm.Mode() == permissions.ModePlan {
			a.Perm.SetMode(permissions.ModeDefault)
			t.Println(ui.Dim("Plan mode off."))
		} else {
			a.Perm.SetMode(permissions.ModePlan)
			t.Println(ui.Dim("Plan mode on: the agent will research and propose a plan without changing anything."))
		}

	case "mode":
		if args == "" {
			t.Println("  Permission mode: " + ui.Bold(string(a.Perm.Mode())))
			t.Println(ui.Dim("  default (ask before edits and commands) · acceptEdits (edit freely, ask for commands)"))
			t.Println(ui.Dim("  plan (read-only) · bypassPermissions (never ask)"))
			break
		}
		m, ok := permissions.ParseMode(args)
		if !ok {
			t.Println(ui.Red("Unknown mode. Use default, acceptEdits, plan or bypassPermissions."))
			break
		}
		a.Perm.SetMode(m)
		t.Println(ui.Dim("Permission mode set to " + string(m)))

	case "permissions":
		verb, rule, _ := strings.Cut(args, " ")
		rule = strings.TrimSpace(rule)
		switch {
		case verb == "allow" && rule != "":
			a.Perm.AddAllow(rule)
			if err := config.AddLocalAllowRule(r.app.Cwd, rule); err != nil {
				t.Println(ui.Red("Added for this session, but could not save: " + err.Error()))
			} else {
				t.Println(ui.Dim("Allowed " + rule + " (saved to " + config.DirName + "/settings.local.json)"))
			}
		case verb == "deny" && rule != "":
			a.Perm.AddDeny(rule)
			t.Println(ui.Dim("Denied " + rule + " for this session"))
		default:
			allow, deny, ask := a.Perm.Rules()
			t.Println("  Mode: " + ui.Bold(string(a.Perm.Mode())))
			show := func(title string, rules []string) {
				t.Println("  " + title + ":")
				if len(rules) == 0 {
					t.Println(ui.Dim("    (none)"))
				}
				for _, x := range rules {
					t.Println("    " + x)
				}
			}
			show("Allow", allow)
			show("Deny", deny)
			show("Ask", ask)
			t.Println(ui.Dim("  Add with /permissions allow Bash(npm test:*)  or  /permissions deny Read(.env)"))
		}

	case "tools":
		for _, tool := range a.Tools.List() {
			desc, _, _ := strings.Cut(tool.Description, "\n")
			t.Println("  " + ui.Cyan(tool.Name) + strings.Repeat(" ", max(1, 26-len(tool.Name))) + ui.Dim(ui.Truncate(desc, 90)))
		}
		if a.WebSearch {
			t.Println("  " + ui.Cyan("web_search") + strings.Repeat(" ", 16) + ui.Dim("Searches the web (runs on the API side)"))
		}

	case "mcp":
		if len(r.app.Settings.MCPServers) == 0 {
			t.Println(ui.Dim("  No MCP servers configured. Add them under \"mcpServers\" in .mcp.json or " + config.DirName + "/settings.json."))
			break
		}
		for _, c := range r.app.MCP {
			t.Println("  " + ui.Green("● ") + ui.Bold(c.Name) + ui.Dim(fmt.Sprintf(" (%d tools)", len(c.Tools))))
			for _, tool := range c.Tools {
				t.Println("      " + tool.Name + ui.Dim("  "+ui.Truncate(strings.ReplaceAll(tool.Description, "\n", " "), 80)))
			}
		}
		for name, err := range r.app.MCPErrs {
			t.Println("  " + ui.Red("● ") + ui.Bold(name) + ui.Red("  "+err.Error()))
		}

	case "todos":
		t.Println(tools.RenderTodos(a.Env.Todos()))

	case "bashes":
		list := a.Env.Shells.List()
		if len(list) == 0 {
			t.Println(ui.Dim("  No background shells."))
		}
		for _, l := range list {
			t.Println("  " + l)
		}

	case "memory":
		mem := config.LoadMemory(r.app.Cwd)
		if len(mem) == 0 {
			t.Println(ui.Dim("  No instruction files found. Run /init to create " + config.MemoryFile + "."))
		}
		for _, m := range mem {
			t.Println(fmt.Sprintf("  %s %s", m.Path, ui.Dim(fmt.Sprintf("(%d lines)", strings.Count(m.Content, "\n")+1))))
		}

	case "init":
		return initPrompt, false

	case "review":
		p := "Review the pending changes in this git repository (staged, unstaged and untracked). Read the diff and the surrounding code, then report correctness bugs, risky changes and missing tests, most severe first, with path:line references. Do not modify any files."
		if args != "" {
			p += " Focus on: " + args
		}
		return p, false

	case "diff":
		cmd := exec.Command("git", "--no-pager", "diff", "--stat", "HEAD")
		cmd.Dir = r.app.Cwd
		out, err := cmd.CombinedOutput()
		switch {
		case err != nil:
			t.Println(ui.Red(strings.TrimSpace(string(out))))
		case len(out) == 0:
			t.Println(ui.Dim("  No changes against HEAD."))
		default:
			t.Println(strings.TrimRight(string(out), "\n"))
		}

	case "resume", "sessions":
		r.resume(args)

	case "export":
		path := args
		if path == "" {
			path = "conversation-" + a.Session.ID + ".md"
		}
		if err := os.WriteFile(path, []byte(renderTranscript(a.Messages)), 0o644); err != nil {
			t.Println(ui.Red("Export failed: " + err.Error()))
		} else {
			t.Println(ui.Dim("Saved " + path))
		}

	case "config":
		for _, p := range config.SettingsPaths(r.app.Cwd) {
			mark := ui.Dim("(not present)")
			if _, err := os.Stat(p); err == nil {
				mark = ui.Green("(loaded)")
			}
			t.Println("  " + p + " " + mark)
		}
		b, _ := json.MarshalIndent(r.app.Settings, "  ", "  ")
		t.Println(ui.Dim("  Merged settings:"))
		t.Println("  " + string(b))

	default:
		if p, ok := r.app.expandCustom(line); ok {
			return p, false
		}
		t.Println(ui.Red("Unknown command /" + name + ". Type /help for the list."))
	}
	return "", false
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (r *REPL) resume(arg string) {
	t := r.term
	list := session.List(r.app.Cwd)
	if len(list) == 0 {
		t.Println(ui.Dim("  No saved sessions for this directory."))
		return
	}
	var pick session.Info
	if arg != "" {
		var err error
		if pick, err = session.Find(r.app.Cwd, arg); err != nil {
			t.Println(ui.Red(err.Error()))
			return
		}
	} else {
		if len(list) > 15 {
			list = list[:15]
		}
		for i, s := range list {
			title := s.Title
			if title == "" {
				title = "(untitled)"
			}
			t.Println(fmt.Sprintf("  %2d. %s  %s  %s", i+1, ui.Dim(s.Modified.Format("Jan 02 15:04")),
				ui.Truncate(title, 60), ui.Dim(fmt.Sprintf("(%d msgs)", s.Messages))))
		}
		ans, ok := t.readLine("  Resume which? (number, enter to cancel) ❯ ")
		n, err := strconv.Atoi(ans)
		if !ok || err != nil || n < 1 || n > len(list) {
			return
		}
		pick = list[n-1]
	}
	s, msgs, err := session.Load(pick.Path)
	if err != nil {
		t.Println(ui.Red("Cannot load session: " + err.Error()))
		return
	}
	a := r.app.Agent
	old, oldMsgs := a.Session, a.Messages
	if err := a.LoadHistory(msgs); err != nil {
		a.Session, a.Messages = old, oldMsgs
		t.Println(ui.Red("Cannot restore session: " + err.Error()))
		return
	}
	a.Session, a.LastContext = s, 0
	t.Println(ui.Dim(fmt.Sprintf("Resumed %s (%d messages)", s.ID, len(msgs))))
}

type msgBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// decodeMessage reads a stored message through its JSON form, which keeps this
// code independent of the SDK's parameter union types.
func decodeMessage(m anthropic.BetaMessageParam) (role string, blocks []msgBlock) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", nil
	}
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw.Content, &s) == nil {
		return raw.Role, []msgBlock{{Type: "text", Text: s}}
	}
	_ = json.Unmarshal(raw.Content, &blocks)
	return raw.Role, blocks
}

func renderTranscript(msgs []anthropic.BetaMessageParam) string {
	var sb strings.Builder
	sb.WriteString("# Conversation\n")
	for _, m := range msgs {
		role, blocks := decodeMessage(m)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.HasPrefix(b.Text, "<system-reminder>") {
					continue
				}
				title := "Assistant"
				if role == "user" {
					title = "User"
				}
				fmt.Fprintf(&sb, "\n## %s\n\n%s\n", title, strings.TrimSpace(b.Text))
			case "tool_use", "server_tool_use":
				fmt.Fprintf(&sb, "\n**Tool call: %s**\n\n```json\n%s\n```\n", b.Name, string(b.Input))
			case "tool_result":
				var text string
				if json.Unmarshal(b.Content, &text) != nil {
					var parts []msgBlock
					_ = json.Unmarshal(b.Content, &parts)
					for _, p := range parts {
						text += p.Text
					}
				}
				label := "Tool result"
				if b.IsError {
					label = "Tool error"
				}
				fmt.Fprintf(&sb, "\n**%s**\n\n```\n%s\n```\n", label, ui.Truncate(text, 4000))
			}
		}
	}
	return sb.String()
}
