-- holaclaw DB schema v1. Forward-only; never edit after release: a change is a
-- new numbered migration.

-- Identities (docs/identity.md §1). A user's role is NULL while pending; a
-- disabled one keeps its role. A room has no role, and approved_at is when it
-- was approved (NULL: not approved). approved_by is who approved it, plain
-- text with no foreign key: it records provenance, not a link between
-- identities, so it survives the approver being deleted.
CREATE TABLE identities (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL DEFAULT 'user' CHECK (kind IN ('user','room')),
  name         TEXT NOT NULL,
  role         TEXT CHECK (role IN ('owner','admin','user')),
  status       TEXT NOT NULL CHECK (status IN ('pending','confirmed','disabled')),
  approved_at  INTEGER,
  approved_by  TEXT,
  created_at   INTEGER NOT NULL,            -- unix ms, as everywhere
  CHECK (CASE kind WHEN 'room' THEN role IS NULL ELSE (status = 'pending') = (role IS NULL) END),
  CHECK (kind = 'room' OR (approved_at IS NULL AND approved_by IS NULL))
) STRICT;

-- One row per platform alias on one channel instance. A control API client is
-- a binding holaclaw:<alias>; platform is a paired device's (docs/identity.md
-- §3), empty for every other binding.
CREATE TABLE bindings (
  channel      TEXT NOT NULL,               -- channel instance name
  alias        TEXT NOT NULL,               -- platform id as seen on a real message
  identity_id  TEXT NOT NULL REFERENCES identities(id) ON DELETE CASCADE,
  display      TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  platform     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (channel, alias)
) STRICT;
CREATE INDEX bindings_identity ON bindings(identity_id);

-- Static API tokens of the holaclaw channel (docs/identity.md). Only the hash
-- is stored: the plaintext is shown once, at creation, and never again.
-- binding_id is the alias of the token's holaclaw binding. It has no foreign
-- key: unbinding revokes the token and keeps it, so it outlives the binding.
-- origin is how the token was made: pair (origin_ref is the invite), token
-- create, rotate (origin_ref is the token it replaced) or init. created_by and
-- revoked_by name the caller, as events do; empty when nobody is known.
CREATE TABLE api_tokens (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  scope         TEXT NOT NULL CHECK (scope IN ('admin','client')),
  hash          TEXT NOT NULL UNIQUE,       -- hex sha256 of the plaintext
  binding_id    TEXT NOT NULL,              -- holaclaw:<binding_id>
  created_at    INTEGER NOT NULL,
  last_used_at  INTEGER,                    -- refreshed at most once a minute
  revoked_at    INTEGER,
  origin        TEXT NOT NULL DEFAULT 'token_create' CHECK (origin IN ('pair','token_create','rotate','init')),
  origin_ref    TEXT,
  created_by    TEXT NOT NULL DEFAULT '',
  revoked_by    TEXT
) STRICT;
CREATE INDEX api_tokens_binding ON api_tokens(binding_id);

-- One-shot expiring invite codes, stored hashed like a token. An invite
-- targets one channel instance (docs/identity.md §2). A DM invite names its
-- identity and no role: the binding gets the identity's role when it is
-- redeemed. A room invite names no identity. The kinds never redeem for each
-- other: a leaked DM code must not enable a stranger's room, and a room code
-- grants its redeemer nothing. scope is the token scope a holaclaw (pairing)
-- invite grants; NULL everywhere else.
CREATE TABLE invites (
  id          TEXT PRIMARY KEY,
  hash        TEXT NOT NULL UNIQUE,         -- hex sha256 of the normalised code
  kind        TEXT NOT NULL CHECK (kind IN ('dm','room')),
  channel     TEXT NOT NULL CHECK (channel <> ''),
  user_id     TEXT REFERENCES identities(id) ON DELETE CASCADE,
  scope       TEXT CHECK (scope IN ('client','admin')),
  created_by  TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  used_at     INTEGER,
  revoked_at  INTEGER,
  CHECK ((kind = 'dm') = (user_id IS NOT NULL)),
  CHECK (scope IS NULL OR (kind = 'dm' AND channel = 'holaclaw'))
) STRICT;
CREATE INDEX invites_expires ON invites(expires_at);

CREATE TABLE sessions (
  id              TEXT PRIMARY KEY,
  key             TEXT NOT NULL UNIQUE,   -- agent/channel/conversation[/thread]
  agent           TEXT NOT NULL,
  kind            TEXT NOT NULL CHECK (kind IN ('chat','task')),
  user_id         TEXT REFERENCES identities(id) ON DELETE SET NULL,
  origin          TEXT NOT NULL,          -- JSON event.Origin
  title           TEXT NOT NULL DEFAULT '',
  -- Context state: the stable prefix is rebuilt only at compaction.
  prefix_hash     TEXT NOT NULL DEFAULT '',
  summary_msg_id  TEXT NOT NULL DEFAULT '', -- messages before this id are compacted away
  compact_fails   INTEGER NOT NULL DEFAULT 0,
  -- Turn ownership (self-healing on start).
  turn_token      TEXT NOT NULL DEFAULT '',
  run_generation  INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  ended_at        INTEGER
) STRICT;
CREATE INDEX sessions_user ON sessions(user_id);

-- pos is a message's place in the transcript. It is the id, except for a
-- mailbox entry, which takes its place when a turn consumes it: a message that
-- arrived while a turn ran must sort after that turn's answer, or the next
-- turn's history ends with the assistant and the question is never answered.
CREATE TABLE messages (
  id            TEXT PRIMARY KEY,         -- ULID; client-generated ids are idempotency keys
  session_id    TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  trace_id      TEXT NOT NULL,
  role          TEXT NOT NULL CHECK (role IN ('user','assistant','tool','system')),
  text          TEXT NOT NULL DEFAULT '',
  body          TEXT NOT NULL,            -- JSON event.Message (full record)
  -- Mailbox: user messages wait here until a turn consumes them.
  pending       INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  pos           TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX messages_session ON messages(session_id, id);
CREATE INDEX messages_session_pos ON messages(session_id, pos);
CREATE INDEX messages_trace ON messages(trace_id);
CREATE INDEX messages_pending ON messages(session_id) WHERE pending = 1;

CREATE TABLE events (
  id          TEXT PRIMARY KEY,
  trace_id    TEXT NOT NULL,
  span_id     TEXT NOT NULL,
  parent_id   TEXT NOT NULL DEFAULT '',
  session_id  TEXT NOT NULL DEFAULT '',
  type        TEXT NOT NULL,
  time        INTEGER NOT NULL,
  duration_ns INTEGER NOT NULL DEFAULT 0,
  message_id  TEXT NOT NULL DEFAULT '',
  attrs       TEXT NOT NULL DEFAULT '{}',
  schema      INTEGER NOT NULL
) STRICT;
CREATE INDEX events_trace ON events(trace_id, id);
CREATE INDEX events_session ON events(session_id, id);

-- Blob metadata; bytes live in the content-addressed blob directory.
CREATE TABLE blobs (
  id          TEXT PRIMARY KEY,           -- hex sha256
  size        INTEGER NOT NULL,
  mime        TEXT NOT NULL,
  created_at  INTEGER NOT NULL
) STRICT;

CREATE TABLE message_blobs (
  message_id  TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  blob_id     TEXT NOT NULL REFERENCES blobs(id),
  PRIMARY KEY (message_id, blob_id)
) STRICT;
CREATE INDEX message_blobs_blob ON message_blobs(blob_id);

-- Policy state: persisted grants and tool approvals.
CREATE TABLE grants (
  id          TEXT PRIMARY KEY,
  agent       TEXT NOT NULL,
  tool        TEXT NOT NULL,
  pattern     TEXT NOT NULL DEFAULT '',
  scope       TEXT NOT NULL CHECK (scope IN ('session','always','task')),
  session_id  TEXT NOT NULL DEFAULT '',
  task_id     TEXT NOT NULL DEFAULT '',
  created_by  TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
) STRICT;
CREATE INDEX grants_lookup ON grants(agent, tool);

CREATE TABLE approvals (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL CHECK (kind IN ('tool','access')),
  status       TEXT NOT NULL CHECK (status IN ('pending','approved','denied','expired')),
  trace_id     TEXT NOT NULL DEFAULT '',
  session_id   TEXT NOT NULL DEFAULT '',
  agent        TEXT NOT NULL DEFAULT '',
  tool         TEXT NOT NULL DEFAULT '',
  summary      TEXT NOT NULL DEFAULT '',
  args         TEXT NOT NULL DEFAULT '',
  choice       TEXT NOT NULL DEFAULT '',
  resolved_by  TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  resolved_at  INTEGER
) STRICT;
CREATE INDEX approvals_pending ON approvals(status, expires_at);

-- Memory: long-term notes and short-term recall behind one store
-- (docs/memory.md). Provenance is columns written by code, never prose the
-- model can forge; the partition key is mandatory and enforced in the store.
-- tainted marks untrusted content wherever it is stored: a recall entry
-- summarising tainted material is injected wrapped as untrusted, never as a
-- plain line (docs/memory.md, docs/security.md).
CREATE TABLE memory_items (
  id          TEXT PRIMARY KEY,
  agent       TEXT NOT NULL,
  layer       TEXT NOT NULL CHECK (layer IN ('note','recall')),
  text        TEXT NOT NULL,
  about       TEXT NOT NULL DEFAULT '',   -- optional person this is about
  visibility  TEXT NOT NULL CHECK (visibility IN ('shared','origin')),
  -- Provenance (written by code from trusted state).
  "partition" TEXT NOT NULL,              -- one per DM user, one per room
  user_id     TEXT NOT NULL DEFAULT '',
  role        TEXT NOT NULL DEFAULT '',   -- sender role at write time
  session_id  TEXT NOT NULL DEFAULT '',
  trace_id    TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,           -- unix ms
  tokens      INTEGER NOT NULL DEFAULT 0, -- tokens.Estimate(text), for the note budget
  source_hash TEXT NOT NULL DEFAULT '',   -- sha256 of the source text, recall dedupe
  tainted     INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX memory_items_scope ON memory_items(agent, layer, "partition");
CREATE INDEX memory_items_recent ON memory_items(agent, created_at);
CREATE UNIQUE INDEX memory_items_recall_dedupe
  ON memory_items(agent, "partition", source_hash)
  WHERE layer = 'recall' AND source_hash <> '';

-- Full-text index over the item text, external content: the rows live in
-- memory_items and the triggers below keep the index in step.
CREATE VIRTUAL TABLE memory_fts USING fts5(
  text,
  about,
  content='memory_items',
  content_rowid='rowid',
  tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER memory_items_ai AFTER INSERT ON memory_items BEGIN
  INSERT INTO memory_fts(rowid, text, about) VALUES (new.rowid, new.text, new.about);
END;
CREATE TRIGGER memory_items_ad AFTER DELETE ON memory_items BEGIN
  INSERT INTO memory_fts(memory_fts, rowid, text, about) VALUES ('delete', old.rowid, old.text, old.about);
END;
CREATE TRIGGER memory_items_au AFTER UPDATE ON memory_items BEGIN
  INSERT INTO memory_fts(memory_fts, rowid, text, about) VALUES ('delete', old.rowid, old.text, old.about);
  INSERT INTO memory_fts(rowid, text, about) VALUES (new.rowid, new.text, new.about);
END;

-- One vector per item, tagged with the embedding model that produced it: a
-- model swap leaves the item pending until Reindex re-embeds it.
CREATE TABLE memory_vectors (
  item_id  TEXT PRIMARY KEY REFERENCES memory_items(id) ON DELETE CASCADE,
  model    TEXT NOT NULL,
  dims     INTEGER NOT NULL,
  vec      BLOB NOT NULL                  -- little-endian float32
) STRICT;
CREATE INDEX memory_vectors_model ON memory_vectors(model);

-- Embedding cache keyed by (provider, model, sha256(text)) with the
-- dimensions recorded, so a re-embed of unchanged text is free.
CREATE TABLE embedding_cache (
  provider    TEXT NOT NULL,
  model       TEXT NOT NULL,
  hash        TEXT NOT NULL,              -- sha256 hex of the embedded text
  dims        INTEGER NOT NULL,
  vec         BLOB NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (provider, model, hash)
) STRICT;

-- Small per-instance state of channel adapters (update offsets, cursors).
CREATE TABLE channel_state (
  channel     TEXT NOT NULL,
  key         TEXT NOT NULL,
  value       TEXT NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (channel, key)
) STRICT;

-- Keys of inbound messages already handled, so a message is routed at most
-- once even when it comes back after a restart (#101): a WhatsApp resend has
-- the same id but new ciphertext and maybe another addressing, and Slack and
-- Google Chat redeliver too. Not channel_state: that is a small per-channel
-- key/value table, and this one grows with traffic until pruned. A key is
-- stored as its SHA-256: only equality is needed, so rows stay one size
-- whatever id a client picks, and the phone numbers and LIDs of strangers in
-- WhatsApp keys are not kept in clear for 14 days.
CREATE TABLE inbound_seen (
  channel  TEXT NOT NULL,     -- channel instance
  key      BLOB NOT NULL CHECK (length(key) = 32), -- sha256 of the adapter's key
  seen_at  INTEGER NOT NULL,  -- unix ms of the first sighting
  PRIMARY KEY (channel, key)
) STRICT, WITHOUT ROWID;
CREATE INDEX inbound_seen_at ON inbound_seen(seen_at);

-- The platform messages core sent (Receipt.PlatformMsgIDs), so a reaction can
-- be routed by its target: an approval prompt answers the approval, another
-- bot message becomes a line in its session (docs/channels/whatsapp.md §6).
-- Adapters qualify their ids with the conversation ("<chat>:<id>"), so an id
-- is unique per channel instance and a reaction is routed by it alone, not
-- by a session key: a Telegram reaction carries no topic. Rows go with their
-- session, and with events under retention.events_days.
CREATE TABLE sent_messages (
  channel          TEXT NOT NULL,
  platform_msg_id  TEXT NOT NULL,
  conversation     TEXT NOT NULL,
  session_id       TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  message_id       TEXT NOT NULL DEFAULT '',   -- transcript message; empty for an approval prompt
  approval_id      TEXT NOT NULL DEFAULT '',   -- set when the message is an approval prompt
  created_at       INTEGER NOT NULL,
  PRIMARY KEY (channel, platform_msg_id)
) STRICT;
CREATE INDEX sent_messages_session ON sent_messages(session_id);
CREATE INDEX sent_messages_created ON sent_messages(created_at);

-- Detached agent runs (docs/tasks.md). A task run lives in its own session
-- (kind = task, key <agent>/task/<task id>); scoped grants live in `grants`
-- with scope = 'task' and task_id = tasks.id.
CREATE TABLE tasks (
  id             TEXT PRIMARY KEY,
  agent          TEXT NOT NULL,
  name           TEXT NOT NULL DEFAULT '',
  prompt         TEXT NOT NULL,
  schedule_kind  TEXT NOT NULL CHECK (schedule_kind IN ('cron','at','now')),
  cron           TEXT NOT NULL DEFAULT '',
  at             INTEGER,                    -- unix ms, schedule_kind = 'at'
  delivery_kind  TEXT NOT NULL CHECK (delivery_kind IN ('session','channel','store')),
  delivery       TEXT NOT NULL DEFAULT '{}', -- JSON: origin session id and its trusted origin
  missed         TEXT NOT NULL DEFAULT 'skip' CHECK (missed IN ('skip','run_once')),
  max_steps      INTEGER NOT NULL DEFAULT 0,
  status         TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','done','failed','cancelled')),
  next_run       INTEGER,                    -- unix ms; NULL when nothing is scheduled
  last_run       INTEGER,
  last_trace_id  TEXT NOT NULL DEFAULT '',
  parent_trace   TEXT NOT NULL DEFAULT '',   -- trace that created the task (task.create)
  created_by     TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL
) STRICT;
CREATE INDEX tasks_due ON tasks(status, next_run);

-- Daily usage counters for capped, paid operations (first user: image
-- generation, docs/security.md). One row per agent, counter and UTC day; old
-- rows are harmless and pruned with the trace events.
CREATE TABLE usage_daily (
  agent  TEXT NOT NULL,
  key    TEXT NOT NULL,
  day    TEXT NOT NULL,                      -- UTC, YYYY-MM-DD
  n      INTEGER NOT NULL,
  PRIMARY KEY (agent, key, day)
) WITHOUT ROWID;

-- Cache of what each connector's remote MCP server told us (docs/connectors.md
-- §3, §5). The intent lives in connectors.toml and the tokens in auth.toml;
-- this row only remembers. The open OAuth intent (PKCE verifier, state) is
-- never stored: it lives in the daemon's memory for its 15 minutes.
CREATE TABLE connectors (
  id              TEXT PRIMARY KEY,                     -- connectors.toml id
  state           TEXT NOT NULL CHECK (state IN ('pending','authorizing','connected','degraded','expired')),
  detail          TEXT NOT NULL DEFAULT '',             -- closed set of daemon strings, never server text
  server_name     TEXT NOT NULL DEFAULT '',
  server_version  TEXT NOT NULL DEFAULT '',
  session_id      TEXT NOT NULL DEFAULT '',             -- Mcp-Session-Id
  client_id       TEXT NOT NULL DEFAULT '',             -- the granted client id, of any kind
  cimd_refused    INTEGER NOT NULL DEFAULT 0 CHECK (cimd_refused IN (0, 1)),
  auth_meta       TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(auth_meta)), -- discovered metadata
  tools           TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(tools)),     -- tools/list as the server sent it
  scopes          TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(scopes)),    -- granted scopes
  last_check      INTEGER,                              -- unix ms; NULL before the first check
  failures        INTEGER NOT NULL DEFAULT 0,           -- consecutive failed checks
  token_expiry    INTEGER,                              -- unix ms of the access token; NULL for an open server
  updated_at      INTEGER NOT NULL
) STRICT;
