package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/memory"
)

// Listing partitions is optional for a backend, so these cover both halves of
// that: what a store that can enumerate them answers, and what the API says
// when the one behind it cannot.

// partitionMemory is a recordingMemory that can also enumerate partitions.
type partitionMemory struct {
	recordingMemory
	parts []memory.Partition
	asked string
}

func (m *partitionMemory) Partitions(_ context.Context, agent string) ([]memory.Partition, error) {
	m.asked = agent
	return m.parts, nil
}

func TestListMemoryPartitionsNamesWhatTheAgentHolds(t *testing.T) {
	h := newHarness(t)
	written := time.Date(2026, 9, 20, 8, 30, 0, 0, time.UTC)
	mem := &partitionMemory{parts: []memory.Partition{
		{Name: "dm/U1", Notes: 2, Recall: 7, LastWritten: written},
		{Name: "task/S9", Recall: 1},
	}}
	h.b.Memory = mem

	out := decode[struct {
		Partitions []gen.MemoryPartition `json:"partitions"`
	}](t, h.do(http.MethodGet, "/memory/partitions?agent=main", h.adminToken, nil), http.StatusOK)

	if mem.asked != "main" {
		t.Fatalf("the backend was asked for agent %q, want main", mem.asked)
	}
	if len(out.Partitions) != 2 {
		t.Fatalf("got %d partitions, want 2: %+v", len(out.Partitions), out.Partitions)
	}
	first := out.Partitions[0]
	if first.Partition != "dm/U1" || first.Notes != 2 || first.Recall != 7 {
		t.Errorf("first = %+v, want dm/U1 with 2 notes and 7 recall", first)
	}
	if first.LastWritten == nil || !first.LastWritten.Equal(written) {
		t.Errorf("last written = %v, want %v", first.LastWritten, written)
	}
	// A partition nothing has been written into leaves the field out rather
	// than reporting the zero time as a date.
	if out.Partitions[1].LastWritten != nil {
		t.Errorf("an unwritten partition carried a time: %v", out.Partitions[1].LastWritten)
	}
}

func TestListMemoryPartitionsNeedsAnAgent(t *testing.T) {
	h := newHarness(t)
	h.b.Memory = &partitionMemory{}
	errorBody(t, h.do(http.MethodGet, "/memory/partitions?agent=", h.adminToken, nil),
		http.StatusBadRequest, CodeBadRequest)
}

func TestListMemoryPartitionsIsNotImplementedWithoutALister(t *testing.T) {
	h := newHarness(t)
	// A complete memory store that simply cannot enumerate: the answer says
	// so instead of pretending the agent has no partitions at all.
	h.b.Memory = &recordingMemory{}
	errorBody(t, h.do(http.MethodGet, "/memory/partitions?agent=main", h.adminToken, nil),
		http.StatusNotImplemented, CodeNotImplemented)

	h.b.Memory = nil
	errorBody(t, h.do(http.MethodGet, "/memory/partitions?agent=main", h.adminToken, nil),
		http.StatusNotImplemented, CodeNotImplemented)
}

func TestListMemoryPartitionsIsAdminOnly(t *testing.T) {
	h := newHarness(t)
	h.b.Memory = &partitionMemory{}
	errorBody(t, h.do(http.MethodGet, "/memory/partitions?agent=main", h.ownerToken, nil),
		http.StatusForbidden, CodeForbidden)
	errorBody(t, h.do(http.MethodGet, "/memory/partitions?agent=main", "nonsense", nil),
		http.StatusUnauthorized, CodeUnauthorized)
}

// TestClientListsPartitionsAndAgents drives the two client methods the memory
// browser opens with against a bare server, so the query it sends and the
// envelope it unwraps are both pinned.
func TestClientListsPartitionsAndAgents(t *testing.T) {
	t.Parallel()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case BaseURL + "/agents":
			_, _ = io.WriteString(w, `{"agents":[{"name":"main","tools":[],"models":{}}]}`)
		case BaseURL + "/memory/partitions":
			_, _ = io.WriteString(w, `{"partitions":[{"partition":"dm/U1","notes":2,"recall":7}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := Dial(srv.URL, "t0ken")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx := context.Background()

	agents, err := c.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "main" {
		t.Fatalf("agents = %+v", agents)
	}

	parts, err := c.ListMemoryPartitions(ctx, "main")
	if err != nil {
		t.Fatalf("list partitions: %v", err)
	}
	if len(parts) != 1 || parts[0].Partition != "dm/U1" || parts[0].Recall != 7 {
		t.Fatalf("partitions = %+v", parts)
	}
	want := BaseURL + "/memory/partitions?agent=main"
	if asked[1] != want {
		t.Fatalf("asked for %q, want %q", asked[1], want)
	}
}

// A refusal still reaches the caller as a ClientError, which is how the
// browser tells an older daemon apart from an empty agent.
func TestClientPartitionsReportsNotImplemented(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = io.WriteString(w, `{"code":"not_implemented","message":"memory partitions is not available in this build"}`)
	}))
	t.Cleanup(srv.Close)

	c, err := Dial(srv.URL, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, err = c.ListMemoryPartitions(context.Background(), "main")
	var ce *ClientError
	if !errors.As(err, &ce) || ce.Status != http.StatusNotImplemented || ce.Code != CodeNotImplemented {
		t.Fatalf("err = %v, want a not_implemented ClientError", err)
	}
}

// A dm/ or room/ partition carries the name of the identity behind it. One with
// no live identity (a stranger's id, a legacy dm/<channel>/<conversation>, an
// isolated room/<channel>/<conversation>, a task) has none.
func TestListMemoryPartitionsNamesTheIdentityBehindThem(t *testing.T) {
	h := newHarness(t)
	room := h.room("-200")
	gone := event.NewID()
	h.b.Memory = &partitionMemory{parts: []memory.Partition{
		{Name: "dm/" + string(h.owner.ID)},
		{Name: "room/" + string(room.ID)},
		{Name: "dm/" + string(gone)},
		{Name: "room/" + string(h.owner.ID)}, // a user is not a room
		{Name: "dm/tg/123"},
		{Name: "room/tg/-100"},
		{Name: "task/S9"},
	}}

	out := decode[struct {
		Partitions []gen.MemoryPartition `json:"partitions"`
	}](t, h.do(http.MethodGet, "/memory/partitions?agent=main", h.adminToken, nil), http.StatusOK)

	got := map[string]string{}
	for _, p := range out.Partitions {
		if p.Name != nil {
			got[p.Partition] = *p.Name
		}
	}
	want := map[string]string{
		"dm/" + string(h.owner.ID): h.owner.Name,
		"room/" + string(room.ID):  "Ops",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if len(out.Partitions) != 7 {
		t.Fatalf("partitions = %d, want all 7 kept", len(out.Partitions))
	}
}
