package approval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.DB()
}

type recorder struct {
	mu  sync.Mutex
	evs []event.Event
}

func (r *recorder) Emit(_ context.Context, e event.Event) {
	r.mu.Lock()
	r.evs = append(r.evs, e)
	r.mu.Unlock()
}

func (r *recorder) all() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.evs...)
}

func (r *recorder) types() []event.Type {
	var out []event.Type
	for _, e := range r.all() {
		out = append(out, e.Type)
	}
	return out
}

func (r *recorder) count(t event.Type) int {
	n := 0
	for _, e := range r.all() {
		if e.Type == t {
			n++
		}
	}
	return n
}

func newService(t *testing.T, opts ...Option) (*Service, *recorder) {
	t.Helper()
	rec := &recorder{}
	return New(newDB(t), rec, opts...), rec
}

// resolveWhenPending resolves id as soon as the row exists, mimicking a human
// answering the prompt that was just raised.
func resolveWhenPending(t *testing.T, s *Service, id event.ID, approve bool, choice tool.GrantScope) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := s.Resolve(context.Background(), id, approve, choice, "user1"); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

// --- grants ------------------------------------------------------------

func TestGrantsCRUD(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()

	g, err := s.AddGrant(ctx, tool.Grant{Agent: "a", Tool: "sh", Scope: tool.GrantAlways})
	if err != nil {
		t.Fatal(err)
	}
	if g.ID == "" || g.CreatedAt.IsZero() {
		t.Fatalf("grant not filled in: %+v", g)
	}
	if _, err := s.AddGrant(ctx, tool.Grant{Agent: "b", Tool: "sh", Scope: tool.GrantAlways}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListGrants(ctx, "a")
	if err != nil || len(list) != 1 || list[0].ID != g.ID {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if err := s.RevokeGrant(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListGrants(ctx, "a"); len(list) != 0 {
		t.Fatalf("grant survived revoke: %+v", list)
	}
	// Revoking twice is not an error.
	if err := s.RevokeGrant(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAddGrantRejectsBadInput(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	for _, g := range []tool.Grant{
		{Tool: "sh", Scope: tool.GrantAlways},
		{Agent: "a", Scope: tool.GrantAlways},
		{Agent: "a", Tool: "sh", Scope: tool.GrantOnce},
		{Agent: "a", Tool: "sh", Scope: ""},
	} {
		if _, err := s.AddGrant(ctx, g); err == nil {
			t.Fatalf("accepted %+v", g)
		}
	}
}

func TestMatchGrantScopes(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	mustGrant := func(g tool.Grant) {
		t.Helper()
		if _, err := s.AddGrant(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	mustGrant(tool.Grant{Agent: "a", Tool: "always", Scope: tool.GrantAlways})
	mustGrant(tool.Grant{Agent: "a", Tool: "sess", Scope: tool.GrantSession, SessionID: "s1"})
	mustGrant(tool.Grant{Agent: "a", Tool: "task", Scope: tool.GrantTask, TaskID: "t1"})
	mustGrant(tool.Grant{Agent: "a", Tool: "sh", Scope: tool.GrantAlways, Pattern: "git status"})

	cases := []struct {
		name                            string
		agent, tn, session, task, patrn string
		want                            bool
	}{
		{"always any session", "a", "always", "sX", "", "", true},
		{"wrong agent", "b", "always", "sX", "", "", false},
		{"wrong tool", "a", "other", "sX", "", "", false},
		{"session match", "a", "sess", "s1", "", "", true},
		{"session mismatch", "a", "sess", "s2", "", "", false},
		{"session empty", "a", "sess", "", "", "", false},
		{"task match", "a", "task", "sX", "t1", "", true},
		{"task mismatch", "a", "task", "sX", "t2", "", false},
		{"task empty", "a", "task", "sX", "", "", false},
		{"pattern prefix", "a", "sh", "sX", "", "git status -s", true},
		{"pattern exact", "a", "sh", "sX", "", "git status", true},
		{"pattern word boundary", "a", "sh", "sX", "", "git statusx", false},
		{"pattern other command", "a", "sh", "sX", "", "rm -rf /", false},
		{"pattern metacharacter", "a", "sh", "sX", "", "git status; rm -rf /", false},
		{"pattern empty call", "a", "sh", "sX", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := s.MatchGrant(ctx, tc.agent, tc.tn, tc.session, tc.task, tc.patrn)
			if ok != tc.want {
				t.Fatalf("MatchGrant = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestMatchGrantFailsClosedOnDBError(t *testing.T) {
	s, _ := newService(t)
	if _, err := s.AddGrant(context.Background(), tool.Grant{Agent: "a", Tool: "sh", Scope: tool.GrantAlways}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := s.MatchGrant(ctx, "a", "sh", "s1", "", ""); ok {
		t.Fatal("a failed lookup must report no match")
	}
}

// --- approvals ---------------------------------------------------------

func TestRequestApproved(t *testing.T) {
	s, rec := newService(t)
	id := event.NewID()
	resolveWhenPending(t, s, id, true, tool.GrantOnce)

	got, err := s.Request(context.Background(), tool.Approval{
		ID: id, Agent: "a", Tool: "sh",
		Summary: "sh: ls", Args: `{"command":"ls"}`, SessionID: "s1", TraceID: "tr1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tool.ApprovalApproved || got.Choice != tool.GrantOnce {
		t.Fatalf("got %+v", got)
	}
	if got.ResolvedBy != "user1" || got.ResolvedAt == nil {
		t.Fatalf("resolution not recorded: %+v", got)
	}
	if got.ExpiresAt.Sub(got.CreatedAt) != DefaultTimeout {
		t.Fatalf("expiry = %v", got.ExpiresAt.Sub(got.CreatedAt))
	}
	// A "once" choice persists nothing.
	if gs, _ := s.ListGrants(context.Background(), "a"); len(gs) != 0 {
		t.Fatalf("once created a grant: %+v", gs)
	}
	if rec.count(event.ApprovalRequested) != 1 || rec.count(event.ApprovalResolved) != 1 {
		t.Fatalf("events = %v", rec.types())
	}
}

func TestRequestDenied(t *testing.T) {
	s, _ := newService(t)
	id := event.NewID()
	resolveWhenPending(t, s, id, false, tool.GrantAlways)
	got, err := s.Request(context.Background(), tool.Approval{ID: id, Agent: "a", Tool: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tool.ApprovalDenied {
		t.Fatalf("status = %q", got.Status)
	}
	if got.Choice != "" {
		t.Fatalf("a denial must not carry a choice: %q", got.Choice)
	}
	if gs, _ := s.ListGrants(context.Background(), "a"); len(gs) != 0 {
		t.Fatalf("denial created a grant: %+v", gs)
	}
}

func TestRequestTimeoutDenies(t *testing.T) {
	s, rec := newService(t, WithTimeout(40*time.Millisecond))
	got, err := s.Request(context.Background(), tool.Approval{Agent: "a", Tool: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tool.ApprovalExpired {
		t.Fatalf("status = %q, want expired", got.Status)
	}
	stored, err := s.Get(context.Background(), got.ID)
	if err != nil || stored.Status != tool.ApprovalExpired {
		t.Fatalf("stored = %+v err=%v", stored, err)
	}
	if rec.count(event.ApprovalResolved) != 1 {
		t.Fatalf("events = %v", rec.types())
	}
	// A timed-out approval is no longer pending, so it cannot be resolved.
	if _, err := s.Resolve(context.Background(), got.ID, true, tool.GrantOnce, "u"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestContextCancelled(t *testing.T) {
	s, _ := newService(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	got, err := s.Request(ctx, tool.Approval{Agent: "a", Tool: "sh"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	stored, err := s.Get(context.Background(), got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != tool.ApprovalDenied || stored.ResolvedBy != "system:cancelled" {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestDoubleResolve(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	a, err := s.insert(ctx, tool.Approval{Agent: "a", Tool: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, a.ID, true, tool.GrantOnce, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, a.ID, false, "", "u2"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("err = %v, want ErrNotPending", err)
	}
	stored, _ := s.Get(ctx, a.ID)
	if stored.Status != tool.ApprovalApproved || stored.ResolvedBy != "u1" {
		t.Fatalf("the first decision must stand: %+v", stored)
	}
	if _, err := s.Resolve(ctx, "nosuch", true, "", "u"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, "nosuch"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestResolvePersistsGrants(t *testing.T) {
	s, _ := newService(t, WithGrantPattern(func(a tool.Approval) string {
		var v struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal([]byte(a.Args), &v)
		return v.Command
	}))
	ctx := context.Background()

	sess, _ := s.insert(ctx, tool.Approval{Agent: "a", Tool: "sh", SessionID: "s1", Args: `{"command":"ls -l"}`})
	if _, err := s.Resolve(ctx, sess.ID, true, tool.GrantSession, "u1"); err != nil {
		t.Fatal(err)
	}
	always, _ := s.insert(ctx, tool.Approval{Agent: "a", Tool: "sh", SessionID: "s1", Args: `{"command":"ls -l"}`})
	if _, err := s.Resolve(ctx, always.ID, true, tool.GrantAlways, "u1"); err != nil {
		t.Fatal(err)
	}
	gs, err := s.ListGrants(ctx, "a")
	if err != nil || len(gs) != 2 {
		t.Fatalf("grants = %+v err=%v", gs, err)
	}
	byScope := map[tool.GrantScope]tool.Grant{}
	for _, g := range gs {
		byScope[g.Scope] = g
	}
	if g := byScope[tool.GrantSession]; g.SessionID != "s1" || g.Pattern != "ls -l" || g.CreatedBy != "u1" {
		t.Fatalf("session grant = %+v", g)
	}
	if g := byScope[tool.GrantAlways]; g.SessionID != "" || g.Pattern != "ls -l" {
		t.Fatalf("always grant = %+v", g)
	}
}

// A once-only approval refuses the two answers that would keep a grant, stays
// pending for the one it takes, and says so in its request event.
func TestResolveOnceOnly(t *testing.T) {
	s, rec := newService(t)
	ctx := context.Background()
	a, err := s.insert(ctx, tool.Approval{Agent: "a", Tool: "browser.do", SessionID: "s1", OnceOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var attrs requestedAttrs
	if err := json.Unmarshal(rec.all()[0].Attrs, &attrs); err != nil || !attrs.OnceOnly {
		t.Fatalf("request event attrs = %s, err = %v", rec.all()[0].Attrs, err)
	}
	for _, choice := range []tool.GrantScope{tool.GrantSession, tool.GrantAlways} {
		if _, err := s.Resolve(ctx, a.ID, true, choice, "u1"); !errors.Is(err, ErrOnceOnly) {
			t.Fatalf("%s: err = %v, want ErrOnceOnly", choice, err)
		}
	}
	stored, err := s.Get(ctx, a.ID)
	if err != nil || stored.Status != tool.ApprovalPending || !stored.OnceOnly || stored.ResolvedBy != "" {
		t.Fatalf("a refused answer changed the approval: %+v err=%v", stored, err)
	}
	if n := rec.count(event.ApprovalResolved); n != 0 {
		t.Fatalf("%d resolved events for refused answers", n)
	}
	got, err := s.Resolve(ctx, a.ID, true, tool.GrantOnce, "u1")
	if err != nil || got.Status != tool.ApprovalApproved || got.Choice != tool.GrantOnce {
		t.Fatalf("once: %+v err=%v", got, err)
	}
	if gs, err := s.ListGrants(ctx, "a"); err != nil || len(gs) != 0 {
		t.Fatalf("grants = %+v err=%v", gs, err)
	}
	// Settled wins over once-only: a late "always" is a second answer.
	if _, err := s.Resolve(ctx, a.ID, true, tool.GrantAlways, "u1"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("err = %v, want ErrNotPending", err)
	}

	// A deny carries no choice, whatever was sent with it.
	b, _ := s.insert(ctx, tool.Approval{Agent: "a", Tool: "browser.do", OnceOnly: true})
	if got, err := s.Resolve(ctx, b.ID, false, tool.GrantAlways, "u1"); err != nil || got.Status != tool.ApprovalDenied {
		t.Fatalf("deny: %+v err=%v", got, err)
	}
	// An ordinary approval is not once-only, in the row or the event.
	c, _ := s.insert(ctx, tool.Approval{Agent: "a", Tool: "sh"})
	if stored, _ := s.Get(ctx, c.ID); stored.OnceOnly {
		t.Fatal("an ordinary approval came back once-only")
	}
	evs := rec.all()
	if strings.Contains(string(evs[len(evs)-1].Attrs), "once_only") {
		t.Fatalf("attrs = %s", evs[len(evs)-1].Attrs)
	}
}

func TestConcurrentApprovalsAreIndependent(t *testing.T) {
	s, _ := newService(t, WithTimeout(5*time.Second))
	ctx := context.Background()

	id1, id2 := event.NewID(), event.NewID()
	done1 := make(chan tool.Approval, 1)
	done2 := make(chan tool.Approval, 1)
	go func() {
		a, _ := s.Request(ctx, tool.Approval{ID: id1, Agent: "a", Tool: "sh", SessionID: "s1"})
		done1 <- a
	}()
	go func() {
		a, _ := s.Request(ctx, tool.Approval{ID: id2, Agent: "a", Tool: "sh", SessionID: "s2"})
		done2 <- a
	}()

	// Both rows must become pending before either is answered.
	waitPending(t, s, 2)

	if _, err := s.Resolve(ctx, id1, true, tool.GrantOnce, "u1"); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-done1:
		if a.Status != tool.ApprovalApproved {
			t.Fatalf("status = %q", a.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolving session 1 did not wake it")
	}
	select {
	case a := <-done2:
		t.Fatalf("session 2 must stay suspended, got %+v", a)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := s.Resolve(ctx, id2, false, "", "u1"); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-done2:
		if a.Status != tool.ApprovalDenied {
			t.Fatalf("status = %q", a.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session 2 never woke")
	}
}

func waitPending(t *testing.T, s *Service, n int) []tool.Approval {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := s.ListPending(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(p) >= n {
			return p
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only saw fewer than %d pending approvals", n)
	return nil
}

func TestListPending(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	a, _ := s.insert(ctx, tool.Approval{Agent: "a", Tool: "sh"})
	b, _ := s.insert(ctx, tool.Approval{Agent: "a", Tool: "sh"})
	p, err := s.ListPending(ctx)
	if err != nil || len(p) != 2 {
		t.Fatalf("pending = %d err=%v", len(p), err)
	}
	if _, err := s.Resolve(ctx, a.ID, true, tool.GrantOnce, "u"); err != nil {
		t.Fatal(err)
	}
	p, _ = s.ListPending(ctx)
	if len(p) != 1 || p[0].ID != b.ID {
		t.Fatalf("pending = %+v", p)
	}
}

func TestExpireStale(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	toolReq, _ := s.insert(ctx, tool.Approval{Agent: "a", Tool: "sh"})
	done, _ := s.insert(ctx, tool.Approval{Agent: "a", Tool: "sh"})
	if _, err := s.Resolve(ctx, done.ID, true, tool.GrantOnce, "u1"); err != nil {
		t.Fatal(err)
	}

	n, err := s.ExpireStale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expired %d rows, want 1", n)
	}
	if got, _ := s.Get(ctx, toolReq.ID); got.Status != tool.ApprovalExpired || got.ResolvedBy != "system:restart" {
		t.Fatalf("tool approval survived a restart: %+v", got)
	}
	if got, _ := s.Get(ctx, done.ID); got.Status != tool.ApprovalApproved {
		t.Fatalf("a resolved approval changed: %q", got.Status)
	}
}

// Every approval is a tool call: the tool timeout, approval.requested, and the
// row stored as kind tool.
func TestInsertPendsAToolCall(t *testing.T) {
	s, rec := newService(t)
	ctx := context.Background()
	a, err := s.insert(ctx, tool.Approval{Agent: "a", Tool: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != tool.ApprovalPending {
		t.Fatalf("approval = %+v", a)
	}
	if a.ExpiresAt.Sub(a.CreatedAt) != DefaultTimeout {
		t.Fatalf("expiry = %v", a.ExpiresAt.Sub(a.CreatedAt))
	}
	if rec.count(event.ApprovalRequested) != 1 {
		t.Fatalf("events = %v", rec.types())
	}
	stored, err := s.Get(ctx, a.ID)
	if err != nil || stored.Tool != "sh" {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
}

func TestRequestEventCapsArgs(t *testing.T) {
	s, rec := newService(t)
	long := strings.Repeat("x", 2000)
	a, err := s.insert(context.Background(), tool.Approval{Agent: "a", Tool: "sh", Args: long})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Args) > tool.MaxApprovalArgs+4 {
		t.Fatalf("stored args are %d bytes", len(a.Args))
	}
	evs := rec.all()
	if len(evs) != 1 || evs[0].Type != event.ApprovalRequested {
		t.Fatalf("events = %v", rec.types())
	}
	var attrs requestedAttrs
	if err := json.Unmarshal(evs[0].Attrs, &attrs); err != nil {
		t.Fatal(err)
	}
	if len(attrs.Args) > tool.MaxApprovalArgs+4 {
		t.Fatalf("event carried %d bytes of arguments", len(attrs.Args))
	}
	if attrs.ID != a.ID || attrs.Tool != "sh" {
		t.Fatalf("attrs = %+v", attrs)
	}
	stored, _ := s.Get(context.Background(), a.ID)
	if len(stored.Args) > tool.MaxApprovalArgs+4 {
		t.Fatalf("db kept %d bytes", len(stored.Args))
	}
}

func TestInjectedClock(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, _ := newService(t, WithNow(func() time.Time { return fixed }))
	a, err := s.insert(context.Background(), tool.Approval{Agent: "a", Tool: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.CreatedAt.Equal(fixed) || !a.ExpiresAt.Equal(fixed.Add(DefaultTimeout)) {
		t.Fatalf("clock not injected: %+v", a)
	}
	g, err := s.AddGrant(context.Background(), tool.Grant{Agent: "a", Tool: "sh", Scope: tool.GrantAlways})
	if err != nil {
		t.Fatal(err)
	}
	if !g.CreatedAt.Equal(fixed) {
		t.Fatalf("grant clock = %v", g.CreatedAt)
	}
}
