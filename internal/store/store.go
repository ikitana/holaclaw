// Package store defines the persistence contract and its SQLite
// implementation: one SQLite file (pure Go driver) plus a content-addressed
// blob directory, with numbered forward-only migrations (ARCHITECTURE.md §2, §7).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"holaclaw/internal/event"
)

// ErrNotFound is returned when a row or blob does not exist.
var ErrNotFound = errors.New("store: not found")

// Role of a confirmed identity.
type Role string

const (
	RoleOwner Role = "owner"
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
	// RoleGuest is never stored on an identity: a pending one has no role.
	// Invites store it (a room invite's placeholder), and above the store it
	// names the policy tier a pending identity runs under.
	RoleGuest Role = "guest"
)

// IdentityKind is a person or a room (docs/identity.md §1).
type IdentityKind string

const (
	KindUser IdentityKind = "user"
	KindRoom IdentityKind = "room"
)

// IdentityStatus says whether anyone has confirmed the identity, and whether
// an admin has switched it off.
type IdentityStatus string

const (
	StatusPending   IdentityStatus = "pending"
	StatusConfirmed IdentityStatus = "confirmed"
	StatusDisabled  IdentityStatus = "disabled"
)

// Identity is a person holding any number of bindings, or a room holding
// exactly one. A person's Role is empty exactly while it is pending; a room's
// always is.
type Identity struct {
	ID     event.ID
	Kind   IdentityKind
	Name   string
	Role   Role
	Status IdentityStatus
	// ApprovedAt is when a room was approved, nil while it isn't. Never set on a
	// person.
	ApprovedAt *time.Time
	// ApprovedBy is who approved the room, empty when unknown. Plain provenance,
	// not a link: it outlives the approver.
	ApprovedBy event.ID
	CreatedAt  time.Time
}

// Pending reports whether nobody has confirmed the identity yet.
func (i Identity) Pending() bool { return i.Status == StatusPending }

// Disabled reports whether an admin has switched the identity off.
func (i Identity) Disabled() bool { return i.Status == StatusDisabled }

// Approved reports whether a room has been approved.
func (i Identity) Approved() bool { return i.ApprovedAt != nil }

// Binding links one platform alias on one channel instance to an identity.
type Binding struct {
	Channel    string
	Alias      string
	IdentityID event.ID
	Display    string
	// Platform is what a paired device said it runs on ("ios", "macos");
	// empty for every other binding.
	Platform  string
	CreatedAt time.Time
}

// Resolution is what one envelope's aliases resolved to (docs/identity.md
// §2): the first trusted match in envelope order, the aliases nobody holds
// yet, and the aliases left alone.
type Resolution struct {
	Identity Identity // zero when nothing trusted matched
	Matched  string   // the alias Identity was found through
	// Free are the envelope's aliases linked to nobody. ResolveAndLink links
	// them to Identity; without an Identity they are what a new one may take.
	Free      []string
	Conflicts []AliasConflict
}

// AliasConflict is an envelope alias the resolution did not act on. Without
// Held, IdentityID is another identity that holds Alias. With Held, IdentityID
// matched but is not trusted: it already holds Held in Alias's namespace, so
// Alias would be its second id there (a recycled number); its own aliases in
// the envelope are not listed again.
type AliasConflict struct {
	Alias      string
	IdentityID event.ID
	Held       string
}

type SessionKind string

const (
	SessionChat SessionKind = "chat"
	SessionTask SessionKind = "task"
)

type Session struct {
	ID            event.ID
	Key           string
	Agent         string
	Kind          SessionKind
	UserID        event.ID // may be empty
	Origin        event.Origin
	Title         string
	PrefixHash    string
	SummaryMsgID  event.ID // messages before this id are compacted away
	CompactFails  int
	TurnToken     string
	RunGeneration int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	EndedAt       *time.Time
}

// Blob is the metadata of a stored blob.
type Blob struct {
	ID        string // hex sha256
	Size      int64
	MIME      string
	CreatedAt time.Time
}

// ConnectorState is where a connector is in its lifecycle
// (docs/connectors.md §2). A deleted connector has no row; "deleted" exists
// only as the state of its last connector.status event.
type ConnectorState string

const (
	ConnectorPending     ConnectorState = "pending"     // the intent exists and discovery is running
	ConnectorAuthorizing ConnectorState = "authorizing" // the authorization URL waits for a person
	ConnectorConnected   ConnectorState = "connected"
	ConnectorDegraded    ConnectorState = "degraded" // a ping or refresh failed; calls still try
	ConnectorExpired     ConnectorState = "expired"  // the refresh token is dead: reconnect
)

// Connector is the cache row of one connector: what its server told us and
// where it is in its lifecycle. The intent is in connectors.toml, the tokens
// in auth.toml, and the open OAuth intent in memory only — none of them here.
type Connector struct {
	ID            string // the connectors.toml id
	State         ConnectorState
	Detail        string // closed set of daemon strings, never server text
	ServerName    string
	ServerVersion string
	SessionID     string // Mcp-Session-Id
	ClientID      string // the granted client id: pre-registered, the CIMD URL, or from registration
	CIMDRefused   bool   // the authorization server refused the metadata document: use DCR
	AuthMeta      json.RawMessage
	Tools         json.RawMessage // tools/list as the server declared it
	Scopes        []string        // granted
	LastCheck     *time.Time
	Failures      int        // consecutive failed checks
	TokenExpiry   *time.Time // of the access token; nil for an open server
	UpdatedAt     time.Time
}

// ConnectorStore persists the connector cache rows. Like the trace and usage
// listers it is a narrow interface beside Store, not part of it.
type ConnectorStore interface {
	// PutConnector inserts or replaces the whole row. UpdatedAt defaults to
	// now.
	PutConnector(ctx context.Context, c Connector) error
	GetConnector(ctx context.Context, id string) (Connector, error)
	// ListConnectors returns every row ordered by id.
	ListConnectors(ctx context.Context) ([]Connector, error)
	DeleteConnector(ctx context.Context, id string) error
}

// InboundStore remembers the inbound messages core has handled, so a message
// that comes back (a redelivery, or a resend under another addressing, even
// after a restart) is routed at most once (#101). A narrow interface beside
// Store.
type InboundStore interface {
	// ClaimInbound records keys for a message on a channel instance and
	// reports whether it is new: false when any key is already recorded (the
	// missing ones are then recorded too, so a copy under another addressing
	// is remembered). No keys is always new and records nothing. Recorded
	// keys keep their first time, and are stored hashed. One transaction.
	// The extra keys a duplicate records outlive a release of the original:
	// a narrow race, accepted, since it errs towards at most once.
	ClaimInbound(ctx context.Context, channel string, keys []string, at time.Time) (fresh bool, err error)
	// ReleaseInbound forgets keys, so a message whose handling failed is
	// processed when redelivered.
	ReleaseInbound(ctx context.Context, channel string, keys []string) error
	// PruneInbound deletes keys recorded before the cutoff and reports how
	// many went.
	PruneInbound(ctx context.Context, before time.Time) (int64, error)
}

// Sent is one platform message core delivered, kept so a reaction to it can
// be routed by its target.
type Sent struct {
	Channel       string
	Conversation  string
	PlatformMsgID string
	SessionID     event.ID
	MessageID     event.ID // the transcript message; empty for an approval prompt
	ApprovalID    event.ID // set when the message is an approval prompt
	CreatedAt     time.Time
}

// SentStore remembers delivered platform messages. Like ConnectorStore it is
// a narrow interface beside Store.
type SentStore interface {
	// PutSent records rows; a platform id seen again replaces its row.
	PutSent(ctx context.Context, rows []Sent) error
	// GetSent is the row of one platform message, matched on the whole id, or
	// ErrNotFound. Adapter ids are unique per channel instance.
	GetSent(ctx context.Context, channel, platformMsgID string) (Sent, error)
	// PruneSent deletes rows older than the cutoff and reports how many went.
	PruneSent(ctx context.Context, before time.Time) (int64, error)
}

// Store is the persistence interface the agent library and the daemon depend
// on. All methods are safe for concurrent use. Later milestones extend the
// implementation with further narrow interfaces (memory, approvals, tasks)
// rather than growing this one.
type Store interface {
	event.Sink // Emit persists non-ephemeral events; failures are logged, not returned

	// Identities and their bindings.
	CreateIdentity(ctx context.Context, i Identity) error
	GetIdentity(ctx context.Context, id event.ID) (Identity, error)
	ListIdentities(ctx context.Context) ([]Identity, error)
	DeleteIdentity(ctx context.Context, id event.ID) error
	// ResolveAndLink resolves one envelope's aliases like ResolveAliases and,
	// in the same transaction, links its free aliases to the identity found.
	// ErrNotFound when no trusted identity matched; the Resolution still
	// carries Free and Conflicts.
	ResolveAndLink(ctx context.Context, channel string, aliases []string, display string, namespaces []string) (Resolution, error)

	// Rooms: a room identity holds one binding, its conversation id.
	// GetRoom is the room of a conversation, ErrNotFound when it has none, or
	// ErrAliasLinked when a user identity holds that binding: it is never
	// adopted.
	GetRoom(ctx context.Context, channel, conversation string) (Identity, error)
	// EnsureRoom is GetRoom, creating a pending room when there is none, and
	// reports whether it did.
	EnsureRoom(ctx context.Context, channel, conversation string, at time.Time) (Identity, bool, error)
	// ApproveRoom sets approved_at and approved_by (NULL for an empty by),
	// creating the pending room when there is none, and reports whether this
	// call created it and approved it. ErrRoomDisabled when an admin switched
	// the room off: approving never revives it.
	ApproveRoom(ctx context.Context, channel, conversation string, by event.ID, at time.Time) (Identity, RoomChange, error)

	// Sessions.
	// EnsureSession returns the session for key, creating it if needed, in
	// one atomic operation. created reports whether it was new.
	EnsureSession(ctx context.Context, s Session) (sess Session, created bool, err error)
	// AdoptSession gives a session with no owner to owner, the room identity
	// that replaced a deleted one, and returns the row: one that has an owner
	// keeps it.
	AdoptSession(ctx context.Context, id, owner event.ID) (Session, error)
	GetSession(ctx context.Context, id event.ID) (Session, error)
	ListSessions(ctx context.Context) ([]Session, error)
	UpdateSessionContext(ctx context.Context, id event.ID, prefixHash string, summaryMsgID event.ID, compactFails int) error
	// DeleteSession cascades to messages, events and unreferenced blobs.
	DeleteSession(ctx context.Context, id event.ID) error
	// BeginTurn claims the session for one turn: sets a fresh turn token,
	// bumps the run generation and returns both. EndTurn clears the token
	// only if it still matches.
	BeginTurn(ctx context.Context, id event.ID) (token string, generation int64, err error)
	EndTurn(ctx context.Context, id event.ID, token string) error
	// HealTurns clears turn tokens left behind by a crash; returns the
	// sessions that had one.
	HealTurns(ctx context.Context) ([]event.ID, error)

	// Messages.
	// AppendMessage stores a message. Storing the same id twice is a no-op
	// that reports inserted=false (idempotency). pending marks a mailbox
	// entry not yet consumed by a turn. A non-pending message without Pos is
	// placed at its id, never before the transcript's end.
	AppendMessage(ctx context.Context, m event.Message, pending bool) (inserted bool, err error)
	GetMessage(ctx context.Context, id event.ID) (event.Message, error)
	// ListMessages returns the transcript in order (Message.Pos), resuming
	// past the message with id after; after="" means from the start,
	// limit<=0 means no limit. Pending messages are excluded.
	ListMessages(ctx context.Context, sessionID, after event.ID, limit int) ([]event.Message, error)
	// TakePending atomically returns and un-marks the pending messages, each
	// placed after everything already in the transcript. perMention stops at
	// the first message that mentions the bot; the rest stay pending.
	TakePending(ctx context.Context, sessionID event.ID, perMention bool) ([]event.Message, error)
	// SessionsWithPending lists sessions that have unconsumed mailbox entries
	// (restart replay).
	SessionsWithPending(ctx context.Context) ([]event.ID, error)

	// Events.
	ListEvents(ctx context.Context, traceID event.ID) ([]event.Event, error)
	// ReceivedTrace is the trace that received one platform message on a
	// channel instance (its channel.received event), or ErrNotFound.
	ReceivedTrace(ctx context.Context, channel, platformMsgID string) (event.ID, error)
	// ListEventsAfter pages events in id order, optionally for one session
	// (SSE resume via Last-Event-ID).
	ListEventsAfter(ctx context.Context, after event.ID, sessionID event.ID, limit int) ([]event.Event, error)
	ListTraceMessages(ctx context.Context, traceID event.ID) ([]event.Message, error)

	// Blobs.
	PutBlob(ctx context.Context, r io.Reader, mime string) (Blob, error)
	OpenBlob(ctx context.Context, id string) (io.ReadCloser, Blob, error)

	Close() error
}
