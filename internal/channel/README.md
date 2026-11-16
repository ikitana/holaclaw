# Channel adapters

`channel.go` is the contract (ARCHITECTURE.md §2, §6, docs/channels); one package per platform implements it. This file
records how holaclaw does channels and why, what every adapter must do, and notes for channels not built yet.

## How holaclaw does it

- **One bot per instance, not per agent.** A channel instance is one bot account on one platform, with its
  own credentials in `auth.toml` (`holaclaw auth set channels.<name> bot_token`), state store, lifecycle and
  access knobs. It serves every user and conversation that bot is in, so talking to three people on
  Telegram is one instance and one token. The instance is not tied to one agent: it can serve several
  (say marketing, dev and finance) through the same bot. Its `agents` list says which agents are reachable
  on it, and leaving it out means every agent; the router picks the agent for each conversation among that
  list (ARCHITECTURE.md §2; v1 runs one agent). The token, access knobs and `reply` are configured once per
  instance and apply to every agent it serves; what each agent may do stays in its `agent.toml`, which
  never names channels. Adding an agent never means registering another bot or setting another token, only
  adding its name to `agents` where it should be reachable. The table key is the instance name and `type`
  defaults to it, so the usual config is one table per platform:

  ```toml
  [channels.telegram]              # type = "telegram"
  agents = ["marketing", "dev"]    # marketing listens only here if no other instance lists it
  [channels.telegram.access]
  rooms = "approved"
  ```

  Add a second instance of a type (`[channels.slack-acme]` with `type = "slack"`) only for a second bot
  account, such as another Slack workspace or another WhatsApp number. A token belongs to exactly one
  instance: two pollers on one Telegram token get `409 Conflict` from `getUpdates`, and Slack splits Socket
  Mode events across connections. One bad instance never stops the others. There is no interactive login
  in v1: `POST /channels/{name}/login` answers `501`.
- **One envelope.** Every platform message becomes one `event.Envelope`: a sender (a set of `Aliases`, an
  untrusted `DisplayName`, `FromSelf`, `Mentioned`) and an origin (`ConversationKind` = `dm` | `room`,
  conversation id, optional `Thread` and `ThreadRoot`, `PlatformMsgID`). Core never sees platform types.
- **Two conversation kinds plus a thread.** Slack channels, Discord channels and Chat spaces are `room`, and so are group DMs (Slack `mpim`, Chat `GROUP_CHAT`). Adapters classify by the platform's conversation type, never by counting participants, and an unknown type is `room`.
  A thread is a field, not a kind: it never changes access rules, it only narrows the session.
- **Store, then acknowledge.** `Inbound` returns only after the envelope is durably stored; only then does
  the adapter advance its platform offset or ack. `PlatformMsgID` is the idempotency key. A restart replays
  stored input instead of losing it.
- **Identities, owned by core.** Channels feed the identity model in
  [docs/identity.md](../../docs/identity.md) (PR #17); they do not define it. An **ID** is one account on
  one channel instance, written `<channel>:<alias>`, where the alias is what the adapter puts in
  `Sender.Aliases`. A **user identity** is a person: a name, a role and any number of IDs, so the same
  person on Telegram, Slack and the CLI is one identity. A **room identity** is one room (Telegram group,
  Slack channel, Chat space) with exactly one ID, the instance plus the conversation id; threads belong to
  their room. A sender the access knobs admit but nobody has confirmed is a **pending** identity.
- **Isolated sessions.** The session key is `<agent>/<channel>/dm/<identity>` or
  `<agent>/<channel>/room/<conversation>[/<thread>]`: per agent, per person per instance, per room, per
  thread, so agents sharing an instance never share a session. There is no shared-DM mode, so two people
  never share context by accident. A person on two channels has two sessions and, per agent, one memory
  (docs/memory.md).
- **Access from real messages.** Two knobs per instance, shared by every agent it serves: `dm` (`invite` |
  `open`, default `invite`) and `rooms` (`off` | `approved` | `open`, default `off`),
  plus `reply` (`mention` | `always`, default `mention`) for *when* the bot answers and `reply_to` (see
  Delivery) for *how*. No allow-lists and no hand-typed platform ids:
  IDs are captured from real messages and live in the DB (docs/identity.md).
  - `invite`: an owner or admin mints a one-shot expiring code for an existing identity on one instance;
    redeeming it on the platform binds the sender's ID to that identity directly, with its role.
  - `open`: unknown senders become rate-limited pending identities that read agent memory only, under the
    agent's `policy.pending` (read only by default). `holaclaw check` refuses open access while that policy
    allows `exec`, `write` or `external` (ACC001) and warns on `reply = "always"` with `rooms = "open"`
    (ACC002).
  - `approved` rooms are approved by an admin, automatically when a known admin adds and mentions the
    bot, or by a room invite: a one-shot code bound to the instance (Telegram `startgroup` deep link)
    that approves the room it is redeemed in and links nobody. The first accepted message creates a
    pending room identity. Approving a room says nothing about its members: a linked person talks as
    themselves, everyone else is pending.

  The knobs decide who is answered; confirming or assigning decides who an account *is*. That stays admin
  work because the ID inherits the memory and the role of the identity it is linked to.
- **Delivery.** Typing indicator while the run works, then one final message: no streaming edits. The
  adapter splits and formats text, degrades attachments it cannot deliver into `Receipt.Degraded` (core
  emits `delivery.degraded` and a text notice), and
  offers typing, buttons and reactions through optional capability interfaces (`Typer`,
  `ApprovalPrompter`, `Reactor`).
- **Reactions.** Adapters only translate; core decides what a reaction means. Out, the agent's `react`
  tool calls `Reactor.React` with the turn's inbound message as `target.PlatformMsgID` (core sets it,
  never the model); `Limits.Reactions` lists the emoji the platform takes (nil = any), compared without
  U+FE0F and sent in the list's form. In, a reaction is an `Envelope` with `Reaction` set and no
  content: `Origin.PlatformMsgID` is the reaction event's own unique id (the router dedupes on it),
  `Reaction.TargetMsgID` is the reacted message in exactly the form `Receipt.PlatformMsgIDs` returns,
  qualified so it is unique per instance (`<chat>:<id>`), and `Emoji` is empty when `Removed`. Core keeps
  the ids it sent and routes by that id alone: a reaction to an approval prompt answers it, one to another
  bot message becomes a line in that session (`[👍 on: …]`) that starts no turn, anything else is
  dropped, and so is a stranger's reaction, with no invite redeemed and no guest made. A channel with `Reactor`
  and no `ApprovalPrompter` gets its approval prompts as text that ends with how to answer
  (👍 once · 🔁 session · ✅ always · 👎 deny); the first owner or admin reaction wins and core replies to
  the prompt with who answered.
- **Reply-to.** An answer is a platform-native reply to the message that started the turn, so a busy room
  can tell which question it answers. `reply_to` sits next to `reply` in `access`: `rooms` (default)
  in room conversations only, `always` in DMs too, `off` for plain messages. Core decides: it sets
  `Outbound.Target.PlatformMsgID` to the triggering message when the setting applies, and clears it
  otherwise. The id comes from the turn trace's `channel.received` event, so it is always the inbound
  envelope, never model input (the session row only keeps the origin of its first message). Notices and
  task results are plain. The adapter marks only the first message it sends (or keeps the whole answer
  in the reply's thread, where that is the platform's reply), and when the message is gone it still
  delivers, as a plain message.

  | Platform | Native reply | Message gone |
  |---|---|---|
  | Telegram | `reply_parameters.message_id` on `sendMessage` and media sends, first message only | `allow_sending_without_reply: true`; a refused reply is resent without it |
  | Slack | A thread rooted at the mention (`thread_ts`) for the whole answer; inside a thread, the answer stays in that thread | `thread_not_found` resends to the channel |
  | Google Chat | `quotedMessageMetadata` on the first message, read with `messages.get` for its `lastUpdateTime`; the answer stays in the user's thread | A failed read or a refused quote sends a plain message |
  | WhatsApp ([docs](../../docs/channels/whatsapp.md)) | The quoted message (`ContextInfo.StanzaID`, `Participant`, `QuotedMessage`), to land with the adapter | Plain message |

  An adapter without a reply primitive ignores `PlatformMsgID` on send.
- **Extension.** Adapters in this tree are the only channels; there are no plugins.

## What every adapter does

Derived from the contract and the three adapters that exist; a new package must match all of it.

- **Inbound**: build one `Envelope` per platform message; set `PlatformMsgID` so the router can dedupe
  and core can name it as the message an answer replies to; set `Thread` only when the user wrote in a
  thread (a Slack top-level mention stays in the channel session, as in Google Chat), and `ThreadRoot` to
  the root message's `PlatformMsgID` when a thread starts on one (Slack), so the new thread session is
  seeded with the exchange that message started;
  return from `Inbound` only after it says the envelope is stored, then advance the platform offset
  (Telegram: `offset` key in the `StateStore`). Adapter-side dedupe covers platform re-delivery inside one
  poll or socket (Slack keys on `event_id` and `channel:ts`).
- **Sender**: every alias the platform has for the account goes in `Aliases`; `DisplayName` is untrusted,
  the router sanitises it and admins see it on pending identities; `FromSelf` on the bot's own messages.
  `Mentioned` when the bot was addressed. Aliases come only from the platform's authenticated sender
  fields (Telegram `from.id` and `from.username`, Slack `team` + `user`), never from message text. The
  adapter never decides who a person is: the router resolves IDs to an identity, and an ID is linked only
  by an invite, a confirm or an assign by an owner or admin (docs/identity.md §2). Telling the agent "I am
  Alice" changes nothing.
- **Secrets**: tokens are read through a func per request, never stored in a field, never logged, and
  scrubbed out of every error (Go's `http` errors quote the URL, and Telegram's URL carries the token).
- **Errors**: one typed `Error` per package with a closed kind set and `errors.Is` sentinels, `RetryAfter`
  from 429 honoured, `KindAuth` turns the instance `logged_out`.
- **Outbound**: `Limits()` is truthful; text is split by the adapter (Telegram 4096 / caption 1024, Slack
  ~4000); markdown is converted per platform (Telegram HTML, Slack mrkdwn); an undeliverable attachment
  goes into `Receipt.Degraded`, never an error for the whole reply; bounded send retries; a set
  `Target.PlatformMsgID` becomes the platform's native reply (see Reply-to).
- **Lifecycle**: `Run` reconnects on its own with capped backoff and reports every transition through the
  status func; one bad instance never stops the others.
- **Tests**: `httptest` fakes only, a redaction test proving the token is absent from errors and logs,
  failure paths for every error kind.

## Google Chat

Built in `internal/channel/googlechat`. The spec and the operator setup guide are
[docs/channels/googlechat.md](../../docs/channels/googlechat.md) and
[docs/guides/googlechat.md](../../docs/guides/googlechat.md); docs/channels has the
one-paragraph adapter notes. In short: a Pub/Sub pull loop plus the Chat REST API, both authenticated with
one service-account key; no public URL or listening port; no file upload (attachments degrade to a text
notice); no typing indicator; spaces only ever deliver @-mentions, so `reply = "always"` cannot work there
(`holaclaw check` ACC003).

## WhatsApp

Built in `internal/channel/whatsapp`. The spec and the operator setup guide are
[docs/channels/whatsapp.md](../../docs/channels/whatsapp.md) and
[docs/guides/whatsapp.md](../../docs/guides/whatsapp.md). In short: holaclaw is a linked device
of a WhatsApp account through whatsmeow, in-process, with no public URL.
whatsmeow's session database (`<data_dir>/whatsapp/<name>.db`, 0600) is the credential, so `auth.toml` holds
nothing for it; linking is a pairing code the daemon requests (`channel.Linker`). Acks wait for `Inbound`, a
sender's phone number and LID come only from the stanza (`wa:+…`, `wa:lid:…`), replies go out in WhatsApp's own
markup split at 4000 characters, there are no buttons, and the adapter owns reconnects (whatsmeow's own are off).

## Open items

- **Config and code follow this file.** ARCHITECTURE.md §4, docs/identity.md already describe the model above, but the
  parser, the generated docs/reference/config.md, the quick start in the top-level README.md and
  deploy/example/channels.toml still require `channels.<name>.agent` and `type`. To follow: replace
  `agent` with an optional `agents` list (absent = every agent; `check` fails on a name with no agent),
  default `type` to the instance name, and run ACC001 against every agent the instance serves. v1 is
  unreleased, so this is a schema edit, not a migration step. How to pick an agent per conversation among
  those an instance serves (per room, or by command in a DM) is designed along with multiple agents, not
  now.
- **Bot senders.** `Sender` has `FromSelf` only; another bot in a room is treated as a person. Proposal:
  mark them with `Sender.Bot bool` so the router can tell them apart (dropped by default, traced as
  `router.dropped`), which also guards against reply loops. Contract change in `internal/event`.
- **Outbound durability.** Adapters retry inline and inbound replays after a restart, but a reply produced
  and not yet sent when the daemon restarts is not covered. Verify before claiming otherwise.
- **Shared send error kinds.** Each adapter has its own kind set. A shared one in this package (e.g.
  `too_long | bad_format | forbidden | not_found | rate_limited | transient`) would make degradation
  uniform, including retrying `bad_format` as plain text.
