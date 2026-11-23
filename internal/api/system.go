package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/memory"
	"holaclaw/internal/provider"
	"holaclaw/internal/store"
)

// maxMemoryLimit is the cap the spec declares for a memory search.
const maxMemoryLimit = 100

// GetStatus is the cheapest call in the API: it is what the latency budget of
// docs/reference/api.md (p99 < 1 ms on localhost) is measured on.
func (h *handlers) GetStatus(w http.ResponseWriter, r *http.Request) {
	out := gen.Status{Version: h.b.Version}
	if !h.b.Started.IsZero() {
		out.UptimeSeconds = int(time.Since(h.b.Started) / time.Second)
	}
	if h.b.Sessions != nil {
		out.SessionsActive = h.b.Sessions.Active()
	}
	if h.b.Approvals != nil {
		if pending, err := h.b.Approvals.ListPending(r.Context()); err == nil {
			out.PendingApprovals = ptr(len(pending))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// RestartDaemon restarts the daemon in this process, which reads the config
// again.
func (h *handlers) RestartDaemon(w http.ResponseWriter, r *http.Request) {
	if h.b.Restart == nil {
		notImplemented(w, "restart")
		return
	}
	h.b.logger().Warn("api: the daemon restarts", "by", string(skillWho(r)))
	w.WriteHeader(http.StatusAccepted)
	// The shutdown this starts lets the request finish, so the answer is out.
	h.b.Restart()
}

// ListAgents describes the configured agents: what they may call and which
// model plays each role.
func (h *handlers) ListAgents(w http.ResponseWriter, _ *http.Request) {
	out := []gen.AgentInfo{}
	if h.b.Config != nil {
		names := make([]string, 0, len(h.b.Config.Agents))
		for n := range h.b.Config.Agents {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			a := h.b.Config.Agents[n]
			tools := a.Tools
			if tools == nil {
				tools = []string{}
			}
			out = append(out, gen.AgentInfo{Name: a.Name, Tools: tools, Models: roleMap(a)})
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Agents []gen.AgentInfo `json:"agents"`
	}{out})
}

// roleMap renders the model roles an agent actually sets (docs/models.md); an unset role
// is left out rather than reported as empty.
func roleMap(a config.Agent) map[string]string {
	out := map[string]string{}
	for role, name := range map[string]string{
		"chat":       a.Models.Chat,
		"fallback":   a.Models.Fallback,
		"guard":      a.Models.Guard,
		"summarize":  a.Models.Summarize,
		"transcribe": a.Models.Transcribe,
		"vision":     a.Models.Vision,
		"embed":      a.Models.Embed,
	} {
		if name != "" {
			out[role] = name
		}
	}
	return out
}

func (h *handlers) ListModels(w http.ResponseWriter, _ *http.Request) {
	out := []gen.ModelInfo{}
	if h.b.Config != nil {
		names := make([]string, 0, len(h.b.Config.Models))
		for n := range h.b.Config.Models {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			m := h.b.Config.Models[n]
			out = append(out, gen.ModelInfo{
				Name: m.Name, Provider: m.Provider, Id: m.ID, Context: m.Context,
				Tools: ptr(m.Tools), Vision: ptr(m.Vision),
			})
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Models []gen.ModelInfo `json:"models"`
	}{out})
}

// modelListTimeout bounds a provider's answer to ListProviderModels.
const modelListTimeout = 5 * time.Second

// ListProviderModels asks a provider for the model ids it serves, on every
// call. An unknown provider is a 404, one that cannot list its models a 501
// and a list that cannot be read a 502 with the provider's error, which never
// holds a secret.
func (h *handlers) ListProviderModels(w http.ResponseWriter, r *http.Request, name string) {
	p, ok := h.b.Providers[name]
	if !ok {
		notFound(w)
		return
	}
	lister, ok := p.(provider.ModelLister)
	if !ok {
		writeError(w, http.StatusNotImplemented, CodeNotImplemented, "provider "+name+" cannot list its models")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelListTimeout)
	defer cancel()
	ids, err := lister.Models(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, CodeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Models []string `json:"models"`
	}{ids})
}

// GetTrace joins the events and the messages of one trace: the full round trip,
// one copy (docs/traceability.md).
func (h *handlers) GetTrace(w http.ResponseWriter, r *http.Request, id gen.Id) {
	traceID := event.ID(id)
	events, err := h.b.Store.ListEvents(r.Context(), traceID)
	if err != nil {
		h.b.fail(w, "list events", err)
		return
	}
	msgs, err := h.b.Store.ListTraceMessages(r.Context(), traceID)
	if err != nil {
		h.b.fail(w, "list trace messages", err)
		return
	}
	if len(events) == 0 && len(msgs) == 0 {
		notFound(w)
		return
	}
	// A trace is as readable as the sessions it touches: a client token reads
	// the sessions it may see only, and a trace it may not read is simply not
	// there. Admin scope reads every trace: it is how admins inspect other
	// identities' conversations (docs/identity.md §4, docs/security.md, docs/traceability.md).
	p, _ := PrincipalFrom(r.Context())
	if p.Scope == auth.ScopeClient {
		ids := make([]event.ID, 0, 4)
		for _, e := range events {
			ids = append(ids, e.SessionID)
		}
		for _, m := range msgs {
			ids = append(ids, m.SessionID)
		}
		if !h.anyVisible(r.Context(), p, ids) {
			notFound(w)
			return
		}
	}
	out := gen.Trace{TraceId: string(traceID), Events: make([]gen.Event, 0, len(events)), Messages: make([]gen.Message, 0, len(msgs))}
	for _, e := range events {
		out.Events = append(out.Events, toGenEvent(e))
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, toGenMessage(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// --- memory -------------------------------------------------------------------

// SearchMemory reads one partition of one agent. Both are mandatory: the store
// is default-deny across partitions (docs/memory.md).
func (h *handlers) SearchMemory(w http.ResponseWriter, r *http.Request, params gen.SearchMemoryParams) {
	if h.b.Memory == nil {
		notImplemented(w, "memory")
		return
	}
	if params.Agent == "" || params.Partition == "" {
		badRequest(w, "agent and partition are required")
		return
	}
	q := memory.Query{Agent: params.Agent, Partition: params.Partition}
	if params.Q != nil {
		q.Text = *params.Q
	}
	if params.Layer != nil {
		q.Layers = []memory.Layer{memory.Layer(*params.Layer)}
	}
	if params.Limit != nil {
		if *params.Limit < 1 || *params.Limit > maxMemoryLimit {
			badRequest(w, "limit is out of range")
			return
		}
		q.Limit = *params.Limit
	}
	hits, err := h.b.Memory.Search(r.Context(), q)
	if err != nil {
		h.b.fail(w, "search memory", err)
		return
	}
	out := make([]gen.MemoryItem, 0, len(hits))
	for _, hit := range hits {
		item := toGenMemory(hit.Item)
		item.Score = ptr(float32(hit.Score))
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, struct {
		Items []gen.MemoryItem `json:"items"`
	}{out})
}

// ListMemoryPartitions names the memory spaces one agent holds. It is the one
// read that crosses partitions, and it may: a name and two counts say what
// exists without saying anything that is in it. Enumerating them is optional
// for a backend, so a store that cannot answers not_implemented rather than
// pretending the agent has none.
func (h *handlers) ListMemoryPartitions(w http.ResponseWriter, r *http.Request, params gen.ListMemoryPartitionsParams) {
	lister, ok := h.b.Memory.(memory.PartitionLister)
	if h.b.Memory == nil || !ok {
		notImplemented(w, "memory partitions")
		return
	}
	if params.Agent == "" {
		badRequest(w, "agent is required")
		return
	}
	parts, err := lister.Partitions(r.Context(), params.Agent)
	if err != nil {
		h.b.fail(w, "list memory partitions", err)
		return
	}
	out := make([]gen.MemoryPartition, 0, len(parts))
	for _, p := range parts {
		item := gen.MemoryPartition{Partition: p.Name, Notes: p.Notes, Recall: p.Recall}
		name, err := h.partitionIdentityName(r, p.Name)
		if err != nil {
			h.b.fail(w, "list memory partitions", err)
			return
		}
		if name != "" {
			item.Name = &name
		}
		if !p.LastWritten.IsZero() {
			item.LastWritten = ptr(p.LastWritten)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, struct {
		Partitions []gen.MemoryPartition `json:"partitions"`
	}{out})
}

// partitionIdentityName is the name of the identity behind a dm/<id> or
// room/<id> partition, empty when there is none: another kind of partition, a
// legacy or isolated one whose rest is not an id, or an identity that is gone.
// Any other lookup failure is the caller's to report.
func (h *handlers) partitionIdentityName(r *http.Request, partition string) (string, error) {
	kind, rest, ok := strings.Cut(partition, "/")
	id := event.ID(rest)
	if !ok || h.b.Store == nil || !id.Valid() {
		return "", nil
	}
	want := store.KindUser
	switch kind {
	case "dm":
	case "room":
		want = store.KindRoom
	default:
		return "", nil
	}
	i, err := h.b.Store.GetIdentity(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && i.Kind != want) {
		return "", nil
	}
	return i.Name, err
}

// WriteMemory stores a note. Provenance is written here, from the request's
// trusted state — never from the body.
func (h *handlers) WriteMemory(w http.ResponseWriter, r *http.Request) {
	if h.b.Memory == nil {
		notImplemented(w, "memory")
		return
	}
	var in gen.MemoryWrite
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Agent == "" || in.Partition == "" {
		badRequest(w, "agent and partition are required")
		return
	}
	if in.Text == "" {
		badRequest(w, "text is required")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	// The default is the narrow one: a note is readable from the partition it
	// came from unless the caller asked for `shared` (docs/memory.md).
	item := memory.Item{
		Layer:      memory.LayerNote,
		Text:       in.Text,
		Visibility: memory.VisOrigin,
		Prov: memory.Provenance{
			Agent:     in.Agent,
			Partition: in.Partition,
			UserID:    p.UserID,
			Role:      string(p.Role),
			Time:      time.Now().UTC(),
		},
	}
	if in.About != nil {
		item.About = *in.About
	}
	if in.Visibility != nil {
		item.Visibility = memory.Visibility(*in.Visibility)
	}
	id, err := h.b.Memory.Write(r.Context(), item)
	if err != nil {
		if errors.Is(err, memory.ErrBudget) {
			writeError(w, http.StatusConflict, CodeConflict, "notes budget exhausted, consolidate first")
			return
		}
		h.b.fail(w, "write memory", err)
		return
	}
	item.ID = id
	writeJSON(w, http.StatusCreated, toGenMemory(item))
}

func (h *handlers) ForgetMemory(w http.ResponseWriter, r *http.Request, id gen.Id, params gen.ForgetMemoryParams) {
	if h.b.Memory == nil {
		notImplemented(w, "memory")
		return
	}
	if params.Agent == "" || params.Partition == "" {
		badRequest(w, "agent and partition are required")
		return
	}
	err := h.b.Memory.Forget(r.Context(), params.Agent, params.Partition, event.ID(id))
	switch {
	case errors.Is(err, memory.ErrNotFound), errors.Is(err, store.ErrNotFound):
		notFound(w)
		return
	case err != nil:
		h.b.fail(w, "forget memory", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toGenMemory(it memory.Item) gen.MemoryItem {
	return gen.MemoryItem{
		Id:         string(it.ID),
		Layer:      gen.MemoryItemLayer(it.Layer),
		Text:       it.Text,
		About:      optString(it.About),
		Visibility: gen.MemoryItemVisibility(it.Visibility),
		Partition:  it.Prov.Partition,
		UserId:     optID(it.Prov.UserID),
		TraceId:    optID(it.Prov.TraceID),
		CreatedAt:  it.Prov.Time,
	}
}
