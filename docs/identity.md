# Identities

Who the agent is talking to. This document covers identities, their bindings
and the tokens that sign clients in: how each is created, confirmed, assigned
and removed, and which sessions each one sees.

It describes the target design. §10 lists what differs from the code today.
Memory scoping has its own document, [memory.md](memory.md). Sessions are
in [sessions.md](sessions.md).

## Contents

1. [Concepts](#1-concepts)
2. [Lifecycle](#2-lifecycle)
3. [Apps, devices and tokens](#3-apps-devices-and-tokens)
4. [Sessions and what each token sees](#4-sessions-and-what-each-token-sees)
5. [Memory](#5-memory)
6. [CLI and API](#6-cli-and-api)
7. [Skills](#7-skills)
8. [Events](#8-events)
9. [Out of scope](#9-out-of-scope)
10. [Changes from today](#10-changes-from-today)
11. [Future additions](#11-future-additions)

## 1. Concepts

### Identity, binding and token

The daemon stores three things:

- **An identity is *who* the agent talks to.** A user identity stands for a
  person (Carla); a room identity stands for a room (the #ops channel).
- **A binding is *where* an identity's messages come from.** One alias on one
  channel instance, written `<channel>:<alias>`: `telegram:12345678`,
  `slack:U024BE7LH`, `holaclaw:01JC9F…`.
- **A token is *how* a `holaclaw` binding proves itself.** Telegram and Slack
  authenticate their own bindings; for the `holaclaw` channel (the CLI, the
  apps, automations) the daemon is the platform, and tokens are the
  credentials.

**A binding belongs to one and only one identity, at all times**, and a token
to one and only one binding. A binding is created on an identity and removed
from it; it never exists alone. Messages arrive from a binding, and the daemon
resolves it to its identity. From then on, only the identity matters.

| | Identity | Binding | Token |
|---|---|---|---|
| Answers | Who is this? | Where did this come from, and where does the reply go? | Is this `holaclaw` client allowed in? |
| Properties | name, kind (`user` or `room`), role, status (`pending`, `confirmed` or `disabled`) | channel, alias, display name, creation date | name, scope, origin, creator, last use, revocation |
| Holds | role, memory, sessions, tasks, invites, bindings | tokens (`holaclaw` bindings only) | nothing |
| Created by | an admin (`identity create`), or the daemon from an unknown sender (pending) | an invite, a pairing, `token create`, `init`, or a second alias in the same message; or with a pending identity | a pairing, `token create`, `init` |
| Removed by | `identity delete`, or automatically when pending and left with no bindings | `identity unbind`, or with its identity | never deleted: `token revoke`, or revoked with its binding |
| Trust | names and roles are set by admins; a name never comes from the platform | the alias is authenticated by the platform, or by a token; the display name is never trusted | a secret shown once and stored hashed |

The rules:
- **A binding grants everything its identity holds.** So only these can
  create one on an existing identity: an owner or admin, a code one of them
  minted, or a client already signed in to that identity, which can pair
  another device for it (§3).
- **Removing a binding removes nothing else.** Memory, sessions and role stay
  with the identity. The next message from that alias is an unknown sender.
- **Bindings move only out of a pending identity onto a confirmed one**, by
  assign (§2). Never between confirmed identities, never between pending ones.
- **A pending identity left with no bindings is deleted**, with its memory.
  A confirmed identity is never deleted implicitly: with no bindings it keeps
  its memory, sessions and role.
- **Two identities are never merged** by anything the daemon infers (§2).
- **Revoking a token leaves its binding**; removing a binding revokes all its
  tokens. A binding with no active token is valid, and revoked tokens are
  kept for traceability.
- **A disabled identity keeps everything and can do nothing.** Its bindings,
  tokens, memory and sessions stay, but none of its bindings is answered and
  none of its tokens is accepted until it is enabled again (§2).

### Resolving a message or request

Every inbound message and every API request goes through the same steps, in
one place:

```
1. binding    the message's alias, or the token's binding; none: unknown sender (§2)
2. identity   the binding's identity
3. status     disabled: the message is dropped, the request gets 403 identity_disabled
4. role       the identity's role; a pending identity runs under the guest policy block
5. scope      holaclaw only: the lower of the token's scope and the role
```

Nothing is cached between messages, so a new status or role applies to the
next message or request. The role is its own step, so another source can
compute it later without touching the others (§11).

A message in a room resolves twice, the room first:

```
room     the room's binding and its room identity; not approved or disabled: dropped
sender   the five steps above, for the sender's alias
```

A room adds no role and takes none away: in a room, every sender acts as its
own identity with its own role. A future rule on who may talk in a room
(§11) goes between the two steps.

### Examples

```
identity angel · user · role owner · confirmed
├── telegram:87654321          "Angel M."
├── holaclaw:cli               the unix socket: no tokens
├── holaclaw:01JA0…            "init"
│   └── token 01JA1…  admin    origin init                      active
└── holaclaw:01JE2…            "iPhone 16"
    └── token 01JE3…  client   origin pair · invite 01JE1…      active

identity carla · user · role user · confirmed
├── telegram:12345678          "Carla R."
├── wa:+34600000000            ┐ one WhatsApp account, two aliases,
├── wa:lid:123456789           ┘ bound from the same message
├── holaclaw:01JC9F…           "iPhone 15"
│   └── token 01JK1…  client   origin pair · invite 01JC8…      active
└── holaclaw:01JD1K…           "automations"
    ├── token 01JM4…  client   "ci"      origin token create    active
    ├── token 01JM7…  client   "backup"  origin token create    revoked (rotated)
    └── token 01JN2…  client   "backup"  origin rotate          active

identity guest-x7k2qm · user · pending        runs under the guest policy
└── telegram:55501234          "dan_k"

identity ops · room · confirmed
└── slack:C024BE91L            "#ops"

identity bob · user · role admin · confirmed  created, invite not redeemed yet
(no bindings)
```

### Other terms

| Term | Meaning |
|---|---|
| **User identity** | Holds a role and any number of bindings. |
| **Room identity** | Holds exactly one binding, the channel instance plus the platform's conversation id (`telegram:-1001234567`), and no role. Threads belong to their room identity. A room is a conversation, not the people in it: it never holds or grants a role. A set of people that grants a role is a role group (§11). A DM with several people is a room (§2). |
| **Pending** | An identity the daemon created for an unknown sender, holding the bindings that message carried. It has no role and runs under the agent's `guest` policy block until an admin confirms it or assigns it. It is named `guest-<short id>` (§2). |
| **Confirmed** | An identity an admin created or confirmed, and the owner. |
| **Disabled** | A confirmed identity or a room an admin switched off. It keeps its bindings, tokens, memory and sessions, and nothing it sends is answered or accepted. |
| **Owner** | The user identity created by `holaclaw init`. It can't be recreated, updated or deleted. |
| **Channel instance** | One configured connection: `telegram-team`, `slack`. The built-in `holaclaw` instance serves the CLI, the apps and automations, and no configured instance can take its name. |
| **Unknown sender** | A message whose aliases match no binding. |
| **Scope** | What a token may call: `client` (the default) or `admin`, never above the identity's role. |

People, platform accounts and devices exist outside the daemon. This document
only uses those words to explain what an identity, a binding or a token
stands for. A **room** is a platform's group conversation; it appears in the
access knobs, which decide whether a room is answered before it has a room
identity. "Group" is the platform's word (a Telegram group) and is never used
for the daemon's own concept: the knob is `rooms`, the conversation kind and
session key segment are `room`, and in this daemon a room is a room identity.

Roles are `owner`, `admin` and `user`. Owner and admin manage identities;
only the owner acts on the owner.

## 2. Lifecycle

```
 holaclaw init ──────────────────────────────▶ owner (confirmed, permanent)

 identity create ───────────────────────────▶ confirmed, no bindings
                                                │  ▲
 unknown sender ──▶ pending ──── confirm ───────┘  │
  (dm / rooms        │    └───── assign ───────────┘ bindings move,
   knobs)            │                               pending deleted
                     └── delete, or last unbind ──▶ deleted

 confirmed ── invite, pairing, token create ──▶ + binding
 confirmed ── unbind ──────────────────────────▶ − binding (identity stays)
 confirmed ◀─ disable / enable ───────────────▶ disabled (everything stays;
                                                 a room also from pending,
                                                 and back to pending)
 confirmed ── identity delete ─────────────────▶ deleted (never the owner)
```

### Bootstrap: the owner

`holaclaw init` creates the owner, and it is the only way to. The API has no
call for it, and `init` runs once: after the owner exists, it can't be
recreated, updated or deleted. `init` creates the owner with `holaclaw:cli`,
the binding the unix socket arrives as. With `--token-file`, it also creates
an `init` binding with one `admin` token and writes the token to that file.
The owner pairs its devices afterwards, with a `holaclaw` invite minted on
the host (§3). Until the
owner exists, the daemon answers every API request with `503 not_initialised`.
The config `init` writes (`--provider`) is covered in
[deployments/README.md](deployments/README.md#2-choose-a-provider).

### Creating an identity

An admin creates a confirmed user identity, then invites it on a channel:

```
holaclaw identity create --name Carla --role user --invite telegram
holaclaw invite create --identity <id> --channel holaclaw   # the id printed above
```

The identity exists right away with no bindings. `--invite` names one channel
and prints the first invite for it. Every other channel gets its own invite
with `holaclaw invite create`.

### Invites

An invite is a one-shot code that creates one binding on an **existing**
identity. It never creates an identity and never carries a role: the binding
gets the identity's role.

- **One channel instance.** `--channel` is required, and the code works
  nowhere else. Take the identity's id from `holaclaw identity list`:
  ```
  $ holaclaw invite create --identity <id> --channel telegram
  code     7KQ2M9XA
  channel  telegram
  expires  2026-11-10 12:15:00Z
  link     https://t.me/holaclaw_bot?start=7KQ2M9XA
  [QR code of the link]
  ```
  Each adapter prints its own link (Telegram `start` links, WhatsApp `wa.me`
  links) with a QR code of it. A platform without links, or an instance not
  connected yet, gets a one-line instruction instead ("Send this code to the
  bot in a direct message."). On `holaclaw`, the code pairs a device (§3).
- **TTL.** The default is 15 minutes and the cap is 1 hour. An invite onto an
  identity with no bindings yet may go up to 24 hours. A longer TTL is
  refused, not shortened.
- **Redeeming.** On a channel, the code is sent to the bot as `/start CODE`
  or by itself; the message is a credential and is never routed. On
  `holaclaw`, only through `POST /v1/pair` (§3).
- **Redeemed by a pending identity:** it is assigned onto the invited identity
  in the same transaction. A code sent from a binding of a confirmed identity
  is refused.
- **Rate limits.** Failed redeems are limited per alias, and pairing per
  source address.
- **Owner.** Only the owner mints invites for the owner.

**Room invites** approve a room and create no binding on any user identity:
`holaclaw invite create --room --channel telegram` (Telegram's `startgroup`
link). They are redeemed in the room, whatever the `dm` knob says, with the
invite's creator as the approver. DM and room codes never redeem for each
other. A wrong code in a room that isn't approved gets no reply.

### Unknown senders: the access knobs

Every inbound message resolves its aliases against the bindings. When nothing
matches, the channel's knobs decide:

| `dm` knob | Unknown sender |
|---|---|
| `invite` (default) | Dropped, unless the message carries an invite code for this channel. |
| `open` | A pending identity is created and answered. |

| `rooms` knob | The room | Unknown senders in it |
|---|---|---|
| `off` (default) | Dropped. | |
| `approved` | Answered once approved: by an admin, by a room invite, or automatically when an owner or admin adds the bot or mentions it. | Pending user identities, answered like `open`. |
| `open` | Answered. | Same. |

- The first accepted message in a room creates its pending room identity.
- `reply = mention | always` decides which room messages start a turn. The
  others are stored for context. With `always`, the model may answer
  `NO_REPLY`.
- Approving a room is a decision about the room identity, not about the
  senders in it: a known binding talks as its identity, an unknown sender is
  pending.
- A room keeps when it was approved and by whom (`approved_at`,
  `approved_by`, shown by `identity show`). The approver is a record, not a
  link: it stays when that identity is deleted.
- Creating pending identities is rate-limited per alias.

The `holaclaw` channel has no knobs: a request without a valid token is
refused.

### Names

**An identity's name never comes from the platform.** A display name is chosen
by whoever owns the account, and an identity's name reaches the model, as the
person who taught a skill. Admins give real names.

- Every pending user identity is named `guest-<short id>`, the lowercase last
  six characters of its id: `guest-x7k2qm`. This covers DMs under `open` and
  senders in rooms.
- The display name goes on the binding only, sanitized: what `identity show`
  lists, so an admin recognises the person.
- `identity confirm --name` sets a real name, and `identity set --name`
  renames later.
- A room is named after its conversation id until it is confirmed.

### Confirming

An admin confirms a pending user identity by giving it a name and a role, or
a pending room identity by giving it a name. Its bindings stay. Confirming
decides what a binding's messages can read and do, so only an owner or admin
does it.

```
holaclaw identity confirm 01JP5… --name Dan --role user
holaclaw identity confirm 01JP7… --name ops            # a room identity
```

Once confirmed, `identity set` changes the name or the role. A new role
applies to every binding on the next message or request. A demotion lowers the
identity's `admin` tokens to `client`; a promotion leaves them at `client` until
raised one by one (§3):

```
holaclaw identity set dan --role admin
holaclaw identity set ops --name platform-ops
```

### Assigning

When a pending identity turns out to be an existing confirmed one (a new
Telegram binding that is really Carla), an admin assigns it with
`identity assign <pending> --to carla`. In one transaction, its bindings move to the target, its memory is re-owned by the
target ([memory.md](memory.md)), its DM sessions are ended (never rekeyed),
and the pending identity, now with no bindings, is deleted. Room identities
are never assigned, and only the owner may assign onto the owner.

### Unbinding

`identity unbind <channel>:<alias>` removes one binding from its identity;
the ref alone finds it, on any channel. A confirmed identity keeps
its memory, sessions and role, even with no bindings left. A pending identity
left with no bindings is deleted. A room's one binding can't be unbound:
disable the room or delete it. Unbinding a `holaclaw` binding revokes every
token on it (§3).

### Disabling

`identity disable <i>` switches off a confirmed identity without losing
anything; `identity enable <i>` switches it back on. While disabled:

- messages and reactions from its bindings are dropped without a reply. They
  never create a pending identity, because the bindings are still held;
- its approval buttons are ignored, and it can't approve a room, by message or
  with a room code;
- its tokens are refused with `403 identity_disabled`, but not revoked, and no
  new token is minted for it;
- its tasks don't run, invites for it are refused, and invites it created
  can't be redeemed. They stay unused, and work again once it is enabled;
- its bindings can still be unbound, its tokens revoked, and the identity
  deleted.

What was already running stops when it is disabled:

- its event streams and browser live views close, and a browser it had taken
  over is handed back to the agent;
- its running turns are aborted with no reply: those of its own sessions (a
  room's, for a room), and a room turn that answers only it. A room turn
  that also answers someone else finishes, with the disabled identity
  counted as a guest;
- messages it sent before it was disabled that haven't been answered yet are
  dropped when their turn comes.

Enabling brings none of it back: clients reconnect, and whatever was dropped
has to be sent again.

Enabling applies the missed-run policy to its tasks before it reads as enabled,
as a restart does for the daemon's downtime: `skip` forgets what came due
meanwhile, `run_once` runs one.

Only an owner or admin disables, and the owner is never disabled. A pending
user identity isn't disabled: it is deleted, or unbound, which deletes it.

A room can be disabled pending or confirmed, since a room is normally pending.
Enabling returns it to `pending`, whatever it was before, keeping its name; its
approval doesn't change, so an approved room is answered again. Nothing records
the earlier status. While a room is disabled, approving it is refused. A task
belongs to its creator, not to the room it reports to, so a disabled room still
receives the deliveries of an enabled user's tasks.

### Deleting

Deleting removes a confirmed identity: its tasks are cancelled, its bindings
and invites go, its tokens are revoked, and its sessions stay with no
identity. Its own memory is forgotten unless `--keep-memory` is passed. The
owner is never deleted. The next message from any of its old aliases is an
unknown sender.

### Other cases

These rules only matter on some platforms: the first two where a message
carries more than one alias, the third where a DM can have several people.

#### Several aliases in one message

WhatsApp sends the phone number (`wa:+34600000000`) and the LID
(`wa:lid:123456789`); Google Chat sends a user id and an email. When a message
resolves to an identity, its other aliases that no binding holds become
bindings of that identity, in the same transaction.

- Only the aliases of one message count. Two identities are never merged: two
  messages are a guess, and a wrong guess hands one identity's sessions and
  memory to another. Joining them stays an admin's call (`identity assign --to`).
- An alias that another identity's binding holds is left alone, and the
  conflict is logged.
- Adapters send only stable aliases the platform authenticated: Telegram's
  numeric id, never the `@username`.

#### Recycled phone numbers

A channel declares its **alias namespaces**: prefixes of which an identity
holds at most one binding. WhatsApp declares `wa:` and `wa:lid:`. An alias
belongs to the longest prefix it matches.

If a message matches an identity that already holds a *different* binding in
the namespace of one of the message's aliases (the identity has
`wa:lid:987`, the message brings `wa:lid:111`), the match is not trusted.
Carriers recycle phone numbers: a known number arriving with a new LID is more
likely someone else.

- The next matching identity is tried. With none left, the message is an
  unknown sender, and only the aliases no binding holds go to a new pending
  identity.
- Under `invite`, a code is refused while another identity holds the number.
  An admin unbinds it first.
- The guard sees one message at a time. A message carrying only the old number
  still resolves to the old identity, so the guard narrows the risk without
  closing it.

#### Group DMs

Slack, Google Chat and Teams let a DM have several people (a Slack group DM,
a Google Chat group chat, a Teams group chat). It is a room, not a DM: more
than one identity reads it, so it can never use anyone's `dm/` partition.

- It gets a room identity with one binding, the platform's conversation id,
  and goes through the `rooms` knob, not `dm`. Confirming gives it a name,
  since group DMs usually have none.
- Each sender acts as its own identity with its own role, as in any room.
- Adapters tell a DM from a room by the platform's conversation type, never by
  counting participants: Slack's `im` or `mpim`, Google Chat's
  `DIRECT_MESSAGE`, `GROUP_CHAT` or `SPACE`, Teams' `oneOnOne` or `group`. A
  type the adapter doesn't know is a room.

## 3. Apps, devices and tokens

The CLI, the macOS and iOS apps and automations are clients of the control
API, which is the built-in `holaclaw` channel. **Each client is a binding**:
one per signed-in device, one per set of automations, and `holaclaw:cli`
for the owner's unix socket. Its alias is an id the daemon generates
(`holaclaw:01JC9F…`) and its display name is the device or automation name.
The CLI refers to every binding the same way, `<channel>:<alias>`, as
`identity show` prints it; the apps show the display name.

### Pairing a device

1. An admin mints a `holaclaw` invite for the identity, or a device already
   signed in to it does (Settings → Add device).
2. The device opens the `holaclaw://pair?server=…&code=…` link, by tap or by
   scanning the QR code, and calls `POST /v1/pair` with the code, a device
   name and its platform.
3. The daemon creates a new binding named after the device, with its first
   token, and returns the token. The app keeps it in the Keychain.

Self-service exists only between signed-in clients, and a pairing link a
client mints carries at most that client's own token scope. No channel can mint a
pairing link, because a `holaclaw` binding sees the identity's DMs from every
binding (§4), which is more than any single binding reaches. An identity with
no signed-in device asks an admin.

The pairing link carries the code, so treat it as a short-lived secret. The
`server` is `server.public_url` when it is set. Otherwise a link minted over
TCP carries the address the request reached, and one minted on the unix socket
the address `server.listen` names, over `https` with TLS and `http` without.
A loopback address (`127.0.0.1`, `::1`, `localhost`), reached through a
tunnel or on the host, counts as none, like a wildcard listen: no phone can
reach it. The code then goes out without a link, with a hint to set
`server.public_url`.

### Tokens

Every API request carries a token, except on the unix socket, which arrives
as `holaclaw:cli` by file permission. A token resolves to its binding, and the
binding to its identity. How tokens are stored, the scopes and the transport
rules are in [security.md §3](security.md#3-the-control-api).

- **Scope.** Each token has its own, `client` by default, set with `--scope
  admin` on `token create` or on a `holaclaw` invite. It can't exceed the
  identity's role (owner and admin allow `admin`, user allows `client`):
  asking for more is refused, and a later demotion lowers it to `client` in
  the same step. Every request also acts at the lower of the token's scope and
  the role.
- **Raising a scope is always explicit.** A promotion only allows `admin`; it
  raises no token. An owner or admin raises one with `holaclaw token set <id>
  --scope admin` (refused while the role is `user`), and `identity set --role
  admin` lists the tokens left at `client`:
  ```
  $ holaclaw identity set carla --role admin
  carla · role admin
    3 tokens stay at client scope; raise them with holaclaw token set <id> --scope admin
  ```
  Rotation keeps the scope. A device paired after the promotion can get
  `admin` from its invite.
- **Automations.** `holaclaw token create --identity I --name N` creates a
  new binding named `N` with its first token; `holaclaw token create
  --binding holaclaw:<alias> --name N` adds a token to an existing one. Both
  print the secret once. Several automations may share a binding
  (`automations` with `ci` and `backup`), or each get their own. An identity made only for automations is
  an ordinary user identity.
- **Rotation.** A token is replaced by a new one on the same binding, with the
  same name and scope, and the old one is revoked in the same transaction.
  A client rotates its own token with `POST /v1/me/token/rotate`, which
  answers with the new secret: the apps do it on a schedule, an automation
  whenever it likes. An admin rotates any token with `holaclaw token rotate
  <id>`, which prints the new secret once. The binding, its sessions and its
  history don't change.
- **Traceability.** Each token records its origin (pairing and the invite,
  `token create`, `rotate` and the token it replaced, or `init`), its
  creator, `last_used_at` and, once revoked, who revoked it and when. Traces
  and events carry the token id, so two automations on one binding stay
  distinguishable.

| To | Run | Effect |
|---|---|---|
| Cut one credential | `holaclaw token revoke <token>` | The binding and its other tokens stay. |
| Cut a whole client | `holaclaw identity unbind holaclaw:01JD1K…` | The binding goes, and every token on it is revoked. |

```
$ holaclaw identity show carla
carla · user · confirmed
  telegram:12345678     "Carla R."                       since 2026-09-01
  wa:+34600000000                                        since 2026-10-12
  wa:lid:123456789                                       since 2026-10-12
  holaclaw:01JC9F…      "iPhone 15"     1 active token   since 2026-11-03
  holaclaw:01JD1K…      "automations"   2 active tokens  since 2026-11-03

$ holaclaw token list --identity carla --all
ID        BINDING            NAME       SCOPE   ORIGIN                LAST USED  STATE
01JK1…    holaclaw:01JC9F…   iPhone 15  client  pair · invite 01JC8…  2h ago     active
01JM4…    holaclaw:01JD1K…   ci         client  token create · angel  5m ago     active
01JN2…    holaclaw:01JD1K…   backup     client  rotate · 01JM7…       1h ago     active
01JM7…    holaclaw:01JD1K…   backup     client  token create · angel  3d ago     revoked
```

In the app, the Devices screen lists the identity's `holaclaw` bindings, each
with its last activity (the latest use of any of its tokens), and removes
them. `GET /v1/me` tells a client who it is: its identity and role, its
effective scope, its binding and its token.

## 4. Sessions and what each token sees

```
dm:        <agent>/<channel>/dm/<identity>
holaclaw:  <agent>/holaclaw/dm/<identity>/<conversation>
room:      <agent>/<channel>/room/<conversation>[/<thread>]
task:      <agent>/task/<task id>
```

An identity with a Telegram and a `holaclaw` binding has separate sessions
and one memory. A reply goes back through the binding the message came from.
`holaclaw` conversations belong to the identity, not to one binding, so every
device and automation of the identity sees them. A room session belongs to
its room identity, never to whoever spoke first.

Every token sees **only its identity's sessions**, admins included:

| Session | Through a token |
|---|---|
| `holaclaw` conversations, from any of the identity's clients | read and write |
| DMs through the identity's other bindings | read-only, with a hint to continue in that channel |
| Task sessions of the identity's tasks | read-only, but a run can be aborted |
| Room sessions | not listed yet (§9) |

A read-only session carries `read_only` in the API. Sending to it or deleting
it answers `conflict`, and so does aborting a DM's turn, naming the channel to
continue in. The event stream and blob reads follow the same rule. Admins
inspect other identities' conversations through traces, by id. The unix
socket, acting as the owner on the host, sees and writes to everything. What
each role and scope may do across the API is in
[security.md §3](security.md#roles-and-scopes).

## 5. Memory

Memory belongs to an agent and is partitioned per identity:
- A user identity's DMs, through every binding, share one partition.
- A room identity has its own.
- Notes are private unless an owner or admin shares one explicitly.
- Tasks have no memory.

The full rules are in [memory.md](memory.md).

## 6. CLI and API

```
holaclaw identity list [--pending] | show <i>
holaclaw identity create --name N [--role user|admin] [--invite C] [--ttl D]
holaclaw identity set <i> [--name N] [--role R]
holaclaw identity disable <i> | enable <i>
holaclaw identity delete <i> [--keep-memory]
holaclaw identity confirm <pending> --name N [--role R]
holaclaw identity assign <pending> --to <i>
holaclaw identity unbind <channel>:<alias>
holaclaw invite create --identity I --channel C [--scope admin] [--ttl D]
holaclaw invite create --room --channel C [--ttl D]
holaclaw invite list | revoke <id>
holaclaw token create (--identity I | --binding holaclaw:<alias>) --name N [--scope admin]
holaclaw token list [--identity I] [--binding holaclaw:<alias>] [--all]
holaclaw token set <id> --scope client|admin
holaclaw token rotate <id>
holaclaw token revoke <id>
holaclaw init [--config dir] [--provider p] [--token-file path]
```

`--scope` on an invite only applies to `holaclaw`.

| Endpoint | Scope |
|---|---|
| `GET POST /v1/identities`, `GET PATCH DELETE /v1/identities/{id}` | admin |
| `POST /v1/identities/{id}/assign` (body: `to`), `DELETE /v1/bindings/{channel}:{alias}` | admin |
| `POST /v1/identities/{id}/disable`, `POST /v1/identities/{id}/enable` | admin |
| `GET POST /v1/invites`, `DELETE /v1/invites/{id}` | admin |
| `GET POST /v1/tokens`, `PATCH /v1/tokens/{id}` (scope), `POST /v1/tokens/{id}/rotate`, `DELETE /v1/tokens/{id}` (revoke) | admin |
| `GET /v1/me` (the caller's identity, effective scope, binding and token) | client |
| `GET POST /v1/me/devices`, `DELETE /v1/me/devices/{binding}` (POST returns a pairing link for the caller) | client |
| `POST /v1/me/token/rotate` (replaces the calling token) | client |
| `POST /v1/pair` | none: the code is the credential |

## 7. Skills

Skills follow the agent, not the identity: one learned level per agent. Every
learned write records the identity that taught it and the trace, so anyone
can see who taught what and undo it. See [skills.md](skills.md).

## 8. Events

| Event | Carries |
|---|---|
| `identity.created` | kind, status, the bindings it was created with, and the admin who created it |
| `identity.confirmed` | name, role, by |
| `identity.assigned` | source, target, bindings moved, memory rows, sessions ended, by or invite |
| `identity.disabled` / `identity.enabled` | identity, by |
| `identity.deleted` | role, bindings removed, tokens revoked, memory forgotten or kept, by, and `reason: pending_unbound` when deleted for having no bindings |
| `identity.bound` | identity, binding, and the binding it was matched through, or the invite, pairing or `token create` that created it |
| `identity.unbound` | identity, binding, tokens revoked, by |
| `token.created` | identity, binding, token, scope, origin (and the replaced token on rotation), by |
| `token.updated` | identity, binding, token, old and new scope, by, and `reason: demoted` when a demotion lowered it |
| `token.revoked` | identity, binding, token, by, or the unbind or rotation that revoked it |
| `access.granted` | room invites and auto-approved rooms |

## 9. Out of scope

- **Merging confirmed identities**, or moving a binding between them.
- **A room identity with more than one binding** (one team on two
  platforms). No platform is known to report several aliases for one room. A
  room whose id changes (Telegram's group → supergroup upgrade) becomes a new
  pending room identity for now.
- **Rooms in the apps.** It needs room membership, and not every platform
  can list members (Telegram bots can't), so the session list leaves rooms
  out for now.
- **Pairing from a channel** (a `/login` command on Telegram or Slack).
- **An admin view in the apps** of other identities' sessions.

## 10. Changes from today

- **Tokens.** `token list` names the creator by identity id and prints times
  as timestamps, not as "2h ago". Tokens made before their origin was
  recorded show `init` for the owner's first admin token and `token create`
  for the rest, with no creator.

## 11. Future additions

Not designed in detail and not planned yet. Each entry says what it needs and
what in this design it relies on, so adding it doesn't mean redoing the rest.

### Directory integration (LDAP, Active Directory, SSO)

For bigger teams, a directory (LDAP, Active Directory, or an SSO provider such
as Entra ID, Okta or Keycloak) becomes the source of truth for who exists,
their role, and whether they are still active.

- **Source.** An identity records where it came from: `local` (everything in
  this document) or a directory, with the directory's stable id
  (`objectGUID`, `entryUUID`, the OIDC `sub`), never the username, which can
  change. The directory owns the name, the role and the status; they are
  read-only locally, so a sync never undoes an admin's edit. Bindings, tokens
  and memory stay local.
- **Provisioning on first sign-in.** An identity is created when someone signs
  in with their directory account, not by importing the whole directory up
  front. A directory knows emails and usernames, not Telegram ids, so an
  imported identity would have no bindings and nothing to do until its person
  signs in anyway. The directory decides who may sign in (members of a given
  group, for instance). After that, a sync only refreshes the identities that
  already exist.
- **Sign-in as a way to create a binding.** The directory sign-in joins the
  three ways in §1. An unknown sender gets a sign-in link instead of waiting
  for an admin; signing in confirms the pending identity, or assigns it onto
  the directory identity if one already exists. An app signs in through the
  same login (the OIDC device flow) and gets a new `holaclaw` binding with its
  first token, as pairing does. The daemon still issues its own tokens.
- **Role groups.** A config rule maps directory groups to roles
  (`holaclaw-admins` → `admin`, everyone else → `user`). It runs at step 4 of
  the resolution (§1), so the scope cap in step 5 needs no change: a lost
  admin group caps that person's tokens on the next request. Role groups are
  not room identities; they never become rooms, sessions or memory. Their
  members come from the directory and are kept apart from room members (from
  the platform): two tables, never a shared one, and never a link between
  identities.
- **Rooms limited to a role group** ("only members of `ops` talk in #ops").
  A check between the room step and the sender step of the resolution (§1).
- **Leavers.** A directory account that is disabled or removed sets the
  identity to `disabled`. Nothing is deleted, and an admin can still delete
  it.
- **The owner stays local.** It never comes from the directory, so a broken or
  unreachable directory never locks everyone out. While the directory is
  unreachable, the last synced state applies: existing bindings and tokens
  keep working, and new sign-ins fail.

It relies on: the `disabled` status, the single resolution path with the role
as its own step and rooms resolved apart from senders, rooms that never carry
a role, policy chosen by role rather than by a list of identities, internal
ids as keys, and a single place in the code that creates bindings.
