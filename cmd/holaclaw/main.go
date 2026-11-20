// Command holaclaw is the daemon and its CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"holaclaw/internal/app"
	"holaclaw/internal/config"
)

// The help groups, in the print order of `holaclaw help`.
const (
	groupDaemon  = "daemon"
	groupChat    = "chat"
	groupInspect = "inspect"
	groupAdmin   = "admin"
)

// cli is one invocation: the streams every command reads and writes, and the
// root's --config. Nothing in a command touches os.Stdin/Stdout/Stderr
// directly, so `run` is testable.
type cli struct {
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	configDir string
}

// usageTemplate keeps the shape `holaclaw help` has always had: the usage
// line first, commands under their group, then the flags. The note on where
// the config and the daemon are found is the root's alone.
const usageTemplate = `usage: {{.UseLine}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if .Groups}}{{range $g := .Groups}}

{{$g.Title}}{{range $cmds}}{{if and (eq .GroupID $g.ID) .IsAvailableCommand}}
  {{rpad .Name .NamePadding}} {{.Short}}{{end}}{{end}}{{end}}{{else}}

commands:{{range $cmds}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding}} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

global flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if not .HasParent}}

config directory: --config, $HOLACLAW_CONFIG or /etc/holaclaw; with no /etc/holaclaw, a user's ~/.config/holaclaw
daemon address:   --addr / --token, $HOLACLAW_ADDR / $HOLACLAW_TOKEN, else the config socket{{end}}
`

func init() {
	// The listing follows the declaration order below, not the alphabet.
	cobra.EnableCommandSorting = false
}

// newRoot builds the whole command tree for one invocation.
func newRoot(c *cli) *cobra.Command {
	root := &cobra.Command{
		Use:           "holaclaw <command>",
		Short:         "the daemon and its CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&c.configDir, "config", app.DefaultConfigDir(), "config directory")
	root.AddGroup(
		&cobra.Group{ID: groupDaemon, Title: groupDaemon + ":"},
		&cobra.Group{ID: groupChat, Title: groupChat + ":"},
		&cobra.Group{ID: groupInspect, Title: groupInspect + ":"},
		&cobra.Group{ID: groupAdmin, Title: groupAdmin + ":"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, cmd := range cmds {
			cmd.GroupID = group
			root.AddCommand(cmd)
		}
	}
	add(groupDaemon, newInitCmd(c), newRunCmd(c), newRestartCmd(c), newStatusCmd(c), newBrowserCmd(c), newVersionCmd(c))
	add(groupChat, newChatCmd(c))
	add(groupInspect, newCheckCmd(c), newPromptSizeCmd(c), newTraceCmd(c))
	add(groupAdmin, newLoginCmd(c), newLogoutCmd(c), newConfigCmd(c), newAuthCmd(c), newTokenCmd(c),
		newIdentityCmd(c), newInviteCmd(c), newApprovalsCmd(c), newApproveCmd(c), newDenyCmd(c),
		newMemoryCmd(c), newSkillsCmd(c), newConnectorsCmd(c), newBackupCmd(c))
	root.AddCommand(newCLITreeCmd())

	root.SetHelpCommand(newHelpCmd())
	root.SetCompletionCommandGroupID(groupAdmin)
	root.InitDefaultCompletionCmd()
	if completion, _, err := root.Find([]string{"completion"}); err == nil {
		completion.Short = "print a shell completion script (bash | zsh | fish | powershell)"
	}
	root.SetUsageTemplate(usageTemplate)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error { return &usageError{cmd: cmd, err: err} })
	finish(root)
	return root
}

// finish applies what every command shares: a wrong positional, a missing
// required flag or a forbidden flag pair is a usage error, and a group — a
// command with subcommands and nothing to do itself — names the subcommand
// it does not know, or prints its usage when it got none.
func finish(cmd *cobra.Command) {
	cmd.SuggestionsMinimumDistance = 2
	if cmd.HasSubCommands() && cmd.Run == nil && cmd.RunE == nil {
		cmd.Args = subcommands
		cmd.RunE = func(cmd *cobra.Command, _ []string) error { return usage(cmd) }
	}
	// The flags are parsed by the time Args runs, and cobra's own check of
	// the required flags and the flag groups comes later, returning a plain
	// error: checking here first is what makes them exit with 2.
	positional := cmd.Args
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		var err error
		if positional != nil {
			err = positional(cmd, args)
		}
		if err == nil {
			err = cmd.ValidateRequiredFlags()
		}
		if err == nil {
			err = cmd.ValidateFlagGroups()
		}
		if err == nil || errors.Is(err, pflag.ErrHelp) {
			return err
		}
		return &usageError{cmd: cmd, err: err}
	}
	for _, sub := range cmd.Commands() {
		finish(sub)
	}
}

// required marks flags of cmd that must be given. A name that is not a flag
// of cmd is a programming error.
func required(cmd *cobra.Command, names ...string) {
	for _, name := range names {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(err)
		}
	}
}

// subcommands is the Args of a group: a word that is not one of its
// subcommands is named, with cobra's suggestions. A lone `help` is the
// group's help, as `holaclaw <group> help` has always been.
func subcommands(cmd *cobra.Command, args []string) error {
	switch {
	case len(args) == 0:
		return nil
	case len(args) == 1 && args[0] == "help":
		return pflag.ErrHelp
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
		msg += "\n\nDid you mean this?\n\t" + strings.Join(s, "\n\t")
	}
	return errors.New(msg)
}

// newHelpCmd is `holaclaw help [command]`. Unlike cobra's own it fails on a
// topic it does not know instead of printing the root's help.
func newHelpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "help about any command",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, rest, err := cmd.Root().Find(args)
			if err != nil {
				return &usageError{cmd: cmd.Root(), err: err}
			}
			if len(rest) > 0 {
				return &usageError{cmd: target, err: subcommands(target, rest[:1])}
			}
			return target.Help()
		},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches one invocation and returns the process exit status. main is
// a thin wrapper around it so every command is testable end to end.
//
// 0 is success, and help that was asked for (it goes to stdout); 2 is a
// wrong invocation, whose usage goes to stderr; 1 is any other error.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	c := &cli{stdin: stdin, stdout: stdout, stderr: stderr}
	root := newRoot(c)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if args == nil {
		args = []string{} // nil makes cobra read os.Args
	}
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)

	var code exitError
	var ue *usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	case errors.As(err, &ue):
		if ue.err != nil {
			_, _ = fmt.Fprintf(stderr, "holaclaw: %v\n\n", ue.err)
		}
		_, _ = fmt.Fprint(stderr, ue.cmd.UsageString())
		return 2
	default:
		report(c, err)
		return 1
	}
}

// exitError ends the process with a specific status and prints nothing: the
// command has already said whatever the user needs to read.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// usageError is a wrong invocation of cmd: run prints err, when there is
// one, then the command's usage, and exits 2.
type usageError struct {
	cmd *cobra.Command
	err error
}

func (e *usageError) Error() string {
	if e.err == nil {
		return "usage: " + e.cmd.UseLine()
	}
	return e.err.Error()
}

func (e *usageError) Unwrap() error { return e.err }

// usage reports a wrong invocation of cmd with nothing more to say than its
// usage.
func usage(cmd *cobra.Command) error { return &usageError{cmd: cmd} }

// usagef reports a wrong invocation of cmd with a reason.
func usagef(cmd *cobra.Command, format string, a ...any) error {
	return &usageError{cmd: cmd, err: fmt.Errorf(format, a...)}
}

// report prints config problems one per line with code, location and hint.
func report(c *cli, err error) {
	var ps config.Problems
	if errors.As(err, &ps) {
		for _, p := range ps {
			_, _ = fmt.Fprintf(c.stderr, "%s %s %s: %s\n", p.Code, p.File, p.Key, p.Message)
			if p.Hint != "" {
				_, _ = fmt.Fprintf(c.stderr, "    fix: %s\n", p.Hint)
			}
		}
		return
	}
	_, _ = fmt.Fprintln(c.stderr, "holaclaw:", err)
}
