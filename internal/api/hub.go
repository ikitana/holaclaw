package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

const (
	// DefaultHeartbeat is the comment sent to keep an idle stream and every
	// proxy on the way alive (docs/reference/api.md).
	DefaultHeartbeat = 15 * time.Second
	// subscriberBuffer is how far one slow client may lag before its stream is
	// closed. It resumes with Last-Event-ID; the emitter is never blocked.
	subscriberBuffer = 256
	// replayPage is how many stored events one resume page carries.
	replayPage = 256
	// sessionCacheMax bounds the session lookup cache of the hub.
	sessionCacheMax = 1024
)

// Hub fans trace events out to the SSE streams. It is an event.Sink: the
// daemon adds it to the fan-out *after* the store sink, so every event it
// carries already has the id a client can resume from.
type Hub struct {
	store store.Store
	log   *slog.Logger

	// Heartbeat overrides DefaultHeartbeat; tests shorten it.
	Heartbeat time.Duration

	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	closed bool
	// partials is the text each session's running turn has streamed so far,
	// for a stream that joins mid-turn. Kept under mu with the fan-out, so a
	// joining stream gets every delta once: in its snapshot or live.
	partials map[event.ID]*partial
	// lastID is the highest stored event id fanned out so far.
	lastID event.ID

	cacheMu sync.Mutex
	cache   map[event.ID]store.Session
}

// partial is the visible text of one running turn.
type partial struct {
	trace event.ID
	text  []byte
}

// joinPoint is where a new subscriber came in: the partial text of every
// running turn at that instant, and the last stored event it already covers.
type joinPoint struct {
	partials []event.Event
	lastID   event.ID
}

// NewHub returns a hub reading resume pages and session ownership from st.
func NewHub(st store.Store, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		store: st, log: log, subs: map[*subscriber]struct{}{},
		partials: map[event.ID]*partial{}, cache: map[event.ID]store.Session{},
	}
}

var _ event.Sink = (*Hub)(nil)

// Emit hands an event to every subscriber. A subscriber that cannot keep up is
// closed rather than waited for: the emitter is on the reply path.
func (h *Hub) Emit(_ context.Context, e event.Event) { h.publish(e, "") }

func (h *Hub) publish(e event.Event, delta string) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.track(e, delta)
	var slow []*subscriber
	for s := range h.subs {
		select {
		case s.ch <- e:
		default:
			slow = append(slow, s)
		}
	}
	for _, s := range slow {
		delete(h.subs, s)
	}
	h.mu.Unlock()
	for _, s := range slow {
		s.drop()
	}
}

// PublishToken streams one visible token delta. model.token is ephemeral: it
// is never stored, so it is sent without an `id:` and cannot be resumed.
func (h *Hub) PublishToken(sessionID, traceID event.ID, delta string) {
	attrs, err := json.Marshal(map[string]string{"delta": delta})
	if err != nil {
		return
	}
	h.publish(event.Event{
		Schema:    event.SchemaVersion,
		TraceID:   traceID,
		SessionID: sessionID,
		Type:      event.ModelToken,
		Time:      time.Now().UTC(),
		Attrs:     attrs,
	}, delta)
}

// track keeps the partial text of each running turn. A turn's text ends with
// its reply or its run; a reply from another trace (a task delivering into the
// main chat) leaves the running turn's text alone. Called with mu held.
func (h *Hub) track(e event.Event, delta string) {
	if e.ID > h.lastID {
		h.lastID = e.ID
	}
	if e.SessionID == "" {
		return
	}
	switch e.Type {
	case event.ModelToken:
		p := h.partials[e.SessionID]
		if p == nil || p.trace != e.TraceID {
			p = &partial{trace: e.TraceID}
			h.partials[e.SessionID] = p
		}
		p.text = append(p.text, delta...)
	case event.Reply, event.RunFinished, event.RunAborted:
		if p := h.partials[e.SessionID]; p != nil && p.trace == e.TraceID {
			delete(h.partials, e.SessionID)
		}
	}
}

// snapshot renders the partial text of the running turns a stream focused on
// focus sees in full: that session's, or every session's without a focus.
// Called with mu held.
func (h *Hub) snapshot(focus event.ID) []event.Event {
	var out []event.Event
	for sid, p := range h.partials {
		if focus != "" && sid != focus {
			continue
		}
		attrs, err := json.Marshal(map[string]string{"text": string(p.text)})
		if err != nil {
			continue
		}
		out = append(out, event.Event{
			Schema:    event.SchemaVersion,
			TraceID:   p.trace,
			SessionID: sid,
			Type:      event.ModelPartial,
			Time:      time.Now().UTC(),
			Attrs:     attrs,
		})
	}
	return out
}

// Close ends every stream. Clients reconnect with Last-Event-ID.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.subs = map[*subscriber]struct{}{}
	h.mu.Unlock()
	for _, s := range subs {
		s.drop()
	}
}

// Disconnect ends every stream opened with a token of identity, and reports
// how many. The daemon calls it when the identity is disabled; a reconnect is
// refused at authentication.
func (h *Hub) Disconnect(identity event.ID) int {
	if identity == "" {
		return 0
	}
	h.mu.Lock()
	var gone []*subscriber
	for s := range h.subs {
		if s.identity == identity {
			gone = append(gone, s)
			delete(h.subs, s)
		}
	}
	h.mu.Unlock()
	for _, s := range gone {
		s.drop()
	}
	return len(gone)
}

// Subscribers is the number of live streams (status and tests).
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// subscribe registers a stream of identity focused on focus and returns where
// it joined.
func (h *Hub) subscribe(focus, identity event.ID) (*subscriber, joinPoint) {
	s := &subscriber{identity: identity, ch: make(chan event.Event, subscriberBuffer), dropped: make(chan struct{})}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		s.drop()
		return s, joinPoint{}
	}
	h.subs[s] = struct{}{}
	join := joinPoint{partials: h.snapshot(focus), lastID: h.lastID}
	h.mu.Unlock()
	return s, join
}

func (h *Hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// session returns a session for the stream filter, cached: a busy stream must
// not hit the database once per event. One it cannot read is the zero
// session, which no identity owns.
func (h *Hub) session(ctx context.Context, id event.ID) store.Session {
	h.cacheMu.Lock()
	info, ok := h.cache[id]
	h.cacheMu.Unlock()
	if ok {
		return info
	}
	if h.store != nil {
		s, err := h.store.GetSession(ctx, id)
		switch {
		case err == nil:
			info = s
		case !errors.Is(err, store.ErrNotFound):
			// A transient failure is not cached: the next event asks again.
			return store.Session{}
		}
	}
	h.cacheMu.Lock()
	if len(h.cache) >= sessionCacheMax {
		h.cache = map[event.ID]store.Session{}
	}
	h.cache[id] = info
	h.cacheMu.Unlock()
	return info
}

// subscriber is one live stream's mailbox.
type subscriber struct {
	identity event.ID // the token's identity; empty for the socket
	ch       chan event.Event
	once     sync.Once
	dropped  chan struct{}
}

func (s *subscriber) drop() { s.once.Do(func() { close(s.dropped) }) }

// --- the stream ---------------------------------------------------------------

// StreamEvents is the one resumable stream per client window. It replays from
// the store when the client resumes, then switches to live without a gap or a
// duplicate: the subscription is taken *before* the replay, so events emitted
// meanwhile are buffered rather than lost, and the drain skips everything the
// replay already delivered. Between the two it sends a model.partial for each
// turn in flight, so a client joining mid-turn sees the text streamed before
// it came; the replay stops at the join point so that text is not older than
// what precedes it.
func (h *handlers) StreamEvents(w http.ResponseWriter, r *http.Request, params gen.StreamEventsParams) {
	hub := h.b.Hub
	if hub == nil {
		notImplemented(w, "events")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.b.fail(w, "stream events", fmt.Errorf("response writer does not flush"))
		return
	}
	p, _ := PrincipalFrom(r.Context())
	st := &stream{hub: hub, w: w, flush: flusher.Flush, principal: p, log: h.b.logger()}
	if params.Focus != nil {
		st.focus = event.ID(*params.Focus)
	}

	sub, join := hub.subscribe(st.focus, p.UserID)
	defer hub.unsubscribe(sub)
	st.dropped = sub.dropped
	// Authentication read the status before the subscription existed: a
	// disable committed in between found no stream to close, so look again.
	if p.UserID != "" && h.b.Store != nil {
		if i, err := h.b.Store.GetIdentity(r.Context(), p.UserID); err == nil && i.Disabled() {
			writeError(w, http.StatusForbidden, CodeIdentityDisabled, "the identity is disabled")
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Nothing on the way may buffer a stream that is mostly silence.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	if params.LastEventID != nil && *params.LastEventID != "" {
		if err := st.replay(ctx, event.ID(*params.LastEventID), join.lastID); err != nil {
			return
		}
	}
	for _, e := range join.partials {
		if err := st.send(ctx, e); err != nil {
			return
		}
	}

	beat := hub.Heartbeat
	if beat <= 0 {
		beat = DefaultHeartbeat
	}
	ticker := time.NewTicker(beat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.dropped:
			return
		case e := <-sub.ch:
			// Everything the replay already delivered is skipped; ephemeral
			// events carry no id and are never skipped.
			if e.ID != "" && st.last != "" && e.ID <= st.last {
				continue
			}
			if err := st.send(ctx, e); err != nil {
				return
			}
		case <-ticker.C:
			if _, err := io.WriteString(st.w, ": hb\n\n"); err != nil {
				return
			}
			st.flush()
		}
	}
}

// stream is one client window: its principal, its focus and where it got to.
type stream struct {
	hub       *Hub
	w         io.Writer
	flush     func()
	principal auth.Principal
	focus     event.ID
	last      event.ID // highest event id sent so far
	log       *slog.Logger
	dropped   <-chan struct{} // closed when the hub drops the subscription
}

// errDropped ends a stream the hub dropped mid-replay.
var errDropped = errors.New("api: stream dropped")

// replay walks the stored events after the client's last id, in pages, until
// it is caught up to upTo; anything later arrives live. An empty upTo is a hub
// that has fanned out nothing yet, so everything stored is older.
func (s *stream) replay(ctx context.Context, after, upTo event.ID) error {
	if s.hub.store == nil {
		return nil
	}
	s.last = after
	for {
		page, err := s.hub.store.ListEventsAfter(ctx, after, "", replayPage)
		if err != nil {
			s.log.Error("api: replay events", "after", after, "error", err)
			return err
		}
		for _, e := range page {
			if upTo != "" && e.ID > upTo {
				return nil
			}
			if err := s.send(ctx, e); err != nil {
				return err
			}
			after = e.ID
		}
		if len(page) < replayPage {
			return nil
		}
	}
}

// send renders one event for this client and writes it, unless the client may
// not see it at all.
func (s *stream) send(ctx context.Context, e event.Event) error {
	// A stream the hub dropped ends here too, not only in the live loop: a
	// long replay must not outlast it.
	select {
	case <-s.dropped:
		return errDropped
	default:
	}
	if e.ID > s.last {
		s.last = e.ID
	}
	data, ok := s.render(ctx, e)
	if !ok {
		return nil
	}
	if err := writeSSE(s.w, string(e.ID), string(e.Type), data); err != nil {
		return err
	}
	s.flush()
	return nil
}

// slimEvent is what a client gets for the sessions it is not focused on:
// enough to know something happened, nothing more (docs/reference/api.md).
type slimEvent struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	TraceID   string `json:"trace_id"`
}

// render applies the access filter and the focus slimming.
func (s *stream) render(ctx context.Context, e event.Event) ([]byte, bool) {
	// A session's events reach the streams that may see the session. An event
	// with no session belongs to no conversation: admin scope watches it, a
	// client token has no one it may watch there (docs/identity.md).
	switch {
	case e.SessionID == "":
		if s.principal.Scope == auth.ScopeClient {
			return nil, false
		}
	case isSocket(s.principal):
		// visible would say yes too; this skips the session lookup.
	case !visible(s.principal, s.hub.session(ctx, e.SessionID)):
		return nil, false
	}
	if s.focus != "" && e.SessionID != s.focus {
		return marshal(slimEvent{
			ID: string(e.ID), Type: string(e.Type),
			SessionID: string(e.SessionID), TraceID: string(e.TraceID),
		})
	}
	return marshal(toGenEvent(e))
}

func marshal(v any) ([]byte, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return b, true
}

// writeSSE writes one frame. An event with no id (an ephemeral type)
// leaves the client's Last-Event-ID untouched, so a resume still lands on the
// last stored event.
func writeSSE(w io.Writer, id, typ string, data []byte) error {
	var buf []byte
	if id != "" {
		buf = append(append(append(buf, "id: "...), id...), '\n')
	}
	buf = append(append(append(buf, "event: "...), typ...), '\n')
	buf = append(append(append(buf, "data: "...), data...), '\n', '\n')
	_, err := w.Write(buf)
	return err
}

// toGenEvent renders a trace event on the wire.
func toGenEvent(e event.Event) gen.Event {
	out := gen.Event{
		Schema:    e.Schema,
		Id:        string(e.ID),
		TraceId:   string(e.TraceID),
		SpanId:    string(e.SpanID),
		ParentId:  optID(e.ParentID),
		SessionId: optID(e.SessionID),
		Type:      string(e.Type),
		Time:      e.Time,
		MessageId: optID(e.MessageID),
	}
	if e.Duration > 0 {
		out.DurationNs = ptr(int64(e.Duration))
	}
	if len(e.Attrs) > 0 {
		var attrs map[string]any
		if err := json.Unmarshal(e.Attrs, &attrs); err == nil {
			out.Attrs = &attrs
		}
	}
	return out
}
