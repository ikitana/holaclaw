package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"holaclaw/internal/termutil"
)

// chatOptions is what every mode needs.
type chatOptions struct {
	dir          string
	agent        string
	resume       string // --resume: join this conversation, or pickConversation to ask which
	cont         bool   // --continue: join the most recent one
	conversation string // the one the chat joined, once it has
	once         string
	plain        bool
	showTrace    bool
	quiet        bool
	verbosity    int // 0 (default), 1 (-v), 2 (-vv), 3 (-vvv) — plain mode only
}

// pickConversation is --resume given without a conversation: list the recent
// ones and ask.
const pickConversation = "\x00pick"

// newChatCmd is `holaclaw chat`. -m, -q, -r and -c are the shorthands of
// --message, --quiet, --resume and --continue, and -v is a count: -vv is two
// of them.
//
// --resume takes its value optionally, which pflag only understands as
// --resume=<conversation>; the conversation in `--resume <conversation>`
// arrives as an argument instead, and is accepted only right there.
func newChatCmd(c *cli) *cobra.Command {
	var o chatOptions
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "talk to an agent in the terminal",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || len(args) == 1 && o.resume == pickConversation {
				return nil
			}
			return cobra.NoArgs(cmd, args)
		},
	}
	cn := c.connFlags(cmd)
	fs := cmd.Flags()
	fs.StringVar(&o.agent, "agent", "main", "agent name")
	fs.StringVarP(&o.resume, "resume", "r", "",
		"resume this conversation instead of starting a new one; without one, choose from the most recent")
	fs.Lookup("resume").NoOptDefVal = pickConversation
	fs.BoolVarP(&o.cont, "continue", "c", false, "resume the most recent conversation instead of starting a new one")
	cmd.MarkFlagsMutuallyExclusive("resume", "continue")
	fs.StringVarP(&o.once, "message", "m", "", "send one message and exit")
	fs.BoolVar(&o.plain, "plain", false, "line-by-line output instead of the full-screen UI")
	fs.BoolVar(&o.showTrace, "trace", false, "print the trace id after each reply")
	fs.BoolVarP(&o.quiet, "quiet", "q", false, "no progress lines while a turn runs")
	fs.CountVarP(&o.verbosity, "verbose", "v",
		"-v also shows tool calls and results, -vv tool arguments and browser moves, -vvv full tool responses "+
			"(--plain only; the UI has a details panel)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			o.resume = args[0]
		}
		o.dir = *cn.dir
		o.verbosity = min(o.verbosity, 3)
		return cmdChat(cmd.Context(), c, cn, o)
	}
	return cmd
}

// cmdChat talks to an agent in the terminal. With a daemon running it attaches
// over the Control API like any other client; without one it runs the agent
// loop in this process, which is the M0 path and still the way to try a config
// before starting a service.
//
// At a terminal it draws the full-screen UI in tui.go; anywhere else — a pipe,
// a one-shot -m, a dumb terminal, --plain — it falls back to the line-by-line
// output in chatplain.go, which is what scripts have always read.
func cmdChat(ctx context.Context, c *cli, cn *conn, o chatOptions) error {
	// Ctrl-C is this command's own signal to abort the running turn, not a
	// reason to drop the connection mid-request, so the context that reaches
	// the engine is detached on purpose.
	detached := context.WithoutCancel(ctx)

	pick := func(convs []conversationInfo) (string, error) {
		return askConversation(c, o.agent, convs)
	}
	var eng chatEngine
	client, err := cn.dial(ctx)
	switch {
	case err == nil:
		var conversation string
		conversation, err = chooseConversation(o, func() ([]conversationInfo, error) {
			return attachedConversations(ctx, client, o.agent)
		}, pick)
		if err == nil {
			eng, err = newAttachedEngine(detached, client, o.agent, conversation)
		}
	case errors.Is(err, errNoDaemon):
		_, _ = fmt.Fprintln(c.stderr, "holaclaw: the daemon is not running; chatting in this process instead")
		eng, err = newLocalEngine(detached, o, pick)
	}
	if err != nil {
		return err
	}
	defer func() { _ = eng.close() }()
	o.conversation = eng.info().conversation

	if fullScreen(c, o) {
		return runChatTUI(detached, c, eng, o)
	}
	return runChatPlain(detached, c, eng, o)
}

// fullScreen decides which UI this invocation gets. The full-screen one needs
// a terminal at both ends and somebody to drive it: a one-shot run, a pipe on
// either side or a terminal that cannot draw is the line-by-line UI's job.
func fullScreen(c *cli, o chatOptions) bool {
	if o.once != "" || o.plain {
		return false
	}
	if v, _ := os.LookupEnv("TERM"); v == "dumb" {
		return false
	}
	return isTerminal(c.stdout) && isTerminalReader(c.stdin)
}

// isTerminalReader is isTerminal for the input side.
func isTerminalReader(r io.Reader) bool {
	f, ok := r.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	return termutil.IsTerminal(int(f.Fd()))
}

// slashCommand splits a typed line into a command and its argument, or
// returns ok=false when the line is an ordinary message. Both UIs read the
// same vocabulary, so /quit means the same thing in each.
func slashCommand(line string) (name, arg string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") || line == "/" {
		return "", "", false
	}
	name, arg, _ = strings.Cut(line[1:], " ")
	return strings.ToLower(name), strings.TrimSpace(arg), true
}
