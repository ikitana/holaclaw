package page

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"holaclaw/internal/browser/cdp"
)

// fakeCDP is a browser at the message level: it speaks the NUL-framed pipe
// protocol, records every command and answers from a table of canned results.
// The unit tests of this package are held against it; the rules that only a
// real browser can prove are in integration_test.go.
type fakeCDP struct {
	t    *testing.T
	conn *cdp.Conn

	evW *io.PipeWriter

	mu      sync.Mutex
	seen    []fakeCmd
	results map[string]any
}

// fakeCmd is one command as the fake received it.
type fakeCmd struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
}

func newFakeCDP(t *testing.T) *fakeCDP {
	t.Helper()
	cmdR, cmdW := io.Pipe()
	evR, evW := io.Pipe()
	f := &fakeCDP{t: t, evW: evW, results: map[string]any{}}
	f.conn = cdp.New(evR, cmdW, cdp.Options{Logger: slog.New(slog.DiscardHandler)})

	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReaderSize(cmdR, 64<<10)
		var buf bytes.Buffer
		for {
			chunk, err := br.ReadSlice(0)
			if errors.Is(err, bufio.ErrBufferFull) {
				buf.Write(chunk)
				continue
			}
			if err != nil {
				return
			}
			buf.Write(chunk[:len(chunk)-1])
			var cmd fakeCmd
			if err := json.Unmarshal(buf.Bytes(), &cmd); err != nil {
				return
			}
			buf.Reset()
			f.mu.Lock()
			f.seen = append(f.seen, cmd)
			result, ok := f.results[cmd.Method]
			f.mu.Unlock()
			if !ok {
				result = map[string]any{}
			}
			f.reply(cmd.ID, result)
		}
	}()
	t.Cleanup(func() {
		_ = f.conn.Close()
		_ = evW.Close()
		_ = cmdR.Close()
		<-done
	})
	return f
}

// result registers the answer for one method.
func (f *fakeCDP) result(method string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[method] = value
}

func (f *fakeCDP) reply(id int64, result any) {
	b, err := json.Marshal(map[string]any{"id": id, "result": result})
	if err != nil {
		f.t.Errorf("fake: marshal: %v", err)
		return
	}
	if _, err := f.evW.Write(append(b, 0)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		f.t.Errorf("fake: write: %v", err)
	}
}

// commands returns every command seen so far.
func (f *fakeCDP) commands() []fakeCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCmd(nil), f.seen...)
}

// sent reports whether a method was called, and returns its parameters.
func (f *fakeCDP) sent(method string) (json.RawMessage, bool) {
	for _, c := range f.commands() {
		if c.Method == method {
			return c.Params, true
		}
	}
	return nil, false
}

// methods lists the methods seen, in order.
func (f *fakeCDP) methods() []string {
	cmds := f.commands()
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Method)
	}
	return out
}

// testEngineNoBrowser is an Engine with no Manager use: enough for the parts
// that never start a browser.
func testEngineNoBrowser(t *testing.T) *Engine {
	t.Helper()
	zero := 0.0
	e, err := New(Options{StateDir: t.TempDir(), Pacing: &zero, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Shutdown(t.Context()) })
	return e
}
