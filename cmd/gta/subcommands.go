package main

import (
	"fmt"
	"io"
	"sort"
)

// subcommand is a `gta <name> ...` command other than the interactive
// agent. Files register theirs from init.
type subcommand struct {
	run     func(args []string) error
	summary string
}

var subcommands = map[string]subcommand{}

// registerSubcommand adds a `gta <name>` command. It panics on a duplicate
// name, which is a programming error.
func registerSubcommand(name, summary string, run func(args []string) error) {
	if _, dup := subcommands[name]; dup {
		panic("gta: duplicate subcommand " + name)
	}
	subcommands[name] = subcommand{run: run, summary: summary}
}

// writeSubcommandHelp lists registered subcommands for the usage text.
func writeSubcommandHelp(w io.Writer) {
	names := make([]string, 0, len(subcommands))
	for n := range subcommands {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintln(w, "\nCommands:")
	for _, n := range names {
		fmt.Fprintf(w, "  gta %-12s %s\n", n, subcommands[n].summary)
	}
}
