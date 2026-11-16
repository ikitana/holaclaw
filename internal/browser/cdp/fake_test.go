package cdp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
)

// fakeBrowser speaks the NUL-framed pipe protocol over two io.Pipes, so the
// unit tests exercise the real framing, ids and routing without a browser.
type fakeBrowser struct {
	t *testing.T

	conn *Conn

	cmdR *io.PipeReader // commands the Conn wrote
	cmdW *io.PipeWriter
	evR  *io.PipeReader // replies and events the fake writes
	evW  *io.PipeWriter

	wmu sync.Mutex

	mu   sync.Mutex
	seen []fakeCommand
}

// fakeCommand is one command as the fake received it.
type fakeCommand struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
}

// newFake wires a Conn to a fake browser. handle runs on the fake's own
// goroutine for every command; it may call f.send.
func newFake(t *testing.T, opts Options, handle func(f *fakeBrowser, cmd fakeCommand)) *fakeBrowser {
	t.Helper()
	f := &fakeBrowser{t: t}
	f.cmdR, f.cmdW = io.Pipe()
	f.evR, f.evW = io.Pipe()
	f.conn = New(f.evR, f.cmdW, opts)

	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReaderSize(f.cmdR, 64<<10)
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
			var cmd fakeCommand
			if err := json.Unmarshal(buf.Bytes(), &cmd); err != nil {
				return
			}
			buf.Reset()
			f.mu.Lock()
			f.seen = append(f.seen, cmd)
			f.mu.Unlock()
			if handle != nil {
				handle(f, cmd)
			}
		}
	}()

	t.Cleanup(func() {
		_ = f.conn.Close()
		_ = f.evW.Close()
		_ = f.cmdR.Close()
		<-done
	})
	return f
}

// send writes one JSON value as a framed message.
func (f *fakeBrowser) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Errorf("fake: marshal: %v", err)
		return
	}
	f.sendRaw(b)
}

// sendRaw writes bytes verbatim, then the NUL terminator.
func (f *fakeBrowser) sendRaw(b []byte) {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if _, err := f.evW.Write(append(b, 0)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		f.t.Errorf("fake: write: %v", err)
	}
}

// reply answers a command with a result.
func (f *fakeBrowser) reply(id int64, result any) {
	f.send(map[string]any{"id": id, "result": result})
}

// replyError answers a command with a protocol error.
func (f *fakeBrowser) replyError(id int64, code int64, msg string) {
	f.send(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}})
}

// event sends an event, optionally for a session.
func (f *fakeBrowser) event(sessionID, method string, params any) {
	m := map[string]any{"method": method}
	if params != nil {
		m["params"] = params
	}
	if sessionID != "" {
		m["sessionId"] = sessionID
	}
	f.send(m)
}

// commands returns the methods seen so far, in order.
func (f *fakeBrowser) commands() []fakeCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCommand(nil), f.seen...)
}
