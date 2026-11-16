package whatsapp

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// fakeCode is distinctive so tests can assert it never reaches a log or a
// status detail.
const fakeCode = "QZXW-7KPV"

// fakeClient is a scriptable waClient. Its "store" is the session file: a
// linked device is the file holding "linked:<number>", which the fake opener
// reads, so a wipe that deletes the file really unlinks.
type fakeClient struct {
	path string

	mu         sync.Mutex
	pn         types.JID
	lid        types.JID
	handlers   []func(any) bool
	connects   int
	connectErr []error // popped one per Connect; nil when empty
	onConnect  func(f *fakeClient)
	pairCode   string
	pairErr    error
	pairArgs   []any
	logoutErr  error
	disconnect int
	logouts    int
	deletes    int
	closed     bool

	sends           []sent
	sendErr         []error // popped one per SendMessage
	uploads         []whatsmeow.MediaType
	uploadErr       error
	media           []byte // what DownloadToFile writes
	mediaSize       int64  // or a body this long, never held in memory
	written         int64  // bytes the capped file accepted
	downloadErr     error
	downloads       int
	presence        []string
	chatPresenceErr error
	reads           []string // "chat/sender/id"
	nextID          int

	rejects   []rejectedCall
	rejectErr error
}

// rejectedCall is one recorded RejectCall call.
type rejectedCall struct {
	from types.JID
	id   string
}

var _ waClient = (*fakeClient)(nil)

func (f *fakeClient) Connect(context.Context) error {
	f.mu.Lock()
	f.connects++
	var err error
	if len(f.connectErr) > 0 {
		err, f.connectErr = f.connectErr[0], f.connectErr[1:]
	}
	hook := f.onConnect
	f.mu.Unlock()
	if err == nil && hook != nil {
		hook(f)
	}
	return err
}

func (f *fakeClient) Disconnect() { f.mu.Lock(); f.disconnect++; f.mu.Unlock() }

func (f *fakeClient) OwnID() (types.JID, types.JID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pn, f.lid
}

func (f *fakeClient) PairPhone(_ context.Context, phone string, show bool, ct whatsmeow.PairClientType, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairArgs = []any{phone, show, ct, name}
	return f.pairCode, f.pairErr
}

func (f *fakeClient) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts++
	if f.logoutErr != nil {
		return f.logoutErr
	}
	f.pn = types.JID{}
	return os.WriteFile(f.path, nil, 0o600)
}

func (f *fakeClient) DeleteLocalDevice(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	if f.pn.IsEmpty() {
		return sqlstore.ErrDeviceIDMustBeSet // as whatsmeow, for a device never saved
	}
	return nil
}

func (f *fakeClient) AddEventHandler(h func(any) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, h)
}

// sent is one recorded SendMessage call.
type sent struct {
	to  types.JID
	msg *waE2E.Message
	id  types.MessageID
}

func (f *fakeClient) SendMessage(_ context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var id types.MessageID
	if len(extra) == 1 {
		id = extra[0].ID
	}
	f.sends = append(f.sends, sent{to: to, msg: msg, id: id})
	if len(f.sendErr) > 0 {
		err := f.sendErr[0]
		f.sendErr = f.sendErr[1:]
		if err != nil {
			return whatsmeow.SendResponse{}, err
		}
	}
	return whatsmeow.SendResponse{ID: id}, nil
}

func (f *fakeClient) Upload(_ context.Context, data []byte, mt whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, mt)
	if f.uploadErr != nil {
		return whatsmeow.UploadResponse{}, f.uploadErr
	}
	return whatsmeow.UploadResponse{URL: "https://mmg.example/x", DirectPath: "/x", FileLength: uint64(len(data))}, nil
}

// DownloadToFile writes f.media, or a body of f.mediaSize bytes, into file in
// 32 KiB chunks, the way a streamed HTTP body arrives, and stops at the
// first write error.
func (f *fakeClient) DownloadToFile(_ context.Context, _ whatsmeow.DownloadableMessage, file whatsmeow.File) error {
	f.mu.Lock()
	f.downloads++
	body, size, derr := f.media, f.mediaSize, f.downloadErr
	f.mu.Unlock()
	if derr != nil {
		return derr
	}
	if size == 0 {
		size = int64(len(body))
	}
	chunk := make([]byte, 32<<10)
	for off := int64(0); off < size; {
		n := min(int64(len(chunk)), size-off)
		part := chunk[:n]
		if body != nil {
			part = body[off : off+n]
		}
		w, err := file.Write(part)
		f.mu.Lock()
		f.written += int64(w)
		f.mu.Unlock()
		if err != nil {
			return err
		}
		off += n
	}
	return nil
}

func (f *fakeClient) SendPresence(_ context.Context, p types.Presence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presence = append(f.presence, string(p))
	return nil
}

func (f *fakeClient) SendChatPresence(_ context.Context, _ types.JID, p types.ChatPresence, _ types.ChatPresenceMedia) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presence = append(f.presence, string(p))
	return f.chatPresenceErr
}

func (f *fakeClient) MarkRead(_ context.Context, ids []types.MessageID, _ time.Time, chat, sender types.JID, _ ...types.ReceiptType) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.reads = append(f.reads, chat.String()+"/"+sender.String()+"/"+id)
	}
	return nil
}

// BuildReaction mirrors whatsmeow's key, simplified: FromMe for an empty
// sender only (whatsmeow also sets it for the account's own PN or LID), the
// participant only in a group.
func (f *fakeClient) BuildReaction(chat, sender types.JID, id types.MessageID, emoji string) *waE2E.Message {
	key := &waCommon.MessageKey{RemoteJID: ptr(chat.String()), ID: ptr(id), FromMe: ptr(sender.IsEmpty())}
	if !sender.IsEmpty() && chat.Server == types.GroupServer {
		key.Participant = ptr(sender.ToNonAD().String())
	}
	return &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: key, Text: ptr(emoji)}}
}

func (f *fakeClient) RejectCall(_ context.Context, callFrom types.JID, callID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejects = append(f.rejects, rejectedCall{from: callFrom, id: callID})
	return f.rejectErr
}

// rejectedCalls returns the recorded RejectCall calls.
func (f *fakeClient) rejectedCalls() []rejectedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rejectedCall(nil), f.rejects...)
}

func (f *fakeClient) GenerateMessageID() types.MessageID {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	return types.MessageID("3EB0" + strconv.Itoa(f.nextID))
}

// sentMsgs returns the recorded sends.
func (f *fakeClient) sentMsgs() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sends...)
}

func (f *fakeClient) uploadsDone() []whatsmeow.MediaType {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]whatsmeow.MediaType(nil), f.uploads...)
}

func (f *fakeClient) presences() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.presence...)
}

func (f *fakeClient) readsDone() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reads...)
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeClient) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// emit delivers evt to the registered handlers, as whatsmeow does.
func (f *fakeClient) emit(evt any) bool {
	f.mu.Lock()
	hs := append([]func(any) bool(nil), f.handlers...)
	f.mu.Unlock()
	ok := true
	for _, h := range hs {
		ok = h(evt) && ok
	}
	return ok
}

// pair stores the device as whatsmeow does on PairSuccess.
func (f *fakeClient) pair(t *testing.T, number string) {
	t.Helper()
	f.mu.Lock()
	f.pn = types.NewJID(number, types.DefaultUserServer)
	f.mu.Unlock()
	if err := os.WriteFile(f.path, []byte("linked:"+number), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeClient) count(field *int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *field
}

// fakeClock hands out timers that fire only when a test says so.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	d     time.Duration
	ch    chan time.Time
	fired bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	tm := &fakeTimer{d: d, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, tm)
	return tm.ch
}

// armed reports how many unfired timers of duration d exist.
func (c *fakeClock) armed(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, tm := range c.timers {
		if tm.d == d && !tm.fired {
			n++
		}
	}
	return n
}

// fire waits for an unfired timer of duration d and fires it.
func (c *fakeClock) fire(t *testing.T, d time.Duration) {
	t.Helper()
	eventually(t, "a timer of "+d.String(), func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, tm := range c.timers {
			if tm.d == d && !tm.fired {
				tm.fired = true
				tm.ch <- c.now.Add(d)
				return true
			}
		}
		return false
	})
}

// statusRec records every status report.
type statusRec struct {
	mu  sync.Mutex
	all []string // "state|detail"
}

func (s *statusRec) report(st channel.State, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all = append(s.all, string(st)+"|"+detail)
}

func (s *statusRec) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.all) == 0 {
		return ""
	}
	return s.all[len(s.all)-1]
}

func (s *statusRec) joined() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.all, "\n")
}

// env is an adapter running against fake clients and a fake clock.
type env struct {
	t      *testing.T
	a      *Adapter
	clock  *fakeClock
	status *statusRec
	logs   *syncBuffer
	path   string

	mu      sync.Mutex
	clients []*fakeClient
	// setup scripts each client as the fake opener makes it.
	setup func(f *fakeClient)

	cancel context.CancelFunc
	done   chan error

	inMu    sync.Mutex
	envs    []event.Envelope
	inErr   []error // popped one per Inbound
	claimed map[string]bool
	dups    int
}

// inbound is the Inbound Run gets: it records the envelope unless a scripted
// error says the store failed. Like the gate's ClaimInbound, it drops an
// envelope sharing a dedupe key with an earlier one (PlatformMsgID when it
// has none) and records the keys it adds.
func (e *env) inbound(_ context.Context, env event.Envelope) error {
	e.inMu.Lock()
	defer e.inMu.Unlock()
	if len(e.inErr) > 0 {
		err := e.inErr[0]
		e.inErr = e.inErr[1:]
		if err != nil {
			return err
		}
	}
	keys := env.DedupeKeys
	if len(keys) == 0 {
		keys = []string{env.Origin.PlatformMsgID}
	}
	if e.claimed == nil {
		e.claimed = map[string]bool{}
	}
	dup := false
	for _, k := range keys {
		dup = dup || e.claimed[k]
		e.claimed[k] = true
	}
	if dup {
		e.dups++
		return channel.ErrDuplicate
	}
	e.envs = append(e.envs, env)
	return nil
}

// duplicates is how many envelopes the fake core dropped as duplicates.
func (e *env) duplicates() int {
	e.inMu.Lock()
	defer e.inMu.Unlock()
	return e.dups
}

func (e *env) stored() []event.Envelope {
	e.inMu.Lock()
	defer e.inMu.Unlock()
	return append([]event.Envelope(nil), e.envs...)
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newEnv builds an adapter whose session file is linked to number, or empty
// when number is "".
func newEnv(t *testing.T, number string, setup func(f *fakeClient)) *env {
	t.Helper()
	e := &env{
		t: t, clock: &fakeClock{now: time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)},
		status: &statusRec{}, logs: &syncBuffer{}, setup: setup,
		path: filepath.Join(t.TempDir(), "whatsapp", "wa.db"),
	}
	if number != "" {
		if err := prepareFile(e.path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.path, []byte("linked:"+number), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.a = New(Options{Name: "wa", SessionPath: e.path,
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	e.a.open = e.open
	e.a.now = e.clock.Now
	e.a.after = e.clock.After
	e.a.jitter = func(n int64) int64 { return n - 1 } // the top of the range
	return e
}

func (e *env) open(_ context.Context, path string, _ *slog.Logger) (waClient, error) {
	if err := prepareFile(path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := &fakeClient{path: path}
	if n, ok := strings.CutPrefix(string(raw), "linked:"); ok {
		f.pn = types.NewJID(n, types.DefaultUserServer)
	}
	e.mu.Lock()
	setup := e.setup
	e.clients = append(e.clients, f)
	e.mu.Unlock()
	if setup != nil {
		setup(f)
	}
	return f, nil
}

// client is the n-th client the adapter opened.
func (e *env) client(n int) *fakeClient {
	e.t.Helper()
	var f *fakeClient
	eventually(e.t, "client to open", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		if len(e.clients) > n {
			f = e.clients[n]
			return true
		}
		return false
	})
	return f
}

func (e *env) opened() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.clients)
}

// run starts Run and stops it at cleanup.
func (e *env) run() {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan error, 1)
	go func() { e.done <- e.a.Run(ctx, e.inbound, e.status.report) }()
	e.t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
			e.t.Error("Run did not return")
		}
	})
}

// waitStatus waits until the last status is want ("state|detail").
func (e *env) waitStatus(want string) {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for e.status.last() != want {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for status %q; have:\n%s", want, e.status.joined())
		}
		time.Sleep(time.Millisecond)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
