// Package agent is the pure agent library: the context builder and the agent
// loop, behind the Provider, tool.Executor and Store interfaces. The daemon,
// `holaclaw chat` and the eval harness all run this same code
// (ARCHITECTURE.md §2, docs/context.md).
package agent

import (
	"context"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// Definition is what an agent *is*, loaded from agents/<name>/ by the config
// package and handed to the library as plain data.
type Definition struct {
	Name    string
	Core    string // built-in core prompt fragment (≤ ~300 tokens)
	Persona string // agents/<name>/prompt.md
	Tools   []string
	// Connectors are the connector ids granted to the agent. A tool spec
	// named "<id>.<tool>" for one of them is a connector schema: outside
	// tool.MaxSchemaTokens, counted in Breakdown.Connectors.
	Connectors []string
	// Skills is the run's skill index: entries the daemon already found
	// eligible (docs/skills.md §3). The builder renders them, never filters.
	Skills []SkillEntry
	// Limits; zero values mean defaults.
	MaxSteps     int // hard step limit per turn (default 12)
	ReplyReserve int // tokens reserved for the reply (default 1024, capped to a quarter of the window)
}

// SkillEntry is one line of the skills fragment.
type SkillEntry struct {
	Name, Description string
}

// Fragment is one named piece of the system prompt, for budgeting and hashes.
type Fragment struct {
	Name   string // "core", "persona", "connectors", "tool:<name>", "connector:<id>.<tool>", ...
	Tokens int
	Hash   string // hex sha256 of the fragment text
}

// Breakdown is the token accounting recorded in context.built and printed by
// `holaclaw prompt-size`. Same numbers in both places.
type Breakdown struct {
	Window       int
	Fragments    []Fragment // system fragments, then one per tool schema
	System       int        // sum of system fragments but "connectors"
	ToolSchemas  int        // sum of built-in tool schema fragments
	Connectors   int        // the "connectors" fragment plus every connector schema
	History      int
	Volatile     int // memory injections + per-turn metadata
	ReplyReserve int
	Total        int // System + ToolSchemas + Connectors + History + Volatile
}

// Prompt is a built, budgeted prompt ready for a provider.
type Prompt struct {
	System     string
	Messages   []provider.Message
	Tools      []provider.ToolSpec
	PrefixHash string     // hash of System + tool schemas: the byte-stable prefix
	MessageIDs []event.ID // transcript messages included, for context.built
	MemoryIDs  []event.ID // injected memories, for context.built
	Breakdown  Breakdown
	// NeedsCompaction is set when history no longer fits and the caller
	// should compact before the next turn.
	NeedsCompaction bool
}

// Injection is volatile content placed in the tail of the prompt (recall
// hits with provenance, precise time, per-message metadata).
type Injection struct {
	ID   event.ID // memory id, empty for metadata
	Text string
}

// BuildInput is everything the context builder needs for one model call.
type BuildInput struct {
	Def        Definition
	Model      provider.Model
	Tools      []provider.ToolSpec
	History    []event.Message // transcript after the compaction point, in order
	Injections []Injection
	// Images resolves image attachments to bytes for vision-capable models;
	// nil means images are represented by their stub/derived text only.
	Images func(ctx context.Context, a event.Attachment) ([]byte, error)
}

// Builder assembles a prompt under the model's budget.
type Builder interface {
	Build(ctx context.Context, in BuildInput) (Prompt, error)
}
