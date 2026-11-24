package event

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Type names a trace event. The list is part of the event schema contract:
// types are added, not removed (docs/traceability.md, ARCHITECTURE.md §7). #151 renamed the
// identity.alias_* pair to identity.bound and identity.unbound, and
// access.requested went with dm = "request" (#183); rows stored before either
// keep their old type. Both the consts and Types stay sorted by name, so a type
// is where you expect it.
type Type string

const (
	AccessGranted          Type = "access.granted" // a room was approved, by an admin or a room invite
	ApprovalRequested      Type = "approval.requested"
	ApprovalResolved       Type = "approval.resolved"
	AuthRefreshed          Type = "auth.refreshed" // a provider's OAuth token was refreshed
	BackupCreated          Type = "backup.created" // a managed backup finished and is ready to download
	BackupDeleted          Type = "backup.deleted"
	BackupFailed           Type = "backup.failed"
	BackupRestored         Type = "backup.restored" // the daemon started on the data of a backup
	BrowserBlocked         Type = "browser.blocked" // a run hit a blocker: site not allowed, approval refused, a login, captcha or 2FA, or a give-up offered to a person
	BrowserDownload        Type = "browser.download"
	BrowserMove            Type = "browser.move" // one move call (plan or chat role) and the moves it proposed
	BrowserScreenshot      Type = "browser.screenshot" // one screenshot of the page was taken; never the image
	BrowserStep            Type = "browser.step"       // one action run on the page; also waits, holds and recoveries
	BrowserTakeoverEnded   Type = "browser.takeover.ended"
	BrowserTakeoverRequest Type = "browser.takeover.requested" // the run asks a person to drive the browser
	BrowserTakeoverStarted Type = "browser.takeover.started"
	BrowserTaskFinished    Type = "browser.task.finished"
	BrowserTaskStarted     Type = "browser.task.started"
	ChannelReacted         Type = "channel.reacted" // the agent put an emoji on the turn's inbound message
	ChannelReceived        Type = "channel.received"
	ChannelSent            Type = "channel.sent"
	ChannelStatus          Type = "channel.status"   // a channel's connection state changed (linking, connected, ...)
	ConnectorStatus        Type = "connector.status" // a connector's state changed (pending, authorizing, connected, ...)
	ContextBuilt           Type = "context.built"    // the prompt was assembled: message ids, fragments, token breakdown
	ContextCompacted       Type = "context.compacted"
	DeliveryDegraded       Type = "delivery.degraded"  // an attachment the channel cannot send went out as a text notice
	GuardDecision          Type = "guard.decision"     // the guard model's verdict on a tool call
	IdentityAssigned       Type = "identity.assigned"  // a pending identity's bindings moved onto a confirmed one
	IdentityBound          Type = "identity.bound"     // a binding was added to an identity that already existed
	IdentityConfirmed      Type = "identity.confirmed" // an admin named a pending identity and gave it a role
	IdentityCreated        Type = "identity.created"   // an identity was created, by the daemon or an admin
	IdentityDeleted        Type = "identity.deleted"   // an identity was deleted with its bindings, and its own memory unless kept
	IdentityDisabled       Type = "identity.disabled"  // an admin switched a confirmed identity off
	IdentityEnabled        Type = "identity.enabled"   // an admin switched a disabled identity back on
	IdentityUnbound        Type = "identity.unbound"   // a binding was removed from its identity
	ImageGenerated         Type = "image.generated"
	MediaDescribed         Type = "media.described"
	MediaRefused           Type = "media.refused" // an inbound attachment was not processed (video, too large, failed)
	MediaTranscribed       Type = "media.transcribed"
	MemoryEmbedded         Type = "memory.embedded" // one embeddings call: memory items or a search query
	MemoryForgotten        Type = "memory.forgotten"
	MemoryInjected         Type = "memory.injected" // recalled memories were added to the prompt
	MemorySaved            Type = "memory.saved"
	ModelError             Type = "model.error"
	ModelFallback          Type = "model.fallback" // the run switched to the fallback model
	ModelPartial           Type = "model.partial"  // stream only: the turn's text so far, to a stream that joins mid-turn
	ModelProgress          Type = "model.progress" // a heartbeat while the model is still answering
	ModelRequest           Type = "model.request"
	ModelResponse          Type = "model.response"
	ModelToken             Type = "model.token"       // stream only, never persisted
	ModelUsageLimit        Type = "model.usage_limit" // the provider's quota is spent until reset_at
	PolicyDecision         Type = "policy.decision"   // the tool gate allowed, refused or sent a call for approval
	Reply                  Type = "reply"             // an answer ready for delivery to the session
	RouterDropped          Type = "router.dropped"    // an inbound message was dropped: unknown sender, disabled room, rate limit
	RouterResolved         Type = "router.resolved"   // an inbound message was matched to an identity, agent and session
	RunAborted             Type = "run.aborted"
	RunFinished            Type = "run.finished"
	RunLoopDetected        Type = "run.loop_detected" // the model repeated the same tool call
	RunStarted             Type = "run.started"
	RunStepLimit           Type = "run.step_limit"
	SkillForgotten         Type = "skill.forgotten"
	SkillInstalled         Type = "skill.installed"
	SkillLoaded            Type = "skill.loaded"
	SkillRestored          Type = "skill.restored"
	SkillReverted          Type = "skill.reverted"
	SkillSaved             Type = "skill.saved"
	TaskFinished           Type = "task.finished"
	TaskMissed             Type = "task.missed" // occurrences skipped while the daemon was down or the last run still going
	TaskScheduled          Type = "task.scheduled"
	TaskStarted            Type = "task.started"
	TokenCreated           Type = "token.created" // an API token was minted: token create, rotation or init
	TokenRevoked           Type = "token.revoked" // an API token was revoked: directly, by a rotation, an unbind or a delete
	TokenUpdated           Type = "token.updated" // an API token's scope changed
	ToolCallEvent          Type = "tool.call"
	ToolRejected           Type = "tool.rejected" // a call refused before it ran: bad arguments or a schema mismatch
	ToolRepaired           Type = "tool.repaired" // a malformed call, or one leaked into text, was fixed and kept
	ToolResult             Type = "tool.result"
)

// Types lists every known event type, for schema export and validation.
var Types = []Type{
	AccessGranted,
	ApprovalRequested, ApprovalResolved,
	AuthRefreshed,
	BackupCreated, BackupDeleted, BackupFailed, BackupRestored,
	BrowserBlocked, BrowserDownload, BrowserMove, BrowserScreenshot, BrowserStep, BrowserTakeoverEnded,
	BrowserTakeoverRequest, BrowserTakeoverStarted, BrowserTaskFinished, BrowserTaskStarted,
	ChannelReacted, ChannelReceived, ChannelSent, ChannelStatus,
	ConnectorStatus,
	ContextBuilt, ContextCompacted,
	DeliveryDegraded,
	GuardDecision,
	IdentityAssigned, IdentityBound, IdentityConfirmed, IdentityCreated, IdentityDeleted, IdentityDisabled,
	IdentityEnabled, IdentityUnbound,
	ImageGenerated,
	MediaDescribed, MediaRefused, MediaTranscribed,
	MemoryEmbedded, MemoryForgotten, MemoryInjected, MemorySaved,
	ModelError, ModelFallback, ModelPartial, ModelProgress, ModelRequest, ModelResponse, ModelToken,
	ModelUsageLimit,
	PolicyDecision,
	Reply,
	RouterDropped, RouterResolved,
	RunAborted, RunFinished, RunLoopDetected, RunStarted, RunStepLimit,
	SkillForgotten, SkillInstalled, SkillLoaded, SkillRestored, SkillReverted, SkillSaved,
	TaskFinished, TaskMissed, TaskScheduled, TaskStarted,
	TokenCreated, TokenRevoked, TokenUpdated,
	ToolCallEvent, ToolRejected, ToolRepaired, ToolResult,
}

// Ephemeral reports whether events of this type are streamed to clients but
// never written to the store.
func (t Type) Ephemeral() bool { return t == ModelToken || t == ModelPartial }

// Event is one hop of a trace. Append-only. Field names follow OpenTelemetry.
// Attrs must never contain secrets.
type Event struct {
	Schema    int             `json:"schema"`
	ID        ID              `json:"id"`
	TraceID   ID              `json:"trace_id"`
	SpanID    ID              `json:"span_id"`
	ParentID  ID              `json:"parent_id,omitempty"`
	SessionID ID              `json:"session_id,omitempty"`
	Type      Type            `json:"type"`
	Time      time.Time       `json:"time"`
	Duration  time.Duration   `json:"duration_ns,omitempty"`
	MessageID ID              `json:"message_id,omitempty"` // message this event refers to
	Attrs     json.RawMessage `json:"attrs,omitempty"`      // JSON object
}

// New builds an event with fresh ids. attrs is marshalled to JSON; a marshal
// failure yields an attrs object carrying the error instead of dropping the
// event.
func New(t Type, traceID, sessionID, parentID ID, attrs any) Event {
	now := time.Now()
	e := Event{
		Schema: SchemaVersion, ID: NewIDAt(now), TraceID: traceID, SpanID: NewIDAt(now),
		ParentID: parentID, SessionID: sessionID, Type: t, Time: now.UTC(),
	}
	if attrs != nil {
		b, err := json.Marshal(attrs)
		if err != nil {
			b, _ = json.Marshal(map[string]string{"attrs_error": err.Error()})
		}
		e.Attrs = b
	}
	return e
}

// Sink receives events. The store persists them; the API fans them out.
// Emit must not block the caller for long and must be safe for concurrent use.
type Sink interface {
	Emit(ctx context.Context, e Event)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(ctx context.Context, e Event)

func (f SinkFunc) Emit(ctx context.Context, e Event) { f(ctx, e) }

// Discard is a Sink that drops everything.
var Discard Sink = SinkFunc(func(context.Context, Event) {})

// Multi fans an event out to several sinks in order.
func Multi(sinks ...Sink) Sink {
	return SinkFunc(func(ctx context.Context, e Event) {
		for _, s := range sinks {
			s.Emit(ctx, e)
		}
	})
}

// Ordered fans out like Multi but stamps a fresh id on every event while
// holding one lock, so ids increase in emission order. Callers create events
// concurrently, and an id made earlier can be emitted later; anything that
// resumes by "events after id X" (the SSE stream) needs the two orders to
// agree. SpanID and ParentID are untouched.
func Ordered(sinks ...Sink) Sink {
	var mu sync.Mutex
	return SinkFunc(func(ctx context.Context, e Event) {
		mu.Lock()
		defer mu.Unlock()
		e.ID = NewID()
		for _, s := range sinks {
			s.Emit(ctx, e)
		}
	})
}
