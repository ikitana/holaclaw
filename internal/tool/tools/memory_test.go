package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/memory"
	memsqlite "holaclaw/internal/memory/sqlite"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// memTools wires the three memory tools over a real SQLite backend and
// collects the events they emit.
type memTools struct {
	exec   tool.Executor
	mem    *memsqlite.Store
	events []event.Event
}

func newMemTools(t *testing.T, opts memsqlite.Options) *memTools {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := &memTools{mem: memsqlite.New(st.DB(), opts)}
	r := tool.NewRegistry()
	sink := event.SinkFunc(func(_ context.Context, e event.Event) { m.events = append(m.events, e) })
	if err := RegisterMemory(r, m.mem, sink); err != nil {
		t.Fatalf("RegisterMemory: %v", err)
	}
	ex, err := r.Select([]string{"memory"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	m.exec = ex
	return m
}

// call runs one tool with raw JSON arguments.
func (m *memTools) call(t *testing.T, c tool.Call, name, args string) tool.Result {
	t.Helper()
	c.Name, c.Args = name, json.RawMessage(args)
	res, err := m.exec.Execute(context.Background(), c)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func dmCall(role string) tool.Call {
	return tool.Call{
		Agent: "main", Partition: "dm/u1", Role: role,
		UserID: event.ID("U1"), SessionID: event.ID("S1"), TraceID: event.ID("T1"),
		Origin: event.Origin{Channel: "tg", ConversationKind: event.ConversationDM},
	}
}

func groupCall(role string) tool.Call {
	c := dmCall(role)
	c.Partition = "group/g1"
	c.Origin.ConversationKind = event.ConversationRoom
	return c
}

// TestMemorySchemaSizes keeps the memory tools inside the docs/context.md budget and logs
// what they cost.
func TestMemorySchemaSizes(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{})
	total := 0
	specs := m.exec.Specs()
	if len(specs) != 3 {
		t.Fatalf("registered %d tools, want 3", len(specs))
	}
	for _, s := range specs {
		n := tool.SchemaTokens(s)
		total += n
		t.Logf("%-14s %3d tokens", s.Name, n)
		if n > tool.MaxSchemaTokens {
			t.Errorf("%s: %d tokens, cap is %d", s.Name, n, tool.MaxSchemaTokens)
		}
	}
	t.Logf("%-14s %3d tokens", "TOTAL", total)
}

func TestMemoryToolsRisk(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{})
	lk, ok := m.exec.(tool.Lookuper)
	if !ok {
		t.Fatal("executor is not a Lookuper")
	}
	want := map[string]tool.Risk{
		"memory.save": tool.RiskWrite, "memory.search": tool.RiskRead, "memory.forget": tool.RiskWrite,
	}
	for name, risk := range want {
		tl, ok := lk.Lookup(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if tl.Risk() != risk {
			t.Errorf("%s risk = %s, want %s", name, tl.Risk(), risk)
		}
	}
}

// TestProvenanceCannotBeForged is the point of the whole design: what the
// model puts in the arguments is ignored, the columns come from the Call.
func TestProvenanceCannotBeForged(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{})
	c := dmCall("user")
	res := m.call(t, c, "memory.save", `{
		"text":"the deploy key is in vault",
		"agent":"other","partition":"dm/victim","user_id":"UHACK","role":"owner",
		"session_id":"SHACK","trace_id":"THACK","created_at":"1999-01-01",
		"layer":"recall","visibility":"shared","id":"forged"}`)
	if res.IsError {
		t.Fatalf("save failed: %s", res.Text)
	}
	hits, err := m.mem.Search(context.Background(), memory.Query{Agent: "main", Partition: "dm/u1", Text: "deploy key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("stored %d items", len(hits))
	}
	it := hits[0].Item
	if it.Prov.Agent != "main" || it.Prov.Partition != "dm/u1" || it.Prov.UserID != event.ID("U1") ||
		it.Prov.Role != "user" || it.Prov.SessionID != event.ID("S1") || it.Prov.TraceID != event.ID("T1") {
		t.Fatalf("provenance taken from the model: %+v", it.Prov)
	}
	if it.Layer != memory.LayerNote {
		t.Fatalf("layer = %s, want note", it.Layer)
	}
	// "visibility":"shared" from a plain user is downgraded, not honoured.
	if it.Visibility != memory.VisOrigin {
		t.Fatalf("visibility = %s, want origin", it.Visibility)
	}
	// Nothing landed in the partition the model named.
	other, err := m.mem.Search(context.Background(), memory.Query{Agent: "main", Partition: "dm/victim", Text: "deploy key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("item reachable from dm/victim: %d", len(other))
	}
}

func TestVisibilityDefaultsPerRole(t *testing.T) {
	cases := []struct {
		name      string
		call      tool.Call
		requested string
		want      memory.Visibility
	}{
		{"owner dm defaults to shared", dmCall("owner"), "", memory.VisShared},
		{"admin dm defaults to shared", dmCall("admin"), "", memory.VisShared},
		{"user dm defaults to origin", dmCall("user"), "", memory.VisOrigin},
		{"owner group defaults to origin", groupCall("owner"), "", memory.VisOrigin},
		{"owner group may share explicitly", groupCall("owner"), "shared", memory.VisShared},
		{"user cannot share", dmCall("user"), "shared", memory.VisOrigin},
		{"owner may keep it local", dmCall("owner"), "origin", memory.VisOrigin},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMemTools(t, memsqlite.Options{})
			args := `{"text":"one remembered fact"}`
			if c.requested != "" {
				args = `{"text":"one remembered fact","visibility":"` + c.requested + `"}`
			}
			if res := m.call(t, c.call, "memory.save", args); res.IsError {
				t.Fatalf("save: %s", res.Text)
			}
			hits, err := m.mem.Search(context.Background(), memory.Query{
				Agent: c.call.Agent, Partition: c.call.Partition, Text: "remembered fact"})
			if err != nil || len(hits) != 1 {
				t.Fatalf("search: %v %d hits", err, len(hits))
			}
			if got := hits[0].Item.Visibility; got != c.want {
				t.Fatalf("visibility = %s, want %s", got, c.want)
			}
		})
	}
}

func TestSaveRejectsUnknownVisibilityAndEmptyText(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{})
	c := dmCall("owner")
	if res := m.call(t, c, "memory.save", `{"text":"x","visibility":"public"}`); !res.IsError {
		t.Fatal("unknown visibility accepted")
	}
	if res := m.call(t, c, "memory.save", `{"text":"   "}`); !res.IsError {
		t.Fatal("empty text accepted")
	}
	if res := m.call(t, c, "memory.save", `{"text":`); !res.IsError {
		t.Fatal("broken JSON accepted")
	}
}

func TestGuestsNeverWriteNotes(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{})
	c := dmCall("guest")
	res := m.call(t, c, "memory.save", `{"text":"guest fact"}`)
	if !res.IsError || !strings.Contains(res.Text, "guest") {
		t.Fatalf("guest save = %+v", res)
	}
	if res := m.call(t, c, "memory.forget", `{"id":"01J"}`); !res.IsError {
		t.Fatalf("guest forget = %+v", res)
	}
	if len(m.events) != 0 {
		t.Fatalf("guest write emitted %d events", len(m.events))
	}
	// Reading is allowed for guests.
	if res := m.call(t, c, "memory.search", `{"query":"anything"}`); res.IsError {
		t.Fatalf("guest search = %+v", res)
	}
}

// A task run has no memory: an owner's task, whose synthetic origin is a DM,
// would otherwise save a shared note.
func TestTaskRunsHaveNoMemory(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{})
	if res := m.call(t, dmCall("owner"), "memory.save", `{"text":"one remembered fact"}`); res.IsError {
		t.Fatalf("seed: %s", res.Text)
	}
	m.events = nil
	c := dmCall("owner")
	c.RunKind = tool.RunTask
	for name, args := range map[string]string{
		"memory.save":   `{"text":"task fact"}`,
		"memory.search": `{"query":"remembered fact"}`,
		"memory.forget": `{"id":"01J"}`,
	} {
		if res := m.call(t, c, name, args); !res.IsError || res.Text != noTaskMemory {
			t.Errorf("%s in a task = %+v", name, res)
		}
	}
	if len(m.events) != 0 {
		t.Fatalf("task run emitted %d events", len(m.events))
	}
	hits, err := m.mem.Search(context.Background(), memory.Query{Agent: "main", Partition: "dm/u1", Text: "fact"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("want only the seeded note: %v %d hits", err, len(hits))
	}
}

func TestBudgetMessageTellsTheModelToConsolidate(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{NoteBudgetTokens: 12})
	c := dmCall("owner")
	if res := m.call(t, c, "memory.save", `{"text":"alpha beta gamma delta epsilon zeta"}`); res.IsError {
		t.Fatalf("first save: %s", res.Text)
	}
	res := m.call(t, c, "memory.save", `{"text":"eta theta iota kappa lambda mu"}`)
	if !res.IsError {
		t.Fatal("budget not enforced")
	}
	if res.Text != budgetHint {
		t.Fatalf("budget message = %q, want %q", res.Text, budgetHint)
	}
}

func TestSaveEmitsMemorySaved(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{})
	c := dmCall("owner")
	if res := m.call(t, c, "memory.save", `{"text":"angel lives in girona"}`); res.IsError {
		t.Fatalf("save: %s", res.Text)
	}
	if len(m.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(m.events))
	}
	e := m.events[0]
	if e.Type != event.MemorySaved || e.TraceID != event.ID("T1") || e.SessionID != event.ID("S1") {
		t.Fatalf("event = %+v", e)
	}
	var attrs map[string]string
	if err := json.Unmarshal(e.Attrs, &attrs); err != nil {
		t.Fatal(err)
	}
	if attrs["memory_id"] == "" || attrs["visibility"] != string(memory.VisShared) || attrs["layer"] != "note" {
		t.Fatalf("attrs = %v", attrs)
	}
}

var memoryLineRE = regexp.MustCompile(`^\[[0-9A-Za-z]+\] \(\d{4}-\d{2}-\d{2}, (shared|origin)\) .+$`)

func TestSearchOutputFormat(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{InjectionCap: 5})
	c := dmCall("owner")
	for _, text := range []string{
		"angel lives in girona",
		"the deploy key is kept in vault",
		"holaclaw ships on fridays",
	} {
		if res := m.call(t, c, "memory.save", `{"text":"`+text+`"}`); res.IsError {
			t.Fatalf("save: %s", res.Text)
		}
	}
	res := m.call(t, c, "memory.search", `{"query":"where does angel live?"}`)
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	lines := strings.Split(res.Text, "\n")
	if lines[0] != recallBanner {
		t.Fatalf("first line = %q, want the do-not-save banner", lines[0])
	}
	if len(lines) < 2 {
		t.Fatalf("no results: %q", res.Text)
	}
	for _, l := range lines[1:] {
		if !memoryLineRE.MatchString(l) {
			t.Fatalf("line %q does not match the compact format", l)
		}
	}
	if res.Tainted {
		t.Fatal("memory results must not be marked tainted")
	}
	// Empty result set says so without being an error.
	empty := m.call(t, c, "memory.search", `{"query":"submarines"}`)
	if empty.IsError || !strings.Contains(empty.Text, "no memories") {
		t.Fatalf("empty search = %+v", empty)
	}
}

func TestSearchCapsLongResults(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{InjectionCap: 5, NoteBudgetTokens: 100000})
	c := dmCall("owner")
	long := strings.Repeat("deployment ", 120)
	for i := 0; i < 5; i++ {
		if res := m.call(t, c, "memory.save", `{"text":"`+long+`"}`); res.IsError {
			t.Fatalf("save: %s", res.Text)
		}
	}
	res := m.call(t, c, "memory.search", `{"query":"deployment"}`)
	if len(res.Text) > maxMemoryResult+64 {
		t.Fatalf("result is %d bytes, cap is %d", len(res.Text), maxMemoryResult)
	}
}

func TestAboutIsPassedThrough(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{InjectionCap: 5})
	c := dmCall("owner")
	if res := m.call(t, c, "memory.save", `{"text":"prefers oat milk","about":"Bob"}`); res.IsError {
		t.Fatalf("save: %s", res.Text)
	}
	if res := m.call(t, c, "memory.save", `{"text":"prefers espresso","about":"Alice"}`); res.IsError {
		t.Fatalf("save: %s", res.Text)
	}
	res := m.call(t, c, "memory.search", `{"query":"prefers","about":"bob"}`)
	if !strings.Contains(res.Text, "oat milk") || strings.Contains(res.Text, "espresso") {
		t.Fatalf("about filter = %q", res.Text)
	}
}

func TestForgetIsPartitionChecked(t *testing.T) {
	m := newMemTools(t, memsqlite.Options{InjectionCap: 5})
	owner := dmCall("owner")
	if res := m.call(t, owner, "memory.save", `{"text":"forgettable fact","visibility":"origin"}`); res.IsError {
		t.Fatalf("save: %s", res.Text)
	}
	hits, err := m.mem.Search(context.Background(), memory.Query{Agent: "main", Partition: "dm/u1", Text: "forgettable"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("search: %v %d", err, len(hits))
	}
	id := string(hits[0].Item.ID)
	m.events = nil

	other := dmCall("owner")
	other.Partition = "dm/u2"
	if res := m.call(t, other, "memory.forget", `{"id":"`+id+`"}`); !res.IsError {
		t.Fatalf("another partition deleted the item: %+v", res)
	}
	if res := m.call(t, owner, "memory.forget", `{"id":"  "}`); !res.IsError {
		t.Fatal("empty id accepted")
	}
	if res := m.call(t, owner, "memory.forget", `{"id":"`+id+`"}`); res.IsError {
		t.Fatalf("forget: %s", res.Text)
	}
	if res := m.call(t, owner, "memory.forget", `{"id":"`+id+`"}`); !res.IsError {
		t.Fatal("second forget succeeded")
	}
	if len(m.events) != 1 {
		t.Fatalf("emitted %d events, want 1 (failed forgets emit none)", len(m.events))
	}
	e := m.events[0]
	if e.Type != event.MemoryForgotten || e.TraceID != event.ID("T1") || e.SessionID != event.ID("S1") {
		t.Fatalf("event = %+v", e)
	}
	var attrs map[string]string
	if err := json.Unmarshal(e.Attrs, &attrs); err != nil {
		t.Fatal(err)
	}
	if attrs["memory_id"] != id || attrs["agent"] != "main" || attrs["partition"] != "dm/u1" {
		t.Fatalf("attrs = %v", attrs)
	}
}

func TestRegisterMemoryValidatesWiring(t *testing.T) {
	r := tool.NewRegistry()
	if err := RegisterMemory(r, nil, nil); err == nil {
		t.Fatal("RegisterMemory accepted a nil store")
	}
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test cleanup
	mem := memsqlite.New(st.DB(), memsqlite.Options{})
	if err := RegisterMemory(r, mem, nil); err != nil { // a nil sink is allowed
		t.Fatalf("RegisterMemory: %v", err)
	}
	if err := RegisterMemory(r, mem, nil); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	// The memory tools are a separate call: Register must not know them.
	r2 := tool.NewRegistry()
	if err := Register(r2, Deps{}); err != nil {
		t.Fatal(err)
	}
	for _, n := range r2.Names() {
		if strings.HasPrefix(n, "memory.") {
			t.Fatalf("Register added %s", n)
		}
	}
}
