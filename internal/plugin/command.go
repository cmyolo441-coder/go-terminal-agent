package plugin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxCommandOutput caps what an executable command may print.
const maxCommandOutput = 256 << 10

// Command is a plugin slash command.
type Command struct {
	// Name is the name to register, without the slash. It is the
	// manifest name unless that collides with another command, in which
	// case it is the qualified "plugin:name" form.
	Name         string
	Description  string
	ArgumentHint string

	plugin   *Plugin
	manifest CommandManifest
	prog     *program
	timeout  time.Duration
	load     LoadOptions
}

func newCommand(p *Plugin, cm CommandManifest, o LoadOptions) (*Command, error) {
	c := &Command{
		Name:         cm.Name,
		Description:  strings.TrimSpace(cm.Description),
		ArgumentHint: strings.TrimSpace(cm.ArgumentHint),
		plugin:       p,
		manifest:     cm,
		timeout:      seconds(cm.TimeoutSec, DefaultCommandTimeout),
		load:         o,
	}
	if strings.TrimSpace(cm.Command) != "" {
		prog, err := resolveProgram(p.Dir, cm.Command, expandAll(expander(p.Dir, o.Workspace), cm.Args))
		if err != nil {
			return nil, err
		}
		c.prog = &prog
	}
	return c, nil
}

// Plugin is the plugin the command belongs to.
func (c *Command) Plugin() *Plugin { return c.plugin }

// LocalName is the command's name in its manifest.
func (c *Command) LocalName() string { return c.manifest.Name }

// QualifiedName is "plugin:name", which is always unique.
func (c *Command) QualifiedName() string { return c.plugin.Manifest.Name + ":" + c.manifest.Name }

// IsPrompt reports whether the command expands to a prompt for the model
// (true) or runs an executable whose output is shown (false).
func (c *Command) IsPrompt() bool { return c.prog == nil }

// Expand renders a prompt command's template with the given arguments.
func (c *Command) Expand(args string) (string, error) {
	if !c.IsPrompt() {
		return "", fmt.Errorf("/%s runs a program; it has no prompt", c.Name)
	}
	return expandTemplate(c.manifest.Prompt, args)
}

func expandTemplate(tmpl, args string) (string, error) {
	args = strings.TrimSpace(args)
	matches := placeholder.FindAllStringSubmatchIndex(tmpl, -1)
	if len(matches) == 0 {
		if args == "" {
			return tmpl, nil
		}
		return strings.TrimRight(tmpl, "\n") + "\n\n" + args, nil
	}
	var words []string
	needWords := false
	for _, m := range matches {
		if tmpl[m[2]:m[3]] != "args" {
			needWords = true
		}
	}
	if needWords {
		w, err := splitWords(args)
		if err != nil {
			return "", err
		}
		words = w
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(tmpl[last:m[0]])
		name := tmpl[m[2]:m[3]]
		if name == "args" {
			b.WriteString(args)
		} else if n, err := strconv.Atoi(name); err == nil && n >= 1 && n <= len(words) {
			b.WriteString(words[n-1])
		}
		last = m[1]
	}
	b.WriteString(tmpl[last:])
	return b.String(), nil
}

// Run executes an executable command and returns its output for display.
// The arguments are split into shell-style words and appended to the
// manifest args; the raw text is also in $GTA_ARGS.
func (c *Command) Run(ctx context.Context, args string) (string, error) {
	if c.IsPrompt() {
		return "", fmt.Errorf("/%s is a prompt command; expand it instead of running it", c.Name)
	}
	words, err := splitWords(args)
	if err != nil {
		return "", err
	}
	env := c.plugin.procEnv(c.load, map[string]string{"GTA_COMMAND_NAME": c.manifest.Name, "GTA_ARGS": strings.TrimSpace(args)})
	dir := c.load.Workspace
	if dir == "" {
		dir = c.plugin.Dir
	}
	res, err := runProcess(ctx, runSpec{
		prog: *c.prog, extraArgs: words, dir: dir, env: env, timeout: c.timeout, maxOut: maxCommandOutput,
	})
	if err != nil {
		return "", err
	}
	out := strings.ToValidUTF8(strings.TrimRight(string(res.stdout), "\n"), "�")
	switch {
	case res.timedOut:
		return out, fmt.Errorf("/%s timed out after %s%s", c.Name, c.timeout, stderrTail(res.stderr))
	case res.overflow:
		return out + "\n… (output truncated)", nil
	case res.exitCode != 0:
		return out, fmt.Errorf("/%s exited with status %d%s", c.Name, res.exitCode, stderrTail(res.stderr))
	}
	if strings.TrimSpace(out) == "" {
		return "(no output)", nil
	}
	return out, nil
}
