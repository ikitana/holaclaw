package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The editor is driven here exactly as a terminal drives it — a byte stream
// in, a line and a cursor out — so none of this needs a real tty.

// feed pushes every keystroke in b through the editor and returns the last
// action it asked for.
func feed(t *testing.T, e *lineEditor, b string) editAction {
	t.Helper()
	act := editContinue
	rest := []byte(b)
	for len(rest) > 0 {
		k, n, ok := decodeKey(rest)
		if !ok {
			t.Fatalf("incomplete key sequence in %q at %q", b, rest)
		}
		rest = rest[n:]
		act = e.apply(k)
	}
	return act
}

// state is the buffer and the cursor, the two things a caller can see.
func state(e *lineEditor) (string, int) { return e.String(), e.pos }

const (
	ctrlA = "\x01"
	ctrlB = "\x02"
	ctrlC = "\x03"
	ctrlD = "\x04"
	ctrlE = "\x05"
	ctrlF = "\x06"
	ctrlK = "\x0b"
	ctrlL = "\x0c"
	ctrlU = "\x15"
	ctrlW = "\x17"
	bs    = "\x7f"
	left  = "\x1b[D"
	right = "\x1b[C"
	up    = "\x1b[A"
	down  = "\x1b[B"
	home  = "\x1b[H"
	end   = "\x1b[F"
	del   = "\x1b[3~"
	altB  = "\x1bb"
	altF  = "\x1bf"
)

func TestLineEditorMotionAndEditing(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
		pos  int
	}{
		{"plain typing", "hola", "hola", 4},
		{"ctrl-a then typing", "hola" + ctrlA + "> ", "> hola", 2},
		{"home is ctrl-a", "hola" + home + "!", "!hola", 1},
		{"ctrl-e after ctrl-a", "hola" + ctrlA + ctrlE + "!", "hola!", 5},
		{"end is ctrl-e", "hola" + home + end + "!", "hola!", 5},
		{"ctrl-b and ctrl-f", "hola" + ctrlB + ctrlB + ctrlF + "X", "holXa", 4},
		{"arrows move too", "hola" + left + left + right + "X", "holXa", 4},
		{"backspace", "hola" + bs, "hol", 3},
		{"backspace at the margin does nothing", ctrlA + bs + "x", "x", 1},
		{"delete forward", "hola" + ctrlA + del, "ola", 0},
		{"delete at the end does nothing", "hola" + del, "hola", 4},
		{"ctrl-k kills to the end", "hola mundo" + ctrlA + ctrlF + ctrlF + ctrlF + ctrlF + ctrlK, "hola", 4},
		{"ctrl-u kills to the start", "hola mundo" + altB + ctrlU, "mundo", 0},
		{"ctrl-w kills a word", "hola mundo" + ctrlW, "hola ", 5},
		{"ctrl-w twice", "hola mundo" + ctrlW + ctrlW, "", 0},
		{"alt-b then alt-f", "hola mundo" + altB + altB + altF + "|", "hola| mundo", 5},
		{"ctrl-d over text deletes forward", "hola" + ctrlA + ctrlD, "ola", 0},
		{"a rune is one cursor step", "añó" + left + left + "X", "aXñó", 2},
		{"backspace deletes a whole rune", "añó" + bs, "añ", 2},
		{"multi-byte home and end", "añó" + ctrlA + "¡" + ctrlE + "!", "¡añó!", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLineEditor()
			feed(t, e, tc.in)
			got, pos := state(e)
			if got != tc.want || pos != tc.pos {
				t.Errorf("buffer = %q at %d, want %q at %d", got, pos, tc.want, tc.pos)
			}
		})
	}
}

func TestLineEditorSubmitAndHistory(t *testing.T) {
	e := newLineEditor()
	for _, line := range []string{"first question", "second question"} {
		feed(t, e, line)
		if act := feed(t, e, "\r"); act != editSubmit {
			t.Fatalf("Enter returned %v, want editSubmit", act)
		}
		if e.String() != line {
			t.Fatalf("submitted %q, want %q", e.String(), line)
		}
		e.remember(e.String())
		e.reset()
	}
	if len(e.history) != 2 {
		t.Fatalf("history = %q", e.history)
	}

	// Up walks back, down walks forward, and stepping off the end brings the
	// half-written line back.
	feed(t, e, "half")
	feed(t, e, up)
	if got, pos := state(e); got != "second question" || pos != len("second question") {
		t.Errorf("one up = %q at %d", got, pos)
	}
	feed(t, e, up)
	if got, _ := state(e); got != "first question" {
		t.Errorf("two up = %q", got)
	}
	feed(t, e, up) // the top holds
	if got, _ := state(e); got != "first question" {
		t.Errorf("past the top = %q", got)
	}
	feed(t, e, down+down)
	if got, _ := state(e); got != "half" {
		t.Errorf("back at the bottom = %q, want the draft", got)
	}
	feed(t, e, down) // the bottom holds too
	if got, _ := state(e); got != "half" {
		t.Errorf("past the bottom = %q", got)
	}

	// Nothing worth a slot is kept.
	e.reset()
	e.remember("   ")
	e.remember("first question")
	e.remember("first question")
	if len(e.history) != 3 || e.history[2] != "first question" {
		t.Errorf("history = %q", e.history)
	}
}

func TestLineEditorCtrlCAndCtrlD(t *testing.T) {
	e := newLineEditor()
	feed(t, e, "hola")
	if act := feed(t, e, ctrlC); act != editCancel {
		t.Errorf("Ctrl-C on a line = %v, want editCancel", act)
	}
	if got, pos := state(e); got != "" || pos != 0 {
		t.Errorf("the line survived Ctrl-C: %q at %d", got, pos)
	}
	if act := feed(t, e, ctrlC); act != editInterrupt {
		t.Errorf("Ctrl-C on an empty line = %v, want editInterrupt", act)
	}
	if act := feed(t, e, ctrlD); act != editEOF {
		t.Errorf("Ctrl-D on an empty line = %v, want editEOF", act)
	}
	if act := feed(t, e, ctrlL); act != editClear {
		t.Errorf("Ctrl-L = %v, want editClear", act)
	}
}

func TestDecodeKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want keyKind
		n    int
	}{
		{"ctrl-a", ctrlA, keyHome, 1},
		{"ctrl-e", ctrlE, keyEnd, 1},
		{"ctrl-c", ctrlC, keyInterrupt, 1},
		{"enter", "\r", keyEnter, 1},
		{"newline", "\n", keyEnter, 1},
		{"backspace", bs, keyBackspace, 1},
		{"left", left, keyLeft, 3},
		{"delete", del, keyDelete, 4},
		{"home as CSI 1~", "\x1b[1~", keyHome, 4},
		{"end as CSI 4~", "\x1b[4~", keyEnd, 4},
		{"home as SS3", "\x1bOH", keyHome, 3},
		{"alt-b", altB, keyWordLeft, 2},
		{"ctrl-right is a word", "\x1b[1;5C", keyWordRight, 6},
		{"a rune", "ñ", keyRune, 2},
		{"an unknown CSI", "\x1b[200~", keyNone, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, n, ok := decodeKey([]byte(tc.in))
			if !ok {
				t.Fatalf("decodeKey(%q) is incomplete", tc.in)
			}
			if k.kind != tc.want || n != tc.n {
				t.Errorf("decodeKey(%q) = kind %v, %d bytes; want %v, %d", tc.in, k.kind, n, tc.want, tc.n)
			}
		})
	}

	// A sequence that is only half read asks for more bytes rather than
	// guessing at it.
	for _, in := range []string{"", "\x1b", "\x1b[", "\x1b[1;5", "\xc3"} {
		if _, _, ok := decodeKey([]byte(in)); ok {
			t.Errorf("decodeKey(%q) claimed to be complete", in)
		}
	}
}

func TestLineEditorRender(t *testing.T) {
	e := newLineEditor()
	feed(t, e, "hola")
	if got := e.render("> ", 0); got != "\r\x1b[K> hola" {
		t.Errorf("render = %q", got)
	}
	feed(t, e, ctrlA)
	got := e.render("> ", 0)
	if !strings.HasSuffix(got, "\x1b[4D") {
		t.Errorf("render with the cursor at the start = %q", got)
	}
}

// TestLineEditorRenderWraps is the terminal-window-width bug: once
// prompt+line reaches the window's column count, the terminal wraps it onto
// a second row that a single-row "\r\x1b[K" clear never reaches, so the
// previous draw stays on screen and every further keystroke appears to pile
// the line up again. A width-aware render must climb back above every row
// it used last time and clear all of them before redrawing.
func TestLineEditorRenderWraps(t *testing.T) {
	e := newLineEditor()
	const width = 10

	feed(t, e, "1234567") // "> 1234567" is exactly 9 columns: still one row
	if got := e.render("> ", width); strings.Contains(got, "\x1b[1A") || !strings.Contains(got, "\x1b[J") {
		t.Errorf("single-row render should not climb rows: %q", got)
	}

	feed(t, e, "89") // "> 123456789" is 11 columns: now wraps to a second row
	got := e.render("> ", width)
	if !strings.Contains(got, "\x1b[J") {
		t.Errorf("wrapped render must clear to end of screen, not just the current row: %q", got)
	}

	feed(t, e, "0") // one more keystroke on the wrapped line
	got = e.render("> ", width)
	if !strings.Contains(got, "\x1b[1A") {
		t.Errorf("redrawing a two-row block should climb back to its top row: %q", got)
	}
	if strings.Contains(got, "\x1b[K") {
		t.Errorf("wrapped render should not fall back to single-row clearing: %q", got)
	}
}

// TestLineEditorRenderColoredPrompt is the chat prompt in practice: theme.style
// wraps "> " in SGR escapes ("\x1b[1;36m> \x1b[0m"). Those bytes print but take
// no column, so counting them toward the line's width would both misplace the
// cursor and, once the miscount crosses the real window width a few keystrokes
// early, send a bogus cursor-up that climbs into whatever was printed above the
// prompt and erases it.
func TestLineEditorRenderColoredPrompt(t *testing.T) {
	plain := "> "
	colored := "\x1b[1;36m> \x1b[0m"
	const width = 10

	for n := 1; n <= 12; n++ {
		p := newLineEditor()
		c := newLineEditor()
		feed(t, p, strings.Repeat("x", n))
		feed(t, c, strings.Repeat("x", n))

		pGot := p.render(plain, width)
		cGot := c.render(colored, width)

		if p.prevRows != c.prevRows {
			t.Fatalf("n=%d: prevRows = %d for plain prompt, %d for colored", n, p.prevRows, c.prevRows)
		}
		// Strip the colored prompt's own escapes back out so the cursor
		// placement (trailing motion) can be compared like for like.
		cGot = strings.Replace(cGot, colored, plain, 1)
		if pGot != cGot {
			t.Errorf("n=%d: plain render = %q, colored render (escapes stripped) = %q", n, pGot, cGot)
		}
	}
}

func TestVisibleWidth(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want int
	}{
		{"> ", 2},
		{"\x1b[1;36m> \x1b[0m", 2},
		{"añó", 3},
	} {
		if got := visibleWidth(tc.s); got != tc.want {
			t.Errorf("visibleWidth(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

func TestWrapPos(t *testing.T) {
	for _, tc := range []struct {
		idx, width int
		row, col   int
	}{
		{0, 10, 0, 0},
		{5, 10, 0, 5},
		{10, 10, 0, 10}, // exactly fills the row: cursor sits at its last column
		{11, 10, 1, 1},  // one more column: now on the second row
		{20, 10, 1, 10}, // exactly fills the second row too
		{21, 10, 2, 1},
	} {
		row, col := wrapPos(tc.idx, tc.width)
		if row != tc.row || col != tc.col {
			t.Errorf("wrapPos(%d, %d) = (%d, %d), want (%d, %d)", tc.idx, tc.width, row, col, tc.row, tc.col)
		}
	}
}

// --- the raw-mode plumbing ------------------------------------------------------

// openPty is a terminal to drive the reader with: the master is what a person
// types on, the slave is what the prompt reads.
func openPty(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("TIOCGPTN: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("TIOCSPTLCK: %v", err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open the pty slave: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return m, s
}

// typeWhenRaw types s on the master once readLine has put the slave in raw
// mode. Typed any earlier, the cooked line discipline gets it first: a Ctrl-C
// there is a signal that flushes the input, and readLine waits forever.
func typeWhenRaw(master, slave *os.File, s string) {
	go func() {
		for {
			tio, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil || tio.Lflag&unix.ICANON == 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		_, _ = master.WriteString(s)
	}()
}

// This is the plumbing the pure tests above do not reach: raw mode on, a line
// read, the terminal handed back exactly as it was found.
func TestTtyLinesReadsALineAndRestoresTheTerminal(t *testing.T) {
	master, slave := openPty(t)
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Skipf("TCGETS on the pty: %v", err)
	}
	l := &ttyLines{in: slave, fd: int(slave.Fd()), out: io.Discard, t: &theme{}, ed: newLineEditor()}

	typeWhenRaw(master, slave, "hola"+ctrlA+"¿"+ctrlE+"?\r")
	line, err := l.readLine("> ")
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if line != "¿hola?" {
		t.Errorf("line = %q, want %q", line, "¿hola?")
	}
	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("TCGETS after: %v", err)
	}
	if after.Lflag != before.Lflag || after.Iflag != before.Iflag {
		t.Errorf("the terminal was left in raw mode: lflag %x→%x, iflag %x→%x",
			before.Lflag, after.Lflag, before.Iflag, after.Iflag)
	}

	// A Ctrl-C on a line clears it; a second one on the empty line leaves.
	typeWhenRaw(master, slave, "half a thought"+ctrlC+ctrlC+ctrlC)
	if line, err := l.readLine("> "); !errors.Is(err, errPromptQuit) {
		t.Errorf("Ctrl-C twice = %q, %v; want errPromptQuit", line, err)
	}
}
