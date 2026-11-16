package main

import (
	"encoding/json"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// cliNode is one command of the tree `holaclaw __cli-tree` prints for
// tools/docgen, which renders docs/reference/cli.md from it instead of
// reading the layout of the help text.
type cliNode struct {
	Path     string    `json:"path"`
	Group    string    `json:"group,omitempty"`
	Short    string    `json:"short"`
	Long     string    `json:"long,omitempty"`
	Usage    string    `json:"usage"`
	Flags    []cliFlag `json:"flags,omitempty"`
	Commands []cliNode `json:"commands,omitempty"`
}

// cliFlag is one flag a command declares itself; the root's --config is on
// the root alone. Defaults are whatever the environment made them: docgen
// runs the binary with no HOLACLAW_* variable set. The root's --config has
// none, since its default depends on the machine (app.DefaultConfigDir).
type cliFlag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage"`
}

// newCLITreeCmd is the hidden `holaclaw __cli-tree`.
func newCLITreeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__cli-tree",
		Short:  "print the command tree as JSON, for tools/docgen",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(cliTreeOf(cmd.Root()))
		},
	}
}

// cliTreeOf walks cmd and its visible subcommands in declaration order.
func cliTreeOf(cmd *cobra.Command) cliNode {
	n := cliNode{Path: cmd.CommandPath(), Group: cmd.GroupID, Short: cmd.Short, Long: cmd.Long, Usage: cmd.UseLine()}
	fs := cmd.LocalNonPersistentFlags()
	if !cmd.HasParent() {
		fs = cmd.PersistentFlags()
	}
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		// A zero default says nothing, as in the help text.
		def := f.DefValue
		if f.Value.Type() != "string" && (def == "false" || def == "0" || def == "0s") {
			def = ""
		}
		if !cmd.HasParent() && f.Name == "config" {
			def = ""
		}
		n.Flags = append(n.Flags, cliFlag{Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(), Default: def, Usage: f.Usage})
	})
	for _, sub := range cmd.Commands() {
		if sub.IsAvailableCommand() {
			n.Commands = append(n.Commands, cliTreeOf(sub))
		}
	}
	return n
}
