package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
)

// The line-by-line chat: what `chat` draws when the full-screen UI would be
// wrong — a one-shot `-m`, a pipe, a dumb terminal, or `--plain`. It is the
// output scripts have always parsed, so the bytes here are deliberately
// unchanged: one progress line per event on stderr, the reply on stdout, a
// dim info line under it.

// errChatInterrupted ends the chat on a second Ctrl-C.
var errChatInterrupted = errors.New("interrupted")

// plainSink draws a turn as it happens. It also owns the two things a turn
// has to read from: the keyboard, for an approval, and the interrupt signal.
type plainSink struct {
	c     *cli
	opts  chatOptions
	prog  *progress
	t     *theme // stderr: prompts, progress, the info line
	tout  *theme // stdout: the reply and what interrupts it
	stdin *bufio.Reader
	sigc  chan os.Signal
	stop  func()

	canAsk bool // false when nobody is at the keyboard
	said   bool // the "nothing is persisted" note is printed once
}

func newPlainSink(c *cli, o chatOptions, canAsk bool) *plainSink {
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	return &plainSink{
		c: c, opts: o, prog: newProgress(c.stderr, o.quiet, o.verbosity),
		t: newTheme(c.stderr), tout: newTheme(c.stdout),
		stdin: bufio.NewReader(c.stdin), sigc: sigc,
		stop: func() { signal.Stop(sigc) }, canAsk: canAsk,
	}
}

func (p *plainSink) close() { p.stop() }

func (p *plainSink) token(d string) {
	// The first delta is the answer the spinner was standing in for.
	p.prog.stopSpin()
	_, _ = fmt.Fprint(p.c.stdout, d)
}

// event draws progress, and the agent's reaction on the message just sent as
// a line of the reply: it is the answer, or part of it, so it goes to stdout
// even with --quiet.
func (p *plainSink) event(ev gen.Event) {
	if event.Type(ev.Type) == event.ChannelReacted {
		if e := evAttr(ev, "emoji"); e != "" {
			p.prog.stopSpin()
			_, _ = fmt.Fprintln(p.c.stdout, "  ↳ "+e)
		}
	}
	p.prog.event(ev)
}

func (p *plainSink) note(s string) {
	p.prog.stopSpin()
	_, _ = fmt.Fprintln(p.c.stderr, "holaclaw: "+s)
}

func (p *plainSink) reply(r replyInfo) {
	p.prog.stopSpin()
	_, _ = fmt.Fprint(p.c.stdout, r.text)
	_, _ = fmt.Fprintln(p.c.stdout)
	for _, at := range r.attachments {
		_, _ = fmt.Fprintf(p.c.stdout, "[attachment: %s (%s)]\n", at.name, at.mime)
	}
}

// approve asks inline on stderr and reads the answer from the chat's own
// stdin. A closed stdin and a run with nobody at it both deny.
func (p *plainSink) approve(_ context.Context, a approvalReq) approvalAnswer {
	p.prog.stopSpin()
	if !p.canAsk {
		_, _ = fmt.Fprintf(p.c.stderr, "\n%s\n",
			p.t.style(styleRed, "denied, nobody to ask: "+a.summary))
		return approvalAnswer{}
	}
	if a.local && !p.said {
		p.said = true
		_, _ = fmt.Fprintln(p.c.stderr, "\n"+p.t.style(styleDim,
			"(local mode: a choice lasts for this run only — nothing is persisted)"))
	}
	choices := "[y]es once / [s]ession / [a]lways / [n]o:"
	if a.local {
		// There is nowhere to persist an "always" here.
		choices = "[y]es once / [s]ession / [n]o:"
	}
	_, _ = fmt.Fprintf(p.c.stderr, "\n%s ",
		p.t.style(styleApproval, fmt.Sprintf("allow %s? %s", a.summary, choices)))
	line, err := p.stdin.ReadString('\n')
	if line == "" && err != nil {
		return approvalAnswer{} // EOF: deny
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return approvalAnswer{approve: true, choice: choiceOnce}
	case "s", "session":
		return approvalAnswer{approve: true, choice: choiceSession}
	case "a", "always":
		return approvalAnswer{approve: true, choice: choiceAlways}
	case "":
		// An empty line is a yes where the daemon can record it, and a no
		// where a mistyped Enter would otherwise grant something silently.
		if a.local {
			return approvalAnswer{}
		}
		return approvalAnswer{approve: true, choice: choiceOnce}
	}
	return approvalAnswer{}
}

// runChatPlain is the whole line-by-line frontend: one turn for -m, otherwise
// a prompt per line until the person leaves.
func runChatPlain(ctx context.Context, c *cli, eng chatEngine, o chatOptions) error {
	p := newPlainSink(c, o, eng.info().attached || o.once == "")
	defer p.close()

	if o.once != "" {
		return plainTurn(ctx, c, eng, p, o.once)
	}
	banner(c, p.t, o, eng.info().model)
	return promptLoop(c, p.t, p.stdin, func(text string) error {
		err := plainTurn(ctx, c, eng, p, text)
		if errors.Is(err, errChatInterrupted) {
			return err
		}
		if err != nil {
			report(c, err)
		}
		return nil
	})
}

// plainTurn runs one turn and draws it. The turn runs on its own goroutine so
// that Ctrl-C stays this command's own signal to abort it — a second one
// exits — rather than a reason to drop the connection mid-request.
func plainTurn(ctx context.Context, c *cli, eng chatEngine, p *plainSink, text string) error {
	p.prog.begin()
	defer p.prog.stopSpin()
	p.prog.spin()

	type outcome struct {
		res turnResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := eng.run(ctx, text, p)
		done <- outcome{res, err}
	}()

	aborting := false
	for {
		select {
		case <-p.sigc:
			p.prog.stopSpin()
			if aborting {
				_, _ = fmt.Fprintln(c.stderr, "\n"+p.t.style(styleRed, "[exiting]"))
				return errChatInterrupted
			}
			aborting = true
			_, _ = fmt.Fprintln(c.stderr, "\n"+
				p.t.style(styleRed, "[aborting the turn; press Ctrl-C again to exit]"))
			if err := eng.abort(ctx); err != nil {
				return err
			}
		case out := <-done:
			p.prog.stopSpin()
			if out.err != nil {
				return out.err
			}
			if out.res.aborted {
				_, _ = fmt.Fprintln(c.stdout, "\n"+p.tout.style(styleRed, "[aborted]"))
				return nil
			}
			if p.opts.verbosity >= 3 && !p.opts.quiet {
				printToolOutputs(c.stderr, p.t, eng.toolOutputs(ctx, out.res.traceID))
			}
			plainAfter(c, p, out.res)
			return nil
		}
	}
}

// plainAfter closes a turn: the dim line saying what it cost. A one-shot run
// (-m) is somebody's script, so its output stays exactly what it has always
// been — the trace id and nothing else.
func plainAfter(c *cli, p *plainSink, res turnResult) {
	o := p.opts
	if o.once != "" {
		switch {
		case !o.showTrace:
		case res.stop != "":
			// The in-process runner knows its own step count.
			_, _ = fmt.Fprintf(c.stderr, "trace %s · %d step(s) · %s\n", res.traceID, res.steps, res.stop)
		default:
			_, _ = fmt.Fprintf(c.stderr, "trace %s\n", res.traceID)
		}
		return
	}
	trace := ""
	if o.showTrace {
		trace = res.traceID
	}
	if line := p.prog.info(trace); line != "" {
		_, _ = fmt.Fprintln(c.stderr, line)
	}
}

// banner is the one line a chat opens with: who is being talked to and the
// two keys that end things.
func banner(c *cli, t *theme, o chatOptions, model string) {
	s := fmt.Sprintf("holaclaw chat · agent %s · conversation %s", o.agent, o.conversation)
	if model != "" {
		s += " · model " + model
	}
	s += " · /quit to leave, Ctrl-C aborts a turn"
	_, _ = fmt.Fprintln(c.stderr, t.style(styleDim, s))
}

// promptLoop reads a line at a time and hands each one to run, until the
// person leaves. A blank line before every prompt is what separates one turn
// from the next on screen.
func promptLoop(c *cli, t *theme, stdin *bufio.Reader, run func(text string) error) error {
	ed := newLineReader(c, t, stdin)
	defer ed.close()
	prompt := t.style(stylePrompt, "> ")
	for {
		_, _ = fmt.Fprintln(c.stderr)
		line, err := ed.readLine(prompt)
		if errors.Is(err, errPromptQuit) {
			return nil
		}
		text := strings.TrimSpace(line)
		if err != nil && text == "" {
			return nil
		}
		switch text {
		case "":
			if err != nil {
				return nil
			}
			continue
		case "/quit", "/exit":
			return nil
		}
		if rerr := run(text); rerr != nil {
			return nil
		}
		if err != nil {
			return nil
		}
	}
}

// printToolOutputs writes the -vvv block: one heading and indented body per
// tool call, in the order the calls were made. The full-screen UI has no need
// of it — there a tool result is a panel away.
func printToolOutputs(w io.Writer, t *theme, out []toolOutput) {
	for _, o := range out {
		name := o.name
		if name == "" {
			name = "tool"
		}
		_, _ = fmt.Fprintln(w, t.style(styleDim, "  tool output — "+name+":"))
		for _, line := range strings.Split(strings.TrimRight(o.text, "\n"), "\n") {
			_, _ = fmt.Fprintln(w, t.style(styleDim, "    "+line))
		}
	}
}
