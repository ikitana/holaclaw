package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// --- fakes ---------------------------------------------------------------

// fakeProv is a provider that also serves an STT endpoint. Chat returns text
// (or err); Transcribe returns the blob bytes as text unless scripted.
type fakeProv struct {
	mu        sync.Mutex
	chatCalls int
	sttCalls  int
	reqs      []provider.Request
	inflight  int
	peak      int

	text  string         // chat reply
	usage provider.Usage // what the chat reply reports
	err   error          // returned by both calls
	delay time.Duration  // held while "in flight"
}

func (p *fakeProv) Name() string { return "fake" }

func (p *fakeProv) enter() {
	p.mu.Lock()
	p.inflight++
	if p.inflight > p.peak {
		p.peak = p.inflight
	}
	p.mu.Unlock()
}

func (p *fakeProv) leave() {
	p.mu.Lock()
	p.inflight--
	p.mu.Unlock()
}

func (p *fakeProv) work(ctx context.Context) error {
	p.enter()
	defer p.leave()
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.err
}

func (p *fakeProv) Chat(ctx context.Context, req provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	p.chatCalls++
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	if err := p.work(ctx); err != nil {
		return nil, err
	}
	return &fakeStream{text: p.text, usage: p.usage}, nil
}

func (p *fakeProv) Transcribe(ctx context.Context, m provider.Model, mimeType string, audio []byte) (string, provider.Usage, error) {
	p.mu.Lock()
	p.sttCalls++
	p.mu.Unlock()
	if err := p.work(ctx); err != nil {
		return "", provider.Usage{}, err
	}
	if p.text != "" {
		return p.text, p.usage, nil
	}
	return string(audio), p.usage, nil
}

func (p *fakeProv) counts() (chat, stt int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chatCalls, p.sttCalls
}

// chatOnly is a provider that does not implement provider.Transcriber.
type chatOnly struct{ calls int }

func (p *chatOnly) Name() string { return "chat-only" }

func (p *chatOnly) Chat(context.Context, provider.Request) (provider.Stream, error) {
	p.calls++
	return &fakeStream{}, nil
}

type fakeStream struct {
	text  string
	usage provider.Usage
	done  bool
}

func (s *fakeStream) Next() (provider.Chunk, error) {
	if s.done {
		return provider.Chunk{}, io.EOF
	}
	s.done = true
	return provider.Chunk{Kind: provider.ChunkDone, Response: &provider.Response{
		Text: s.text, Finish: provider.FinishStop, Usage: s.usage,
	}}, nil
}

func (s *fakeStream) Close() error { return nil }

// sink collects events.
type sink struct {
	mu sync.Mutex
	ev []event.Event
}

func (s *sink) Emit(_ context.Context, e event.Event) {
	s.mu.Lock()
	s.ev = append(s.ev, e)
	s.mu.Unlock()
}

func (s *sink) types() []event.Type {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]event.Type, len(s.ev))
	for i, e := range s.ev {
		out[i] = e.Type
	}
	return out
}

func (s *sink) attrs(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]any{}
	if err := json.Unmarshal(s.ev[i].Attrs, &m); err != nil {
		panic(err)
	}
	return m
}

// harness wires a Normaliser over an in-memory blob map.
type harness struct {
	blobs   map[string][]byte
	events  *sink
	derived []event.Derived
	setErr  error
	mu      sync.Mutex
	deps    Deps
}

func newHarness() *harness {
	h := &harness{blobs: map[string][]byte{}, events: &sink{}}
	h.deps = Deps{
		Blobs: func(_ context.Context, id string) (io.ReadCloser, int64, error) {
			h.mu.Lock()
			b, ok := h.blobs[id]
			h.mu.Unlock()
			if !ok {
				return nil, 0, errors.New("no such blob")
			}
			return io.NopCloser(strings.NewReader(string(b))), int64(len(b)), nil
		},
		SetDerived: func(_ context.Context, _, _ event.ID, d event.Derived) error {
			h.mu.Lock()
			h.derived = append(h.derived, d)
			err := h.setErr
			h.mu.Unlock()
			return err
		},
		Events: h.events,
		Now:    func() time.Time { return time.Unix(0, 0).UTC() },
	}
	return h
}

func (h *harness) put(id, content string) { h.blobs[id] = []byte(content) }

func (h *harness) new() *Normaliser { return New(h.deps) }

func (h *harness) persisted() []event.Derived {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]event.Derived(nil), h.derived...)
}

func att(id, blob, mimeType, name string, size int64) event.Attachment {
	return event.Attachment{
		ID: event.ID(id), BlobID: blob, MIME: mimeType, Name: name,
		Size: size, Kind: event.KindFromMIME(mimeType),
	}
}

func turn(as ...event.Attachment) []event.Message {
	return []event.Message{{ID: "m1", Role: event.RoleUser, Text: "hi", Attachments: as}}
}

// --- audio ---------------------------------------------------------------

func TestTranscribeHappyPath(t *testing.T) {
	h := newHarness()
	h.put("b1", "spoken words")
	p := &fakeProv{}
	h.deps.Transcribe = &Role{Model: provider.Model{Name: "whisper"}, Provider: p}
	n := h.new()

	msgs := turn(att("a1", "b1", "audio/ogg; codecs=opus", "voice.ogg", 12))
	notices := n.Normalise(context.Background(), "t1", "s1", msgs)

	if len(notices) != 0 {
		t.Fatalf("notices = %v, want none", notices)
	}
	d := msgs[0].Attachments[0].Derived
	if d == nil || d.Text != "spoken words" || d.By != event.DerivedByTranscribe {
		t.Fatalf("derived = %+v", d)
	}
	if got := h.persisted(); len(got) != 1 || got[0].Text != "spoken words" {
		t.Fatalf("persisted = %+v", got)
	}
	if types := h.events.types(); len(types) != 1 || types[0] != event.MediaTranscribed {
		t.Fatalf("events = %v", types)
	}
	a := h.events.attrs(0)
	if a["attachment"] != "a1" || a["bytes"] != float64(12) || a["chars"] != float64(12) {
		t.Fatalf("attrs = %v", a)
	}
	if s := string(h.events.ev[0].Attrs); strings.Contains(s, "spoken words") {
		t.Fatalf("event carries content: %s", s)
	}
	if h.events.ev[0].MessageID != "m1" {
		t.Fatalf("message id = %q", h.events.ev[0].MessageID)
	}
}

func TestTranscribeRoleAbsent(t *testing.T) {
	h := newHarness()
	h.put("b1", "spoken words")
	n := h.new() // no Transcribe role

	msgs := turn(att("a1", "b1", "audio/mpeg", "note.mp3", 12))
	notices := n.Normalise(context.Background(), "t1", "s1", msgs)

	if len(notices) != 1 || notices[0] != NoticeNoTranscribe {
		t.Fatalf("notices = %v", notices)
	}
	if msgs[0].Attachments[0].Derived != nil {
		t.Fatal("derived set")
	}
	if types := h.events.types(); len(types) != 1 || types[0] != event.MediaRefused {
		t.Fatalf("events = %v", types)
	}
	if a := h.events.attrs(0); a["reason"] != reasonNoTranscribe {
		t.Fatalf("attrs = %v", a)
	}
}

func TestTranscribeProviderIsNotTranscriber(t *testing.T) {
	h := newHarness()
	h.put("b1", "spoken words")
	p := &chatOnly{}
	h.deps.Transcribe = &Role{Provider: p}
	n := h.new()

	msgs := turn(att("a1", "b1", "audio/mpeg", "note.mp3", 12))
	notices := n.Normalise(context.Background(), "t1", "s1", msgs)

	if len(notices) != 1 || notices[0] != NoticeNoTranscribe {
		t.Fatalf("notices = %v", notices)
	}
	if a := h.events.attrs(0); a["reason"] != reasonNoTranscribe {
		t.Fatalf("attrs = %v", a)
	}
}

func TestTranscribeErrorHidesProviderText(t *testing.T) {
	h := newHarness()
	h.put("b1", "spoken words")
	p := &fakeProv{err: &provider.Error{
		Kind: provider.ErrRateLimit, Provider: "fake", Message: "sk-secret quota blown",
	}}
	h.deps.Transcribe = &Role{Provider: p}
	n := h.new()

	msgs := turn(att("a1", "b1", "audio/mpeg", "note.mp3", 12))
	notices := n.Normalise(context.Background(), "t1", "s1", msgs)

	if len(notices) != 1 || notices[0] != NoticeTranscribeFail {
		t.Fatalf("notices = %v", notices)
	}
	if strings.Contains(notices[0], "quota") || strings.Contains(notices[0], "sk-secret") {
		t.Fatalf("notice leaks the error: %q", notices[0])
	}
	a := h.events.attrs(0)
	if a["reason"] != reasonError || a["error"] != string(provider.ErrRateLimit) {
		t.Fatalf("attrs = %v", a)
	}
	if s := string(h.events.ev[0].Attrs); strings.Contains(s, "sk-secret") {
		t.Fatalf("event leaks the error: %s", s)
	}
}

func TestTranscribeOversize(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared int64
		blob     int
	}{
		{"declared size", MaxAudioBytes + 1, 4},
		{"actual blob", 0, MaxAudioBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.put("b1", strings.Repeat("a", tc.blob))
			p := &fakeProv{}
			h.deps.Transcribe = &Role{Provider: p}
			n := h.new()

			msgs := turn(att("a1", "b1", "audio/wav", "long.wav", tc.declared))
			notices := n.Normalise(context.Background(), "t1", "s1", msgs)

			if len(notices) != 1 || notices[0] != NoticeAudioTooLarge {
				t.Fatalf("notices = %v", notices)
			}
			if _, stt := p.counts(); stt != 0 {
				t.Fatalf("stt calls = %d, want 0", stt)
			}
			if a := h.events.attrs(0); a["reason"] != reasonTooLarge {
				t.Fatalf("attrs = %v", a)
			}
		})
	}
}

func TestTranscriptCappedWithMarker(t *testing.T) {
	h := newHarness()
	h.put("b1", "x")
	p := &fakeProv{text: strings.Repeat("é", MaxDerivedRunes+500)}
	h.deps.Transcribe = &Role{Provider: p}
	n := h.new()

	msgs := turn(att("a1", "b1", "audio/wav", "long.wav", 1))
	n.Normalise(context.Background(), "t1", "s1", msgs)

	d := msgs[0].Attachments[0].Derived
	if d == nil || len([]rune(d.Text)) > MaxDerivedRunes {
		t.Fatalf("derived not capped: %d runes", len([]rune(d.Text)))
	}
	if !strings.HasSuffix(d.Text, TruncMarker) {
		t.Fatalf("missing marker: %q", d.Text[len(d.Text)-40:])
	}
}

func TestIdempotentSecondCall(t *testing.T) {
	h := newHarness()
	h.put("b1", "spoken words")
	p := &fakeProv{}
	h.deps.Transcribe = &Role{Provider: p}
	n := h.new()

	msgs := turn(att("a1", "b1", "audio/ogg", "voice.ogg", 12))
	n.Normalise(context.Background(), "t1", "s1", msgs)
	_, first := p.counts()
	n.Normalise(context.Background(), "t1", "s1", msgs) // Derived is set now
	_, second := p.counts()

	if first != 1 || second != 1 {
		t.Fatalf("stt calls: first=%d then=%d, want 1 and 1", first, second)
	}
	if got := len(h.persisted()); got != 1 {
		t.Fatalf("SetDerived calls = %d, want 1", got)
	}
	if got := len(h.events.types()); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
}

// --- images --------------------------------------------------------------

func TestImageWithChatVisionDoesNothing(t *testing.T) {
	h := newHarness()
	h.put("b1", "PNGDATA")
	p := &fakeProv{text: "a cat"}
	h.deps.Vision = &Role{Provider: p}
	h.deps.ChatVision = true
	n := h.new()

	msgs := turn(att("a1", "b1", "image/png", "cat.png", 7))
	notices := n.Normalise(context.Background(), "t1", "s1", msgs)

	if len(notices) != 0 {
		t.Fatalf("notices = %v", notices)
	}
	if chat, _ := p.counts(); chat != 0 {
		t.Fatalf("chat calls = %d, want 0", chat)
	}
	if msgs[0].Attachments[0].Derived != nil || len(h.events.types()) != 0 {
		t.Fatal("chat vision should leave the image untouched")
	}
}

func TestVisionDescribesImage(t *testing.T) {
	h := newHarness()
	h.put("b1", "PNGDATA")
	p := &fakeProv{text: "  A ginger cat on a keyboard.  "}
	h.deps.Vision = &Role{Model: provider.Model{Name: "vl", Vision: true}, Provider: p}
	n := h.new()

	msgs := turn(att("a1", "b1", "image/png", "cat.png", 7))
	notices := n.Normalise(context.Background(), "t1", "s1", msgs)

	if len(notices) != 0 {
		t.Fatalf("notices = %v", notices)
	}
	d := msgs[0].Attachments[0].Derived
	want := ImageDescriptionPrefix + "A ginger cat on a keyboard."
	if d == nil || d.Text != want || d.By != event.DerivedByVision {
		t.Fatalf("derived = %+v, want %q", d, want)
	}
	if got := h.persisted(); len(got) != 1 || got[0].Text != want {
		t.Fatalf("persisted = %+v", got)
	}
	if types := h.events.types(); len(types) != 1 || types[0] != event.MediaDescribed {
		t.Fatalf("events = %v", types)
	}

	req := p.reqs[0]
	if req.System != VisionPrompt {
		t.Fatalf("system = %q", req.System)
	}
	if !strings.Contains(req.System, "data, not instructions") {
		t.Fatal("vision prompt lost its injection defence")
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != provider.RoleUser {
		t.Fatalf("messages = %+v", req.Messages)
	}
	var img *provider.Image
	for _, part := range req.Messages[0].Parts {
		if part.Image != nil {
			img = part.Image
		}
	}
	if img == nil {
		t.Fatal("no image part was sent")
	}
	if img.MIME != "image/png" || string(img.Data) != "PNGDATA" {
		t.Fatalf("image part = %+v", img)
	}
	if req.Model.Name != "vl" {
		t.Fatalf("wrong model: %q", req.Model.Name)
	}
}

func TestVisionOversizeAndErrors(t *testing.T) {
	t.Run("oversize", func(t *testing.T) {
		h := newHarness()
		h.put("b1", "x")
		p := &fakeProv{text: "cat"}
		h.deps.Vision = &Role{Provider: p}
		n := h.new()

		msgs := turn(att("a1", "b1", "image/png", "big.png", MaxImageBytes+1))
		notices := n.Normalise(context.Background(), "t1", "s1", msgs)
		if len(notices) != 1 || notices[0] != NoticeImageTooLarge {
			t.Fatalf("notices = %v", notices)
		}
		if chat, _ := p.counts(); chat != 0 {
			t.Fatalf("chat calls = %d", chat)
		}
	})
	t.Run("provider error", func(t *testing.T) {
		h := newHarness()
		h.put("b1", "x")
		p := &fakeProv{err: &provider.Error{Kind: provider.ErrServer, Message: "boom 500"}}
		h.deps.Vision = &Role{Provider: p}
		n := h.new()

		msgs := turn(att("a1", "b1", "image/png", "cat.png", 1))
		notices := n.Normalise(context.Background(), "t1", "s1", msgs)
		if len(notices) != 1 || notices[0] != NoticeVisionFail {
			t.Fatalf("notices = %v", notices)
		}
		if a := h.events.attrs(0); a["reason"] != reasonError || a["error"] != string(provider.ErrServer) {
			t.Fatalf("attrs = %v", a)
		}
	})
	t.Run("empty reply", func(t *testing.T) {
		h := newHarness()
		h.put("b1", "x")
		p := &fakeProv{text: "   "}
		h.deps.Vision = &Role{Provider: p}
		n := h.new()

		msgs := turn(att("a1", "b1", "image/png", "cat.png", 1))
		notices := n.Normalise(context.Background(), "t1", "s1", msgs)
		if len(notices) != 1 || notices[0] != NoticeVisionFail {
			t.Fatalf("notices = %v", notices)
		}
		if msgs[0].Attachments[0].Derived != nil {
			t.Fatal("derived set from an empty description")
		}
	})
}

func TestImageRefusedWithoutVision(t *testing.T) {
	h := newHarness()
	h.put("b1", "PNGDATA")
	n := h.new() // no vision role, chat has no vision

	msgs := turn(att("a1", "b1", "image/jpeg", "cat.jpg", 7))
	notices := n.Normalise(context.Background(), "t1", "s1", msgs)

	if len(notices) != 1 || notices[0] != NoticeNoVision {
		t.Fatalf("notices = %v", notices)
	}
	if types := h.events.types(); len(types) != 1 || types[0] != event.MediaRefused {
		t.Fatalf("events = %v", types)
	}
	if a := h.events.attrs(0); a["reason"] != reasonNoVision || a["kind"] != string(event.KindImage) {
		t.Fatalf("attrs = %v", a)
	}
}

// --- video ---------------------------------------------------------------

func TestVideoAlwaysRefused(t *testing.T) {
	h := newHarness()
	h.put("b1", "MP4")
	p := &fakeProv{text: "x"}
	h.deps.Vision = &Role{Provider: p}
	h.deps.Transcribe = &Role{Provider: p}
	n := h.new()

	msgs := turn(att("a1", "b1", "video/mp4", "clip.mp4", 3))
	notices := n.Normalise(context.Background(), "t1", "s1", msgs)

	if len(notices) != 1 || notices[0] != NoticeNoVideo {
		t.Fatalf("notices = %v", notices)
	}
	chat, stt := p.counts()
	if chat != 0 || stt != 0 {
		t.Fatalf("calls chat=%d stt=%d, want none", chat, stt)
	}
	if a := h.events.attrs(0); a["reason"] != reasonVideo {
		t.Fatalf("attrs = %v", a)
	}
}

// --- ordering, concurrency, cancellation ---------------------------------

func TestConcurrencyLimitAndOrderStability(t *testing.T) {
	h := newHarness()
	p := &fakeProv{delay: 20 * time.Millisecond}
	h.deps.Transcribe = &Role{Provider: p}
	n := h.new()

	var as []event.Attachment
	const count = 9
	for i := range count {
		id := fmt.Sprintf("a%d", i)
		h.put("b"+id, "clip "+id)
		as = append(as, att(id, "b"+id, "audio/ogg", id+".ogg", 8))
	}
	msgs := turn(as...)
	n.Normalise(context.Background(), "t1", "s1", msgs)

	p.mu.Lock()
	peak := p.peak
	p.mu.Unlock()
	if peak > MaxInFlight {
		t.Fatalf("peak in flight = %d, want <= %d", peak, MaxInFlight)
	}
	if peak < 2 {
		t.Fatalf("peak in flight = %d, expected real concurrency", peak)
	}
	for i := range count {
		want := fmt.Sprintf("clip a%d", i)
		if d := msgs[0].Attachments[i].Derived; d == nil || d.Text != want {
			t.Fatalf("attachment %d derived = %+v, want %q", i, d, want)
		}
	}
	// Persisted order and event order follow the attachment order.
	got := h.persisted()
	for i, d := range got {
		if d.Text != fmt.Sprintf("clip a%d", i) {
			t.Fatalf("persisted[%d] = %q", i, d.Text)
		}
	}
	for i, e := range h.events.ev {
		if a := h.events.attrs(i); a["attachment"] != fmt.Sprintf("a%d", i) {
			t.Fatalf("event %d = %v (%s)", i, a, e.Type)
		}
	}
}

func TestNoticeOrderAndDedup(t *testing.T) {
	h := newHarness()
	h.put("b1", "x")
	n := h.new() // no roles at all

	msgs := []event.Message{
		{ID: "m1", Role: event.RoleUser, Attachments: []event.Attachment{
			att("a1", "b1", "video/mp4", "clip.mp4", 1),
			att("a2", "b1", "image/png", "one.png", 1),
			att("a3", "b1", "image/png", "two.png", 1), // same notice, once
		}},
		{ID: "m2", Role: event.RoleUser, Attachments: []event.Attachment{
			att("a4", "b1", "audio/ogg", "voice.ogg", 1),
		}},
		{ID: "m3", Role: event.RoleAssistant, Attachments: []event.Attachment{
			att("a5", "b1", "audio/ogg", "ignored.ogg", 1), // not a user message
		}},
	}
	notices := n.Normalise(context.Background(), "t1", "s1", msgs)

	want := []string{NoticeNoVideo, NoticeNoVision, NoticeNoTranscribe}
	if len(notices) != len(want) {
		t.Fatalf("notices = %v, want %v", notices, want)
	}
	for i := range want {
		if notices[i] != want[i] {
			t.Fatalf("notices = %v, want %v", notices, want)
		}
	}
	if got := len(h.events.types()); got != 4 { // one per refused attachment
		t.Fatalf("events = %d, want 4", got)
	}
	if msgs[2].Attachments[0].Derived != nil {
		t.Fatal("assistant attachment was processed")
	}
}

func TestContextCancelled(t *testing.T) {
	h := newHarness()
	h.put("b1", "spoken")
	p := &fakeProv{delay: time.Hour}
	h.deps.Transcribe = &Role{Provider: p}
	n := h.new()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msgs := turn(att("a1", "b1", "audio/ogg", "voice.ogg", 6))

	done := make(chan []string, 1)
	go func() { done <- n.Normalise(ctx, "t1", "s1", msgs) }()
	select {
	case notices := <-done:
		if len(notices) != 0 {
			t.Fatalf("notices = %v", notices)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Normalise did not return on a cancelled context")
	}
	if _, stt := p.counts(); stt != 0 {
		t.Fatalf("stt calls = %d, want 0", stt)
	}
	if len(h.events.types()) != 0 {
		t.Fatal("events emitted for a cancelled turn")
	}
}

func TestCancelDuringWork(t *testing.T) {
	h := newHarness()
	h.put("b1", "spoken")
	p := &fakeProv{delay: time.Hour}
	h.deps.Transcribe = &Role{Provider: p}
	n := h.new()

	ctx, cancel := context.WithCancel(context.Background())
	msgs := turn(att("a1", "b1", "audio/ogg", "voice.ogg", 6))
	done := make(chan []string, 1)
	go func() { done <- n.Normalise(ctx, "t1", "s1", msgs) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case notices := <-done:
		// The call was aborted: a failure notice is fine, a hang is not.
		if len(notices) > 1 {
			t.Fatalf("notices = %v", notices)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Normalise did not return after cancellation")
	}
}

func TestSetDerivedFailureKeepsTurnUsable(t *testing.T) {
	h := newHarness()
	h.put("b1", "spoken words")
	h.setErr = errors.New("db is locked")
	p := &fakeProv{}
	h.deps.Transcribe = &Role{Provider: p}
	n := h.new()

	msgs := turn(att("a1", "b1", "audio/ogg", "voice.ogg", 12))
	if notices := n.Normalise(context.Background(), "t1", "s1", msgs); len(notices) != 0 {
		t.Fatalf("notices = %v", notices)
	}
	if d := msgs[0].Attachments[0].Derived; d == nil || d.Text != "spoken words" {
		t.Fatalf("derived = %+v", d)
	}
}

func TestBlobFailureIsANotice(t *testing.T) {
	h := newHarness() // blob "b1" missing
	p := &fakeProv{}
	h.deps.Transcribe = &Role{Provider: p}
	n := h.new()

	msgs := turn(att("a1", "b1", "audio/ogg", "voice.ogg", 12))
	notices := n.Normalise(context.Background(), "t1", "s1", msgs)
	if len(notices) != 1 || notices[0] != NoticeTranscribeFail {
		t.Fatalf("notices = %v", notices)
	}
	if strings.Contains(notices[0], "blob") {
		t.Fatalf("notice leaks the error: %q", notices[0])
	}
	if a := h.events.attrs(0); a["reason"] != reasonError {
		t.Fatalf("attrs = %v", a)
	}
}

func TestNoAttachmentsIsANoOp(t *testing.T) {
	h := newHarness()
	n := h.new()
	msgs := []event.Message{{ID: "m1", Role: event.RoleUser, Text: "hello"}}
	if notices := n.Normalise(context.Background(), "t1", "s1", msgs); notices != nil {
		t.Fatalf("notices = %v", notices)
	}
	if len(h.events.types()) != 0 {
		t.Fatal("events emitted")
	}
}

func TestEventsCarryNoContent(t *testing.T) {
	h := newHarness()
	h.put("b1", "the launch code is 1234")
	h.put("b2", "IMGBYTES")
	p := &fakeProv{}
	v := &fakeProv{text: "a whiteboard with the launch code 1234"}
	h.deps.Transcribe = &Role{Provider: p}
	h.deps.Vision = &Role{Provider: v}
	n := h.new()

	msgs := turn(
		att("a1", "b1", "audio/ogg", "voice.ogg", 22),
		att("a2", "b2", "image/png", "board.png", 8),
	)
	n.Normalise(context.Background(), "t1", "s1", msgs)

	for i, e := range h.events.ev {
		s := string(e.Attrs)
		for _, bad := range []string{"launch code", "1234", "whiteboard"} {
			if strings.Contains(s, bad) {
				t.Fatalf("event %d (%s) carries content %q: %s", i, e.Type, bad, s)
			}
		}
	}
}

var visionCost = provider.NanoUSD(700000)

// usageAttrs checks the usage attributes of one event: the tokens always,
// the cost only when the call reported one.
func usageAttrs(t *testing.T, a map[string]any, u provider.Usage) {
	t.Helper()
	if a["prompt_tokens"] != float64(u.PromptTokens) || a["completion_tokens"] != float64(u.CompletionTokens) {
		t.Errorf("tokens = %v/%v, want %d/%d", a["prompt_tokens"], a["completion_tokens"], u.PromptTokens, u.CompletionTokens)
	}
	got, ok := a["cost_nanousd"]
	switch {
	case u.Cost == nil && ok:
		t.Errorf("cost = %v on an unpriced call", got)
	case u.Cost != nil && (!ok || got != float64(*u.Cost)):
		t.Errorf("cost = %v (present %v), want %v", got, ok, *u.Cost)
	}
}

// media.described carries the vision call's usage, the cost only when the
// upstream reported one.
func TestVisionDescribedCarriesUsage(t *testing.T) {
	for _, u := range []provider.Usage{
		{PromptTokens: 700, CompletionTokens: 60, Cost: &visionCost},
		{PromptTokens: 700, CompletionTokens: 60},
	} {
		h := newHarness()
		h.put("b1", "PNGDATA")
		h.deps.Vision = &Role{Model: provider.Model{Name: "vl", Vision: true},
			Provider: &fakeProv{text: "a cat", usage: u}}
		n := h.new()
		n.Normalise(context.Background(), "t1", "s1", turn(att("a1", "b1", "image/png", "cat.png", 7)))
		if types := h.events.types(); len(types) != 1 || types[0] != event.MediaDescribed {
			t.Fatalf("events = %v", types)
		}
		usageAttrs(t, h.events.attrs(0), u)
	}
}

// An empty description is refused, but the vision call was answered and
// paid for: media.refused carries its usage and counts. A refusal that made
// no call carries none.
func TestVisionEmptyDescriptionKeepsItsUsage(t *testing.T) {
	u := provider.Usage{PromptTokens: 700, CompletionTokens: 1, Cost: &visionCost}
	h := newHarness()
	h.put("b1", "PNGDATA")
	h.deps.Vision = &Role{Model: provider.Model{Name: "vl", Vision: true}, Provider: &fakeProv{text: "  ", usage: u}}
	h.new().Normalise(context.Background(), "t1", "s1", turn(att("a1", "b1", "image/png", "cat.png", 7)))
	if types := h.events.types(); len(types) != 1 || types[0] != event.MediaRefused {
		t.Fatalf("events = %v", types)
	}
	a := h.events.attrs(0)
	usageAttrs(t, a, u)
	if got := event.UsageOf(event.MediaRefused, a); got.PromptTokens != 700 || got.Cost == nil {
		t.Errorf("counted as %+v", got)
	}

	h = newHarness()
	h.put("b1", "PNGDATA")
	h.new().Normalise(context.Background(), "t1", "s1", turn(att("a1", "b1", "image/png", "cat.png", 7)))
	if _, ok := h.events.attrs(0)["prompt_tokens"]; ok {
		t.Errorf("a refusal without a call carries usage: %v", h.events.attrs(0))
	}
}

// media.transcribed carries the speech-to-text call's usage the same way.
func TestTranscribedCarriesUsage(t *testing.T) {
	cost := provider.NanoUSD(200000)
	for _, u := range []provider.Usage{
		{PromptTokens: 14, CompletionTokens: 45, Cost: &cost},
		{}, // whisper reports seconds, not tokens, and no price
	} {
		h := newHarness()
		h.put("b1", "OggS")
		h.deps.Transcribe = &Role{Model: provider.Model{Name: "whisper"}, Provider: &fakeProv{text: "hola", usage: u}}
		h.new().Normalise(context.Background(), "t1", "s1", turn(att("a1", "b1", "audio/ogg", "v.ogg", 4)))
		if types := h.events.types(); len(types) != 1 || types[0] != event.MediaTranscribed {
			t.Fatalf("events = %v", types)
		}
		a := h.events.attrs(0)
		usageAttrs(t, a, u)
		if got := event.UsageOf(event.MediaTranscribed, a); got.PromptTokens != u.PromptTokens || (got.Cost == nil) != (u.Cost == nil) {
			t.Errorf("counted as %+v", got)
		}
	}
}
