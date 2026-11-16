package whatsapp

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"holaclaw/internal/channel"
)

// These cover what review found: hostile input, races with whatsmeow and
// links that end in unusual ways.

// TestOversizedMediaIsNeverMaterialised: a body far past the cap, with no
// declared length, stops streaming at the cap and leaves no temporary file.
func TestOversizedMediaIsNeverMaterialised(t *testing.T) {
	e, f := connected(t)
	e.a.putBlob = (&blobSink{}).put
	e.a.maxDownload = 1 << 20
	f.mu.Lock()
	f.mediaSize = 1 << 30
	f.mu.Unlock()
	env := one(t, e, f, dm("BIG", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: ptr("dump.bin")}}))
	if env.Message.Text != "[file too large: dump.bin]" || len(env.Message.Attachments) != 0 {
		t.Fatalf("oversized body = %q", env.Message.Text)
	}
	f.mu.Lock()
	written := f.written
	f.mu.Unlock()
	if written > e.a.maxDownload+64 {
		t.Fatalf("%d bytes accepted past a %d cap", written, e.a.maxDownload)
	}
	entries, err := os.ReadDir(filepath.Dir(e.path))
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range entries {
		if de.Name() != filepath.Base(e.path) {
			t.Fatalf("left behind %s", de.Name())
		}
	}
}

func TestPerConversationMemoryIsBounded(t *testing.T) {
	a := New(Options{})
	for i := range maxConversations + 10 {
		conv := strconv.Itoa(i) + "@s.whatsapp.net"
		a.addUnread(conv, readRef{id: "x"})
		a.setExpiration(t.Context(), conv, 60)
	}
	if len(a.unread) > maxConversations || len(a.expiry) > maxConversations {
		t.Fatalf("maps hold %d / %d conversations, cap %d", len(a.unread), len(a.expiry), maxConversations)
	}
	if _, ok := a.unread["0@s.whatsapp.net"]; ok {
		t.Fatal("the oldest conversation was kept")
	}
	if _, ok := a.unread[strconv.Itoa(maxConversations+9)+"@s.whatsapp.net"]; !ok {
		t.Fatal("the newest conversation was evicted")
	}
}

func TestSanitisers(t *testing.T) {
	bot := types.NewJID(number, types.DefaultUserServer)
	if got := stripSelfMention("@"+number+"99 x @"+number+" y", bot, types.JID{}); got != "@"+number+"99 x y" {
		t.Fatalf("strip after a longer number = %q", got)
	}
	if got := sanitiseName("inv\u202egpj.exe"); got != "invgpj.exe" {
		t.Fatalf("bidi override kept: %q", got)
	}
	long := strings.Repeat("ñ", 100) // 200 bytes
	if got := sanitiseName(long); !utf8.ValidString(got) || len(got) > 120 {
		t.Fatalf("long name cut to %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	loc := stubOf(&waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: ptr(1.0), DegreesLongitude: ptr(2.0), Name: ptr("a\nb")}})
	if loc != "[location: 1.00000, 2.00000 a b]" {
		t.Fatalf("location = %q", loc)
	}
}

// TestPairSuccessWithoutConnectedRetries: if whatsmeow's own reconnect after
// pairing fails, no event follows; the retry armed on PairSuccess connects.
func TestPairSuccessWithoutConnectedRetries(t *testing.T) {
	e, f, _ := linking(t)
	f.pair(t, number)
	f.emit(&events.PairSuccess{ID: types.NewJID(number, types.DefaultUserServer)})
	before := f.count(&f.connects)
	e.clock.fire(t, 2*time.Second)
	eventually(t, "the adapter's own connect", func() bool { return f.count(&f.connects) == before+1 })
	f.emit(&events.Connected{})
	e.waitStatus(statusConnected)
}

func TestLinkFailsOnLogoutOrOutdatedClient(t *testing.T) {
	e, f, _ := linking(t)
	f.emit(&events.ClientOutdated{})
	e.waitStatus("logged_out|" + detailOutdated)
	if got := e.a.LinkState(); got.Status != channel.LinkFailed || got.Detail != detailOutdated {
		t.Fatalf("LinkState = %+v", got)
	}

	e2, f2, _ := linking(t)
	f2.emit(&events.LoggedOut{})
	e2.waitStatus("logged_out|" + detailLoggedOut)
	if got := e2.a.LinkState(); got.Status != channel.LinkFailed || got.Detail != detailLoggedOut {
		t.Fatalf("LinkState = %+v", got)
	}
}

// TestUnlinkStopsALinkWaitingForTheSocket: Link waits for the QR without
// holding the session lock, and ending the link lets it go at once.
func TestUnlinkStopsALinkWaitingForTheSocket(t *testing.T) {
	e := newEnv(t, "", nil) // the socket never sends a QR
	e.run()
	e.waitStatus(statusUnlinked)
	errc := make(chan error, 1)
	go func() {
		_, err := e.a.Link(t.Context(), "+"+number)
		errc <- err
	}()
	eventually(t, "the link to start", func() bool { return e.a.LinkState().Status == channel.LinkPending })
	done := make(chan error, 1)
	go func() { done <- e.a.Unlink(t.Context()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unlink: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unlink stalled behind the QR wait")
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("the cancelled link succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Link kept waiting after the link ended")
	}
}

func TestLogBridgeDropsTheClosedStoreDelete(t *testing.T) {
	var buf bytes.Buffer
	l := newLogBridge(slog.New(slog.NewTextHandler(&buf, nil)), "whatsmeow")
	l.Warnf("Failed to delete store after %d failure: %v", 401, errors.New("failed to delete device: sql: database is closed"))
	if buf.Len() != 0 {
		t.Fatalf("logged: %s", buf.String())
	}
	l.Warnf("Failed to delete store after %d failure: %v", 401, errors.New("disk I/O error"))
	if !strings.Contains(buf.String(), "disk I/O error") {
		t.Fatal("a real delete failure was dropped")
	}
}

// TestSessionPathIsEscaped: '?', '#' and '%' in an instance name name a file;
// they never start a query or a fragment of the SQLite URI.
func TestSessionPathIsEscaped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "whatsapp")
	path := filepath.Join(dir, "wa?mode=memory#x%41 b.db")
	c, err := openSession(t.Context(), path, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		var names []string
		for _, de := range entries {
			names = append(names, de.Name())
		}
		t.Fatalf("files = %q, want only %q", names, filepath.Base(path))
	}
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		t.Fatalf("the database is not in the named file: %v", err)
	}
}
