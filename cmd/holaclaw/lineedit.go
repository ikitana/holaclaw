package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"holaclaw/internal/termutil"
)

// The chat prompt when a human is at a terminal: raw mode, one line, the
// motions a shell has taught everybody. It is split in two on purpose — a
// lineEditor that is a pure function of the bytes it is fed (what the tests
// drive), and the raw-mode plumbing around it (what needs a terminal).
//
// The history lives in this struct and nowhere else: a chat line can hold
// anything the person was about to tell their agent, so it is never written
// to disk.

// errPromptQuit ends the prompt loop the way /quit does: Ctrl-D on an empty
// line, or a second Ctrl-C.
var errPromptQuit = errors.New("leaving the chat")

// --- keys -------------------------------------------------------------------

type keyKind int

const (
	keyNone keyKind = iota // a sequence with no meaning here
	keyRune
	keyEnter
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyWordLeft
	keyWordRight
	keyKillToEnd  // Ctrl-K
	keyKillToHome // Ctrl-U
	keyKillWord   // Ctrl-W
	keyClear      // Ctrl-L
	keyInterrupt  // Ctrl-C
	keyEOF        // Ctrl-D
)

// key is one decoded keystroke; r is set only for keyRune.
type key struct {
	kind keyKind
	r    rune
}

// decodeKey reads the first keystroke out of b. It returns the key and how
// many bytes it used, or ok=false when b holds only the start of a sequence
// and the caller must read more.
func decodeKey(b []byte) (key, int, bool) {
	if len(b) == 0 {
		return key{}, 0, false
	}
	switch c := b[0]; c {
	case 0x01:
		return key{kind: keyHome}, 1, true
	case 0x02:
		return key{kind: keyLeft}, 1, true
	case 0x03:
		return key{kind: keyInterrupt}, 1, true
	case 0x04:
		return key{kind: keyEOF}, 1, true
	case 0x05:
		return key{kind: keyEnd}, 1, true
	case 0x06:
		return key{kind: keyRight}, 1, true
	case 0x08, 0x7f:
		return key{kind: keyBackspace}, 1, true
	case '\r', '\n':
		return key{kind: keyEnter}, 1, true
	case 0x0b:
		return key{kind: keyKillToEnd}, 1, true
	case 0x0c:
		return key{kind: keyClear}, 1, true
	case 0x0e:
		return key{kind: keyDown}, 1, true
	case 0x10:
		return key{kind: keyUp}, 1, true
	case 0x15:
		return key{kind: keyKillToHome}, 1, true
	case 0x17:
		return key{kind: keyKillWord}, 1, true
	case 0x1b:
		return decodeEscape(b)
	}
	if b[0] < 0x20 {
		return key{kind: keyNone}, 1, true
	}
	r, n := utf8.DecodeRune(b)
	if r == utf8.RuneError && n <= 1 {
		if !utf8.FullRune(b) {
			return key{}, 0, false // a rune split across two reads
		}
		return key{kind: keyNone}, 1, true
	}
	return key{kind: keyRune, r: r}, n, true
}

// decodeEscape handles the sequences that start with ESC: the arrows and the
// home/end/delete keys in both encodings terminals send, and Alt-B / Alt-F.
func decodeEscape(b []byte) (key, int, bool) {
	if len(b) < 2 {
		return key{}, 0, false
	}
	switch b[1] {
	case 'b', 'B':
		return key{kind: keyWordLeft}, 2, true
	case 'f', 'F':
		return key{kind: keyWordRight}, 2, true
	case 0x7f:
		return key{kind: keyKillWord}, 2, true
	case 'O':
		if len(b) < 3 {
			return key{}, 0, false
		}
		switch b[2] {
		case 'H':
			return key{kind: keyHome}, 3, true
		case 'F':
			return key{kind: keyEnd}, 3, true
		}
		return key{kind: keyNone}, 3, true
	case '[':
		// CSI: parameters, then one final byte in @-~.
		for i := 2; i < len(b); i++ {
			if b[i] >= '@' && b[i] <= '~' {
				return csiKey(string(b[2:i]), b[i]), i + 1, true
			}
		}
		return key{}, 0, false
	}
	return key{kind: keyNone}, 2, true
}

// csiKey maps one CSI sequence. A modifier parameter (";5" for Ctrl, ";3" for
// Alt) turns the horizontal arrows into word motion, which is what those keys
// do in every other prompt.
func csiKey(params string, final byte) key {
	word := strings.Contains(params, ";3") || strings.Contains(params, ";5")
	switch final {
	case 'A':
		return key{kind: keyUp}
	case 'B':
		return key{kind: keyDown}
	case 'C':
		if word {
			return key{kind: keyWordRight}
		}
		return key{kind: keyRight}
	case 'D':
		if word {
			return key{kind: keyWordLeft}
		}
		return key{kind: keyLeft}
	case 'H':
		return key{kind: keyHome}
	case 'F':
		return key{kind: keyEnd}
	case '~':
		switch strings.SplitN(params, ";", 2)[0] {
		case "1", "7":
			return key{kind: keyHome}
		case "3":
			return key{kind: keyDelete}
		case "4", "8":
			return key{kind: keyEnd}
		}
	}
	return key{kind: keyNone}
}

// --- the editor --------------------------------------------------------------

// editAction is what one keystroke asks the caller to do; everything else is
// handled inside the editor and only redraws the line.
type editAction int

const (
	editContinue  editAction = iota // the line changed or the cursor moved
	editSubmit                      // Enter: the line is ready
	editCancel                      // Ctrl-C on a line with text: it is gone
	editInterrupt                   // Ctrl-C on an empty line
	editEOF                         // Ctrl-D on an empty line
	editClear                       // Ctrl-L: clear the screen, then redraw
)

// lineEditor is one prompt line and the session's history. Positions are in
// runes throughout, so a cursor moves by character and not by byte.
type lineEditor struct {
	buf     []rune
	pos     int
	history []string
	hist    int    // index in history while browsing; len(history) when not
	draft   string // the line being written, kept while browsing history

	// prevRows is how many terminal rows the last render used, so the next
	// one can find its way back to the top of that block before redrawing.
	// It is meaningless (and unused) once the line stops wrapping.
	prevRows int
}

func newLineEditor() *lineEditor { return &lineEditor{} }

// String is the line as it stands.
func (e *lineEditor) String() string { return string(e.buf) }

// reset empties the line without touching the history.
func (e *lineEditor) reset() {
	e.buf, e.pos, e.hist, e.draft, e.prevRows = nil, 0, len(e.history), "", 0
}

// remember adds a submitted line to the session history. Blank lines and a
// repeat of the last line are not worth a slot.
func (e *lineEditor) remember(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	if n := len(e.history); n > 0 && e.history[n-1] == line {
		e.hist = n
		return
	}
	e.history = append(e.history, line)
	e.hist = len(e.history)
}

// apply folds one keystroke into the line.
func (e *lineEditor) apply(k key) editAction {
	switch k.kind {
	case keyRune:
		e.insert(k.r)
	case keyEnter:
		return editSubmit
	case keyInterrupt:
		if len(e.buf) == 0 {
			return editInterrupt
		}
		e.reset()
		return editCancel
	case keyEOF:
		if len(e.buf) == 0 {
			return editEOF
		}
		e.deleteAt(e.pos) // Ctrl-D over text is a forward delete
	case keyClear:
		return editClear
	case keyBackspace:
		if e.pos > 0 {
			e.pos--
			e.deleteAt(e.pos)
		}
	case keyDelete:
		e.deleteAt(e.pos)
	case keyLeft:
		if e.pos > 0 {
			e.pos--
		}
	case keyRight:
		if e.pos < len(e.buf) {
			e.pos++
		}
	case keyHome:
		e.pos = 0
	case keyEnd:
		e.pos = len(e.buf)
	case keyWordLeft:
		e.pos = e.wordStart()
	case keyWordRight:
		e.pos = e.wordEnd()
	case keyKillToEnd:
		e.buf = e.buf[:e.pos]
	case keyKillToHome:
		e.buf = append([]rune{}, e.buf[e.pos:]...)
		e.pos = 0
	case keyKillWord:
		start := e.wordStart()
		e.buf = append(append([]rune{}, e.buf[:start]...), e.buf[e.pos:]...)
		e.pos = start
	case keyUp:
		e.recall(-1)
	case keyDown:
		e.recall(+1)
	}
	return editContinue
}

func (e *lineEditor) insert(r rune) {
	rest := append([]rune{r}, e.buf[e.pos:]...)
	e.buf = append(append([]rune{}, e.buf[:e.pos]...), rest...)
	e.pos++
}

// deleteAt removes the rune at i, if there is one.
func (e *lineEditor) deleteAt(i int) {
	if i < 0 || i >= len(e.buf) {
		return
	}
	e.buf = append(append([]rune{}, e.buf[:i]...), e.buf[i+1:]...)
}

// wordStart is the beginning of the word before the cursor: the spaces are
// skipped first, so a cursor sitting after a word kills that word.
func (e *lineEditor) wordStart() int {
	i := e.pos
	for i > 0 && unicode.IsSpace(e.buf[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(e.buf[i-1]) {
		i--
	}
	return i
}

// wordEnd is the end of the word after the cursor.
func (e *lineEditor) wordEnd() int {
	i := e.pos
	for i < len(e.buf) && unicode.IsSpace(e.buf[i]) {
		i++
	}
	for i < len(e.buf) && !unicode.IsSpace(e.buf[i]) {
		i++
	}
	return i
}

// recall walks the history. Stepping off the end brings back the line that
// was being written when the walk started.
func (e *lineEditor) recall(d int) {
	if len(e.history) == 0 {
		return
	}
	next := e.hist + d
	if next < 0 || next > len(e.history) {
		return
	}
	if e.hist == len(e.history) {
		e.draft = string(e.buf)
	}
	e.hist = next
	if next == len(e.history) {
		e.buf = []rune(e.draft)
	} else {
		e.buf = []rune(e.history[next])
	}
	e.pos = len(e.buf)
}

// render is the line as it should appear: back to the top of whatever this
// line drew last time, erase what was there, print prompt and line, then put
// the cursor back where it is.
//
// width is the terminal's column count. Once prompt+line reaches it, the
// terminal itself wraps the text onto further rows — rows a plain "\r\x1b[K"
// (current row only) never reaches, so the previous draw of a wrapped line
// stayed on screen above whatever was printed next, appearing to repeat with
// every keystroke. width<=0 (unknown) keeps the old single-row behaviour.
func (e *lineEditor) render(prompt string, width int) string {
	line := prompt + string(e.buf)
	if width <= 0 {
		s := "\r\x1b[K" + line
		if back := len(e.buf) - e.pos; back > 0 {
			s += "\x1b[" + strconv.Itoa(back) + "D"
		}
		e.prevRows = 1
		return s
	}

	var s string
	if e.prevRows > 1 {
		s += "\x1b[" + strconv.Itoa(e.prevRows-1) + "A"
	}
	s += "\r\x1b[J" + line

	// The prompt may be colour-coded (theme.style wraps it in SGR escapes):
	// those bytes print but take up no column, so they must not count
	// toward where the line wraps or where the cursor lands.
	promptWidth := visibleWidth(prompt)
	total := promptWidth + len(e.buf)
	cursorIdx := promptWidth + e.pos
	endRow, _ := wrapPos(total, width)
	targetRow, targetCol := wrapPos(cursorIdx, width)
	if up := endRow - targetRow; up > 0 {
		s += "\x1b[" + strconv.Itoa(up) + "A"
	}
	s += "\r"
	if targetCol > 0 {
		s += "\x1b[" + strconv.Itoa(targetCol) + "C"
	}

	e.prevRows = endRow + 1
	return s
}

// wrapPos is the row and column a cursor sits at, relative to the top-left
// of the block, after idx columns of text have been laid out at the given
// width — i.e. what a terminal's own line-wrapping would produce. col can
// reach width itself (rather than stopping at width-1): that is the same
// "just past the last column" position a real terminal leaves the cursor in
// after filling a row exactly, and cursor-forward movement clamps there the
// same way, so the two agree.
func wrapPos(idx, width int) (row, col int) {
	if idx == 0 {
		return 0, 0
	}
	row = (idx - 1) / width
	col = idx - row*width
	return row, col
}

// visibleWidth is how many columns s takes up on screen: an SGR escape
// (theme.style wraps the prompt in "\x1b[...m") prints but occupies none, so
// it is skipped rather than counted as a rune.
func visibleWidth(s string) int {
	n := 0
	b := []byte(s)
	for i := 0; i < len(b); {
		if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '[' {
			j := i + 2
			for j < len(b) && (b[j] < '@' || b[j] > '~') {
				j++
			}
			if j < len(b) {
				j++ // consume the final byte too
			}
			i = j
			continue
		}
		_, size := utf8.DecodeRune(b[i:])
		n++
		i += size
	}
	return n
}

// --- raw mode ----------------------------------------------------------------

// lineReader is the prompt: one line per call, however it is read.
type lineReader interface {
	readLine(prompt string) (string, error)
	close()
}

// newLineReader picks the editor for a real terminal and the plain reader for
// everything else — a pipe, a test, a `-m` run. stdin is the same buffered
// reader the approver uses, so the two never split a line between them.
func newLineReader(c *cli, t *theme, stdin *bufio.Reader) lineReader {
	f, ok := c.stdin.(*os.File)
	if ok && termutil.IsTerminal(int(f.Fd())) && isTerminal(c.stderr) && stdin.Buffered() == 0 {
		return &ttyLines{in: f, fd: int(f.Fd()), out: c.stderr, t: t, ed: newLineEditor()}
	}
	return &plainLines{in: stdin, out: c.stderr}
}

// ttyLines drives lineEditor over a terminal in raw mode.
type ttyLines struct {
	in      *os.File
	fd      int
	out     io.Writer
	t       *theme
	ed      *lineEditor
	pending []byte // bytes read but not yet consumed by a keystroke
	armed   bool   // a Ctrl-C on an empty line is waiting for its second
}

func (l *ttyLines) close() {}

// readLine puts the terminal in raw mode for exactly as long as it is reading:
// while a turn runs the terminal is its normal self, so Ctrl-C is a signal
// again and the turn's own handler aborts it.
func (l *ttyLines) readLine(prompt string) (string, error) {
	restore, err := rawMode(l.fd)
	if err != nil {
		// No raw mode: fall back to the line discipline rather than fail.
		_, _ = io.WriteString(l.out, prompt)
		return readPlainLine(bufio.NewReader(l.in))
	}
	defer restore()

	l.ed.reset()
	for {
		_, _ = io.WriteString(l.out, l.ed.render(prompt, terminalWidth(l.fd)))
		k, err := l.nextKey()
		if err != nil {
			_, _ = io.WriteString(l.out, "\r\n")
			if line := l.ed.String(); line != "" {
				return line, err
			}
			return "", err
		}
		if k.kind != keyInterrupt {
			l.armed = false
		}
		switch l.ed.apply(k) {
		case editSubmit:
			line := l.ed.String()
			l.ed.remember(line)
			_, _ = io.WriteString(l.out, "\r\n")
			return line, nil
		case editCancel:
			_, _ = io.WriteString(l.out, "\r\n")
		case editInterrupt:
			if l.armed {
				_, _ = io.WriteString(l.out, "\r\n")
				return "", errPromptQuit
			}
			l.armed = true
			_, _ = io.WriteString(l.out, "\r\n"+l.t.style(styleDim, "[Ctrl-C again or /quit to leave]")+"\r\n")
		case editEOF:
			_, _ = io.WriteString(l.out, "\r\n")
			return "", errPromptQuit
		case editClear:
			_, _ = io.WriteString(l.out, "\x1b[2J\x1b[H")
		case editContinue:
		}
	}
}

// nextKey decodes one keystroke, reading more bytes when the sequence it has
// is not complete yet.
func (l *ttyLines) nextKey() (key, error) {
	for {
		if k, n, ok := decodeKey(l.pending); ok {
			l.pending = l.pending[n:]
			return k, nil
		}
		var b [64]byte
		n, err := l.in.Read(b[:])
		if n > 0 {
			l.pending = append(l.pending, b[:n]...)
			continue
		}
		if err == nil {
			err = io.EOF
		}
		return key{}, err
	}
}

// rawMode turns off echo and the line discipline, and returns the undo. The
// undo is idempotent and runs from a defer, so a panic unwinding through
// readLine leaves the terminal usable; a signal that would kill the process
// outright is caught here for the same reason.
func rawMode(fd int) (func(), error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IXON | unix.ICRNL | unix.BRKINT | unix.INPCK | unix.ISTRIP
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}

	done := make(chan struct{})
	var once sync.Once
	undo := func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		select {
		case s := <-sigc:
			undo()
			signal.Stop(sigc)
			// Die the way the signal meant to kill us.
			if sig, ok := s.(syscall.Signal); ok {
				signal.Reset(sig)
				_ = syscall.Kill(os.Getpid(), sig)
			}
		case <-done:
		}
	}()
	return func() {
		once.Do(func() {
			signal.Stop(sigc)
			close(done)
			undo()
		})
	}, nil
}

// --- the plain reader ---------------------------------------------------------

// plainLines is the prompt when stdin is not a terminal: the prompt is
// printed and a line is read, exactly as `chat` has always done.
type plainLines struct {
	in  *bufio.Reader
	out io.Writer
}

func (l *plainLines) close() {}

func (l *plainLines) readLine(prompt string) (string, error) {
	_, _ = io.WriteString(l.out, prompt)
	return readPlainLine(l.in)
}

// readPlainLine returns the line without its newline, and the read error as
// it came: a last line with no newline is still a line.
func readPlainLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	return strings.TrimRight(s, "\r\n"), err
}
