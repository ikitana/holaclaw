// Package tool defines the tool contract, the registry and (in gate.go, M1)
// the policy gate. Tools are typed Go functions with compact schemas, each
// declaring a risk class (docs/security.md). The agent loop can only reach a
// tool through an Executor.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tokens"
)

// Risk is the class the policy gate decides on.
type Risk string

const (
	RiskRead     Risk = "read"
	RiskWrite    Risk = "write"
	RiskExternal Risk = "external"
	RiskExec     Risk = "exec"
)

// MaxSchemaTokens is the hard cap for one tool's name + description +
// parameter schema. Register rejects anything larger (docs/context.md).
const MaxSchemaTokens = 150

// ReactTool reacts to the turn's inbound message. The loop offers it only
// when Call.CanReact is set.
const ReactTool = "react"

// RunKind distinguishes a run with a human attached from a detached task.
type RunKind string

const (
	RunInteractive RunKind = "interactive"
	RunTask        RunKind = "task"
)

// Roles in Call.Role, the sender's role as the store records it. Compare
// with these, never with a literal.
const (
	RoleOwner = "owner"
	RoleAdmin = "admin"
	RoleUser  = "user"
	RoleGuest = "guest"
)

// Call is everything a tool or the gate may know about one invocation. All
// fields are set by code from trusted state; none come from the model except
// Name, CallID and Args.
type Call struct {
	CallID string
	Name   string
	Args   json.RawMessage

	TraceID   event.ID
	SessionID event.ID
	Agent     string
	UserID    event.ID
	Role      string       // owner | admin | user | guest
	Origin    event.Origin // trusted envelope origin; delivery targets derive from it
	RunKind   RunKind
	TaskID    event.ID // set for task runs, used to look up scoped grants
	Tainted   bool     // untrusted content entered this turn
	Partition string   // memory partition key of this conversation
	Workspace string   // absolute path of the agent workspace
	CanReact  bool     // the conversation's channel takes reactions (the `react` tool)
}

// Result is what a tool returns to the model.
type Result struct {
	Text string
	// IsError marks a failure the model should see and may recover from.
	IsError bool
	// Tainted marks the text as untrusted external content.
	Tainted bool
	// Attachments are queued for the final reply (the `attach` tool).
	Attachments []event.Attachment
	// Reacted marks a reaction put on the turn's inbound message (the `react`
	// tool): a turn that wrote no text sends nothing else.
	Reacted bool
}

// Tool is one callable capability. A Tool with several operations registers
// one Tool per operation (e.g. "memory.save", "memory.search").
type Tool interface {
	Spec() provider.ToolSpec
	Risk() Risk
	// Run executes the call. Args have been validated as a JSON object; the
	// tool validates its own fields and returns a Result with IsError for bad
	// input. A returned error means an internal failure.
	Run(ctx context.Context, c Call) (Result, error)
}

// Executor runs a tool call. The registry executes directly; the policy gate
// wraps an Executor and is what the daemon hands to the agent loop.
type Executor interface {
	// Specs lists the tools visible to the model for this agent.
	Specs() []provider.ToolSpec
	Execute(ctx context.Context, c Call) (Result, error)
}

// SchemaTokens is the budgeted size of a tool spec: the same number
// `holaclaw prompt-size` and context.built report.
func SchemaTokens(s provider.ToolSpec) int {
	return tokens.Estimate(s.Name) + tokens.Estimate(s.Description) + tokens.Estimate(string(s.Parameters))
}

// Registry holds the compiled-in tools. Safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds a tool. It fails on duplicates, invalid schemas and schemas
// over MaxSchemaTokens.
func (r *Registry) Register(t Tool) error {
	s := t.Spec()
	if s.Name == "" {
		return fmt.Errorf("tool: empty name")
	}
	var obj map[string]any
	if err := json.Unmarshal(s.Parameters, &obj); err != nil {
		return fmt.Errorf("tool %s: parameters are not a JSON object: %w", s.Name, err)
	}
	if n := SchemaTokens(s); n > MaxSchemaTokens {
		return fmt.Errorf("tool %s: schema is %d tokens, cap is %d", s.Name, n, MaxSchemaTokens)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[s.Name]; dup {
		return fmt.Errorf("tool %s: already registered", s.Name)
	}
	r.tools[s.Name] = t
	return nil
}

// Get returns a tool by exact name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names returns all registered tool names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Select returns an Executor limited to the given entries. An entry is either
// an exact tool name ("fetch") or a group prefix ("memory" selects every
// "memory.*"). Unknown entries are an error. Order is stable (sorted) so the
// prompt prefix stays byte-stable.
func (r *Registry) Select(entries []string) (Executor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	picked := map[string]Tool{}
	for _, e := range entries {
		found := false
		for name, t := range r.tools {
			if name == e || (len(name) > len(e) && name[:len(e)] == e && name[len(e)] == '.') {
				picked[name] = t
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("tool: unknown tool or group %q", e)
		}
	}
	names := make([]string, 0, len(picked))
	for n := range picked {
		names = append(names, n)
	}
	sort.Strings(names)
	sel := &selection{tools: picked}
	for _, n := range names {
		sel.specs = append(sel.specs, picked[n].Spec())
	}
	return sel, nil
}

type selection struct {
	tools map[string]Tool
	specs []provider.ToolSpec
}

func (s *selection) Specs() []provider.ToolSpec { return s.specs }

func (s *selection) Execute(ctx context.Context, c Call) (Result, error) {
	t, ok := s.tools[c.Name]
	if !ok {
		return Result{Text: fmt.Sprintf("unknown tool %q", c.Name), IsError: true}, nil
	}
	return t.Run(ctx, c)
}

// Lookup exposes the tool behind a name for wrappers (the policy gate needs
// the risk class).
func (s *selection) Lookup(name string) (Tool, bool) { t, ok := s.tools[name]; return t, ok }

// Lookuper is implemented by executors that can reveal tool metadata.
type Lookuper interface {
	Lookup(name string) (Tool, bool)
}
