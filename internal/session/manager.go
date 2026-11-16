package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// Turner runs one turn of one session. *agent.Runner satisfies it; tests and
// the eval harness substitute their own.
type Turner interface {
	Turn(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error)
}

var _ Turner = (*agent.Runner)(nil)

// ErrShutdown is returned by Submit after Shutdown.
var ErrShutdown = errors.New("session: manager shut down")

const (
	// DefaultDebounce is how long an actor waits after a wake-up before it
	// drains the mailbox, so rapid messages coalesce into one turn.
	DefaultDebounce = 600 * time.Millisecond
	// DefaultIdleTimeout is how long an actor with nothing to do survives.
	DefaultIdleTimeout = 5 * time.Minute
	// maxDebounceFactor caps the total debounce wait at n × Debounce, so a
	// steady trickle of messages cannot postpone a turn forever.
	maxDebounceFactor = 3
)

// apologyText is delivered when a turn fails. It names no internals.
const apologyText = "Sorry, something went wrong on my side. Please try again."

// Deps is everything the manager needs from the daemon. Only Store is
// strictly required; the function fields are supplied by the shell that owns
// providers, tools and delivery.
type Deps struct {
	Store  store.Store
	Events event.Sink
	// Runner returns the loop for a session (the daemon builds it with the
	// policy gate inside). Called once per turn.
	Runner func(sess store.Session) (Turner, error)
	// Prepare fills the per-turn input: injections (memory, time), the
	// trusted tool.Call template, the image resolver. Called once per turn,
	// after the mailbox is drained. SessionID, TraceID and History are
	// stamped by the manager.
	Prepare func(ctx context.Context, sess store.Session, pending []event.Message) (agent.TurnInput, error)
	// Deliver sends the final reply where the conversation lives. The target
	// is always sess.Origin, never model-set (ARCHITECTURE.md §2).
	Deliver func(ctx context.Context, sess store.Session, traceID event.ID, reply event.Message) error
	// ProviderOf names the provider a session's turns hit, for lane caps.
	ProviderOf func(sess store.Session) string
	// LaneCaps is the max concurrent turns per (provider, lane).
	LaneCaps map[Lane]int
	// Debounce is the coalescing window; zero means DefaultDebounce, use a
	// negative value to disable it (tests).
	Debounce time.Duration
	// IdleTimeout is how long an idle actor lives; zero means the default.
	IdleTimeout time.Duration
	Now         func() time.Time
	Logger      *slog.Logger
}

// Manager owns the actor registry: one actor per session, created and looked
// up in a single atomic operation (docs/sessions.md).
type Manager struct {
	d   Deps
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	actors map[event.ID]*actor
	closed bool
	wg     sync.WaitGroup

	semMu sync.Mutex
	sems  map[string]chan struct{}

	// hookIdleExit is called by an idle actor just before it takes the
	// registry lock to remove itself; tests use it to drive a Submit into the
	// lost-wakeup window.
	hookIdleExit func()
}

// New builds a manager. Start replays the persisted mailboxes.
func New(d Deps) *Manager {
	if d.Events == nil {
		d.Events = event.Discard
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Debounce == 0 {
		d.Debounce = DefaultDebounce
	}
	if d.IdleTimeout <= 0 {
		d.IdleTimeout = DefaultIdleTimeout
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{d: d, log: log, actors: map[event.ID]*actor{}, sems: map[string]chan struct{}{}}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m
}

func (m *Manager) debounce() time.Duration {
	if m.d.Debounce < 0 {
		return 0
	}
	return m.d.Debounce
}

// Start heals turn tokens left behind by a crash and wakes an actor for every
// session that still holds mailbox entries: a restart replays instead of
// losing input.
func (m *Manager) Start(ctx context.Context) error {
	healed, err := m.d.Store.HealTurns(ctx)
	if err != nil {
		return fmt.Errorf("session: heal turns: %w", err)
	}
	if len(healed) > 0 {
		m.log.Warn("session: healed turns left by a crash", "sessions", len(healed))
	}
	ids, err := m.d.Store.SessionsWithPending(ctx)
	if err != nil {
		return fmt.Errorf("session: sessions with pending: %w", err)
	}
	for _, id := range ids {
		sess, err := m.d.Store.GetSession(ctx, id)
		if err != nil {
			m.log.Error("session: replay: load session", "session", id, "error", err)
			continue
		}
		if _, err := m.wake(sess); err != nil {
			return err
		}
	}
	return nil
}

// Submit stores msg in the session mailbox and wakes the actor. It returns
// once the message is durably stored; storing the same id twice is a no-op
// that reports duplicate and starts no second turn.
func (m *Manager) Submit(ctx context.Context, sess store.Session, msg event.Message) (bool, error) {
	if msg.ID == "" {
		msg.ID = event.NewID()
	}
	msg.SessionID = sess.ID
	if msg.Schema == 0 {
		msg.Schema = event.SchemaVersion
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = m.d.Now()
	}
	inserted, err := m.d.Store.AppendMessage(ctx, msg, true)
	if err != nil {
		return false, fmt.Errorf("session: submit: %w", err)
	}
	if !inserted {
		return true, nil
	}
	if _, err := m.wake(sess); err != nil {
		return false, err
	}
	return false, nil
}

// wake looks the actor up and creates it if needed, then signals it — all
// under one lock, so a concurrent Submit can neither create a second actor
// nor lose its wake-up to an actor that is exiting.
func (m *Manager) wake(sess store.Session) (*actor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrShutdown
	}
	a, ok := m.actors[sess.ID]
	if !ok {
		a = &actor{m: m, id: sess.ID, wake: make(chan struct{}, 1)}
		m.actors[sess.ID] = a
		m.wg.Add(1)
		go a.run()
	}
	a.signal()
	return a, nil
}

// Abort cancels the running turn of a session. Pending messages are kept and
// run as the next turn.
func (m *Manager) Abort(sessionID event.ID) bool {
	m.mu.Lock()
	a := m.actors[sessionID]
	m.mu.Unlock()
	if a == nil {
		return false
	}
	return a.abort()
}

// AbortIdentity cancels the running turns that are id's alone: those of a
// session it owns (its DMs and tasks, or every turn of a room when id is the
// room), and those whose only sender is id. A room turn that also answers
// somebody else keeps going. It reports how many turns it stopped.
func (m *Manager) AbortIdentity(id event.ID) int {
	if id == "" {
		return 0
	}
	m.mu.Lock()
	actors := make([]*actor, 0, len(m.actors))
	for _, a := range m.actors {
		actors = append(actors, a)
	}
	m.mu.Unlock()
	n := 0
	for _, a := range actors {
		if a.abortFor(id) {
			n++
		}
	}
	return n
}

// Senders is who a turn answers: the distinct users its messages came from,
// or the session's owner when none carries one (a task run, rows written
// before the field existed). Empty only when neither is known.
func Senders(owner event.ID, msgs []event.Message) []event.ID {
	var ids []event.ID
	seen := map[event.ID]bool{}
	for _, m := range msgs {
		if m.UserID == "" || seen[m.UserID] {
			continue
		}
		seen[m.UserID] = true
		ids = append(ids, m.UserID)
	}
	if len(ids) == 0 && owner != "" {
		ids = append(ids, owner)
	}
	return ids
}

// Busy reports whether a turn is running for the session.
func (m *Manager) Busy(sessionID event.ID) bool {
	m.mu.Lock()
	a := m.actors[sessionID]
	m.mu.Unlock()
	return a != nil && a.busy()
}

// Active is the number of sessions running a turn.
func (m *Manager) Active() int {
	m.mu.Lock()
	actors := make([]*actor, 0, len(m.actors))
	for _, a := range m.actors {
		actors = append(actors, a)
	}
	m.mu.Unlock()
	n := 0
	for _, a := range actors {
		if a.busy() {
			n++
		}
	}
	return n
}

// Shutdown stops accepting submissions, cancels running turns and waits for
// every actor to finish. Messages already stored stay pending and are
// replayed by the next Start.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) emit(ctx context.Context, e event.Event) {
	m.d.Events.Emit(context.WithoutCancel(ctx), e)
}

// actor is one session: a goroutine draining the persisted mailbox in order.
type actor struct {
	m    *Manager
	id   event.ID
	wake chan struct{}

	mu sync.Mutex

	running bool
	aborted bool
	cancel  context.CancelFunc
	// owner and senders are the running turn's once it has taken its input;
	// AbortIdentity matches on them.
	owner   event.ID
	senders []event.ID
}

func (a *actor) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *actor) busy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

func (a *actor) abort() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.cancel == nil {
		return false
	}
	a.aborted = true
	a.cancel()
	return true
}

// abortFor aborts the running turn when it is id's alone. A turn that has not
// taken its input yet is left alone: Prepare drops it if it turns out to be.
func (a *actor) abortFor(id event.ID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.cancel == nil || len(a.senders) == 0 {
		return false
	}
	if a.owner != id && slices.ContainsFunc(a.senders, func(s event.ID) bool { return s != id }) {
		return false
	}
	a.aborted = true
	a.cancel()
	return true
}

func (a *actor) run() {
	m := a.m
	defer m.wg.Done()
	idle := time.NewTimer(m.d.IdleTimeout)
	defer idle.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-a.wake:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			a.drain()
			idle.Reset(m.d.IdleTimeout)
		case <-idle.C:
			if a.exit() {
				return
			}
			idle.Reset(m.d.IdleTimeout)
		}
	}
}

// exit removes the actor from the registry. It reports false when a wake-up
// landed in the meantime: the registry lock is the same one Submit holds
// while it signals, so a message can never be left with no actor to run it.
func (a *actor) exit() bool {
	m := a.m
	if m.hookIdleExit != nil {
		m.hookIdleExit()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-a.wake:
		a.signal() // put it back; the run loop picks it up
		return false
	default:
	}
	delete(m.actors, a.id)
	return true
}

// drain coalesces, then runs turns until the mailbox is empty. Messages that
// arrive while a turn runs stay pending and form the next turn; they never
// interrupt the running one.
func (a *actor) drain() {
	for {
		a.coalesce()
		if a.m.ctx.Err() != nil {
			return
		}
		if !a.turn() {
			return
		}
		// Messages stored during the turn left a wake-up behind; consume it,
		// the next TakePending sees them anyway.
		select {
		case <-a.wake:
		default:
		}
	}
}

// coalesce waits out the debounce window, restarting it on every new message
// and capping the total wait, so a burst becomes one turn.
func (a *actor) coalesce() {
	d := a.m.debounce()
	if d <= 0 {
		return
	}
	deadline := a.m.d.Now().Add(maxDebounceFactor * d)
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return
		case <-a.m.ctx.Done():
			return
		case <-a.wake:
			left := deadline.Sub(a.m.d.Now())
			if left <= 0 {
				return
			}
			if left > d {
				left = d
			}
			t.Reset(left)
		}
	}
}

// turn runs one turn if the mailbox holds anything, and reports whether it
// did: lane slot, turn ownership, history, the loop, delivery. A failure never
// kills the actor.
func (a *actor) turn() bool {
	m := a.m
	ctx, cancel := context.WithCancel(m.ctx)
	a.mu.Lock()
	a.running, a.aborted, a.cancel = true, false, cancel
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.running, a.cancel, a.owner, a.senders = false, nil, "", nil
		a.mu.Unlock()
	}()

	// Re-read the session row: compaction may have moved SummaryMsgID.
	sess, err := m.d.Store.GetSession(ctx, a.id)
	if err != nil {
		m.log.Error("session: load session", "session", a.id, "error", err)
		return false
	}
	release, err := m.acquire(ctx, m.providerOf(sess), LaneOf(sess))
	if err != nil {
		m.log.Info("session: turn cancelled while queued", "session", sess.ID)
		return false
	}
	defer release()

	// The loop is built before the mailbox is consumed: if that fails, the
	// messages stay pending and a restart replays them.
	if m.d.Runner == nil {
		m.log.Error("session: no runner configured", "session", sess.ID)
		return false
	}
	runner, err := m.d.Runner(sess)
	if err != nil {
		m.log.Error("session: build runner", "session", sess.ID, "error", err)
		return false
	}
	// In a room each mention is its own request: the turn ends at the first
	// one, and the next mention gets its own turn and its own reply.
	perMention := sess.Origin.ConversationKind == event.ConversationRoom
	msgs, err := m.d.Store.TakePending(ctx, sess.ID, perMention)
	if err != nil {
		m.log.Error("session: take pending", "session", sess.ID, "error", err)
		return false
	}
	if len(msgs) == 0 {
		return false
	}
	a.mu.Lock()
	a.owner, a.senders = sess.UserID, Senders(sess.UserID, msgs)
	a.mu.Unlock()
	// The turn's trace is the trace of the last message it answers; each
	// inbound message got its trace id at the router (docs/traceability.md).
	traceID := msgs[len(msgs)-1].TraceID

	token, err := m.beginTurn(ctx, sess.ID)
	if err != nil {
		m.log.Error("session: begin turn", "session", sess.ID, "trace", traceID, "error", err)
		// The input was already taken from the mailbox: put it back, or a
		// transient store error would silently drop the message.
		a.repend(ctx, sess.ID, msgs)
		return true
	}
	defer func() {
		if err := m.d.Store.EndTurn(context.WithoutCancel(ctx), sess.ID, token); err != nil {
			m.log.Error("session: end turn", "session", sess.ID, "error", err)
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("session: turn panicked", "session", sess.ID, "trace", traceID, "panic", r)
			m.emit(ctx, event.New(event.RunAborted, traceID, sess.ID, "", map[string]any{"reason": "panic"}))
		}
	}()

	res, err := a.run1(ctx, sess, traceID, msgs, runner)
	switch {
	case a.wasAborted() || errors.Is(ctx.Err(), context.Canceled) || res.Stop == agent.StopAborted:
		// An aborted turn delivers nothing. When it was the shutdown — not the
		// user — that cut it short, its input goes back to the mailbox so the
		// restart replays it.
		if !a.wasAborted() && m.ctx.Err() != nil {
			a.repend(ctx, sess.ID, msgs)
		}
	case errors.Is(err, ErrIdentityDisabled):
		// Its input came in before the sender was disabled: answered with
		// nothing, as if the turn had started and been stopped.
		m.log.Info("session: dropped the turn of a disabled identity", "session", sess.ID, "trace", traceID)
		m.emit(ctx, event.New(event.RunAborted, traceID, sess.ID, "", map[string]any{"reason": "identity_disabled"}))
	case err != nil:
		m.log.Error("session: turn failed", "session", sess.ID, "trace", traceID, "error", err)
		a.apologize(ctx, sess, traceID)
	case res.Reply.ID != "" && sess.Origin.ConversationKind == event.ConversationRoom && agent.Passed(res.Reply):
		m.log.Debug("session: passed on room chatter", "session", sess.ID, "trace", traceID)
	case res.Reply.ID != "" && agent.Silent(res.Reply):
		m.log.Debug("session: the turn only reacted", "session", sess.ID, "trace", traceID)
	case res.Reply.ID != "":
		a.deliver(ctx, sess, traceID, res.Reply)
	}
	return true
}

// beginTurn claims the session, healing once if a crash left the row claimed.
func (m *Manager) beginTurn(ctx context.Context, id event.ID) (string, error) {
	token, _, err := m.d.Store.BeginTurn(ctx, id)
	if !errors.Is(err, store.ErrTurnActive) {
		return token, err
	}
	m.log.Warn("session: turn already active, healing", "session", id)
	if _, herr := m.d.Store.HealTurns(ctx); herr != nil {
		return "", fmt.Errorf("heal: %w", herr)
	}
	token, _, err = m.d.Store.BeginTurn(ctx, id)
	return token, err
}

// run1 builds the turn input and runs the loop.
func (a *actor) run1(ctx context.Context, sess store.Session, traceID event.ID, msgs []event.Message, runner Turner) (agent.TurnResult, error) {
	m := a.m
	var in agent.TurnInput
	var err error
	if m.d.Prepare != nil {
		if in, err = m.d.Prepare(ctx, sess, msgs); err != nil {
			return agent.TurnResult{}, fmt.Errorf("prepare: %w", err)
		}
	}
	// After un-pending, so the new user messages are part of the history, and
	// after Prepare, which may persist derived media text on them.
	hist, err := m.d.Store.ListMessages(ctx, sess.ID, sess.SummaryMsgID, 0)
	if err != nil {
		return agent.TurnResult{}, fmt.Errorf("history: %w", err)
	}
	in.SessionID, in.TraceID, in.History = sess.ID, traceID, hist
	in.Call.SessionID, in.Call.TraceID = sess.ID, traceID
	return runner.Turn(ctx, in)
}

func (a *actor) wasAborted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.aborted
}

// apologize stores and delivers a short fixed reply. model.error is already
// emitted by the loop, so nothing is emitted here.
func (a *actor) apologize(ctx context.Context, sess store.Session, traceID event.ID) {
	m := a.m
	ctx = context.WithoutCancel(ctx)
	msg := event.Message{
		Schema: event.SchemaVersion, ID: event.NewID(), SessionID: sess.ID, TraceID: traceID,
		Role: event.RoleAssistant, Text: apologyText, CreatedAt: m.d.Now(),
	}
	if _, err := m.d.Store.AppendMessage(ctx, msg, false); err != nil {
		m.log.Error("session: store apology", "session", sess.ID, "error", err)
	}
	a.deliver(ctx, sess, traceID, msg)
}

// deliver hands the reply to the channel the conversation lives on. A
// delivery failure is logged and evented, never fatal to the actor.
func (a *actor) deliver(ctx context.Context, sess store.Session, traceID event.ID, reply event.Message) {
	m := a.m
	if m.d.Deliver == nil {
		return
	}
	if err := m.d.Deliver(context.WithoutCancel(ctx), sess, traceID, reply); err != nil {
		m.log.Error("session: delivery failed", "session", sess.ID, "trace", traceID, "error", err)
		ev := event.New(event.DeliveryDegraded, traceID, sess.ID, "", map[string]any{
			"reason":  err.Error(),
			"channel": sess.Origin.Channel,
		})
		ev.MessageID = reply.ID
		m.emit(ctx, ev)
	}
}

// repender is implemented by stores that can put consumed input back in the
// mailbox (store.SQLite does).
type repender interface {
	Repend(ctx context.Context, ids []event.ID) error
}

// repend puts a turn's input back in the mailbox when the store supports it.
func (a *actor) repend(ctx context.Context, sessionID event.ID, msgs []event.Message) {
	rp, ok := a.m.d.Store.(repender)
	if !ok {
		return
	}
	ids := make([]event.ID, 0, len(msgs))
	for _, msg := range msgs {
		ids = append(ids, msg.ID)
	}
	if err := rp.Repend(context.WithoutCancel(ctx), ids); err != nil {
		a.m.log.Error("session: requeue input", "session", sessionID, "error", err)
	}
}
