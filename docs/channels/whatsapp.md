# WhatsApp channel

The spec for `internal/channel/whatsapp`, a built-in channel built on `whatsmeow`. For linking an account
and running it day to day, see the [operator guide](../guides/whatsapp.md).

## 1. How it works

HolaClaw becomes a **linked device** of a WhatsApp account, the way WhatsApp Web or the desktop app is. It speaks
WhatsApp's multi-device protocol in-process through [whatsmeow](https://github.com/tulir/whatsmeow), a pure-Go
library. It needs no public URL, no open port and no Meta business account:

```
person ──message──▶ WhatsApp servers ──websocket──▶ holaclaw (linked device)
person ◀──reply──── WhatsApp servers ◀──websocket── holaclaw
                          ▲
            phone (primary device) links holaclaw once with an 8-character code
```

- **Linking:** the operator runs `holaclaw login whatsapp --phone +34…`, and holaclaw asks WhatsApp for a pairing
  code. On the phone they open *Linked devices → Link a device → Link with phone number instead* and type it.
  There is no QR code to scan.
- **Inbound:** WhatsApp pushes every message for the account over the websocket. HolaClaw acknowledges a message only
  after the envelope is stored.
- **Outbound:** replies go out over the same websocket, end-to-end encrypted like any WhatsApp message.
- **Credential:** none to paste. Linking creates the device keys, and they live in a session database that holaclaw
  writes (§5.2). `auth.toml` holds nothing for this channel.

> **Why a database and not a secret.** The device keys alone would fit in a secret, but whatsmeow keeps much more
> next to them: an encryption session per contact, the group keys, the one-time keys WhatsApp hands out and the
> linked id ↔ phone map. Every message sent or received moves the encryption session with that contact forward,
> and the new state must be saved before the next message, so the library reads and writes its store on nearly
> every message. That is the job of a database, and whatsmeow already ships one with its own schema and migrations.

## 2. What you need

- **A WhatsApp account on a phone.** The phone is the primary device, and holaclaw is one of its linked devices
  (WhatsApp allows four).
- **A number for the bot, ideally its own.** People message that number to reach the agent. Use a second SIM, an
  eSIM, or the WhatsApp Business app with a second number on the same phone. Linking your personal number is not
  supported:
  - the bot would see every one of your chats;
  - your contacts would reach the access gate instead of you;
  - you could only talk to the bot through "Message yourself".
- **The phone online now and then.** WhatsApp logs out linked devices whose primary phone has not connected for
  about 14 days.
- **Accepting the risk of an unofficial client.** WhatsApp's terms do not allow automated or unofficial clients, and
  Meta can ban a number it considers abusive. §7 lists what holaclaw does to look like a person and not a spammer.
  It reduces the risk; it does not remove it.

## 3. Configure holaclaw

```toml
# channels.toml
[channels.whatsapp]
type  = "whatsapp"
agent = "main"
[channels.whatsapp.access]
dm     = "invite"
rooms  = "approved"
reply  = "mention"
```

No new config keys. The phone number is not a key: it is needed once, to link, and afterwards the session knows it.

`reply = "mention"` does not starve the agent of context. As on every channel, room messages that don't mention
the bot are still stored in the room transcript. When someone mentions it, a bounded window of recent messages,
each prefixed with its verified sender, goes into the prompt (docs/sessions.md). The mention only decides when a turn
starts.

**Link the device.** The daemon must be running:

```
$ holaclaw login whatsapp --phone "+34 600 000 000"
On the phone with +34 600 000 000, open WhatsApp → Settings → Linked devices → Link a device
→ Link with phone number instead, and enter:

    ABCD-EFGH

The code expires at 10:02:40. WhatsApp also shows a notification on the phone that opens this screen.
waiting for the phone… (Ctrl-C stops waiting; the daemon keeps the code alive until it expires)
whatsapp: linked as +34600000000
```

- **Phone format.** `--phone` is the number in international format. Spaces, dashes and a leading `+` are ignored.
  A number starting with `0` is refused (`holaclaw: whatsapp: use the international format, with the country code`).
- **Expiry.** The code lives about 160 s: WhatsApp closes the linking socket when the QR codes it rotates run out.
  If it expires, run the command again.
- **Resuming.** Ctrl-C only stops the CLI watching. Running `holaclaw login whatsapp` again while the code is
  pending shows the same code and waits; `--phone` is needed only to request a new one.
- **Already linked.** The command refuses to relink an instance that is already linked
  (``holaclaw: whatsapp: already linked as +34…; run `holaclaw logout whatsapp` first``). Relinking needs no restart.
- **Device name.** The phone lists the device as **holaclaw**.
- **The number stays visible.** Once linked, `holaclaw status` shows the bot's number as the channel's detail, so
  nobody has to remember which number the bot is:

  ```
  whatsapp  whatsapp  main  connected  +34600000000
  ```

`holaclaw logout whatsapp` unlinks: the device is removed from the phone's list, and the session database is
deleted. Removing the device from the phone's *Linked devices* does the same from the other side: the instance goes
`logged_out` and its session is wiped (§5.3).

**Who can talk to it.** The access knobs, the same as every channel ([identity.md](../identity.md) §2). Anyone with
the number can message it, as on Telegram, so `dm = "invite"` is the sensible setting.
`dm = "open"` answers any stranger who finds the number.

**Invites.** Take the identity's id from `holaclaw identity list`, then `holaclaw invite create --identity <id>
--channel whatsapp` prints a `link` line and its QR code, like Telegram:

```
code     <code>
channel  whatsapp
expires  2026-10-16 10:00:00Z
link     https://wa.me/34600000000?text=<code>
```

`wa.me` opens a chat with the bot's number with the code already typed, so the person taps *send*. The number comes
from the instance status (§5.1), so an instance that is not connected prints the code with a one-line instruction
instead. The access gate already reads a bare code as an invite.

**Groups.** Someone in the group adds the bot's number, the way they add any contact. With `rooms = "approved"`:

- An owner or admin already linked on this channel approves the group by adding the bot, through `OnJoined`, if
  WhatsApp names who added it (§9).
- Otherwise `holaclaw invite create --room --channel whatsapp` mints a room code. Someone sends `@bot <code>` in the
  group. The adapter strips the bot's own mention, so the gate sees a bare code with `Mentioned` set, the form it
  already reads.

## 4. The account and its ids

WhatsApp knows a person by two ids. Both are delivered in authenticated stanza fields, never in message text:

- **Phone number JID:** `34600000000@s.whatsapp.net`.
- **LID** (linked id): `123456789@lid`. It is an opaque per-account id that WhatsApp uses more and more, and always
  in groups that hide phone numbers.

A stanza carries the sender in `Info.Sender` and, often, the other id in `Info.SenderAlt`. `SenderAlt` is trusted
only when the sender is LID-addressed: then it comes from the stanza's `sender_pn` or `participant_pn`. For a
phone-addressed sender whatsmeow fills it from its own LID map, so the number is the only alias.

| | WhatsApp |
|---|---|
| Aliases | `wa:+34600000000` (E.164) and `wa:lid:123456789`. Both when the stanza carries both; the LID alone when the number is hidden. Device suffixes (`:12`) are stripped. |
| Display name | The push name the sender set for themselves. Untrusted, like every display name. |
| DM conversation | The peer's phone JID when known, else the LID. The DM session key is per identity, not per conversation id, so a chat that moves from phone to LID addressing keeps its session. |
| Group conversation | The group JID, `<id>@g.us`. |
| Thread | Never set. WhatsApp has no threads. |
| `FromSelf` | Set on messages the account itself sent (typed on the phone, or another linked device). The router drops them. |

**A person is linked once.** Matching alone finds a person through any alias in the envelope, but it never adds a
missing one. Without linking, a person ends up with two identities:

1. Their first message arrives with the phone number alone, and it gets linked.
2. Later, in a group that hides phone numbers, a message arrives with the LID alone, so the LID matches nobody.
3. The person becomes a new pending identity, and an admin has to sort it out.

**Core links the missing ids, from one envelope only.** When an envelope resolves to an identity through one of its
aliases, the other aliases of *that same envelope* are linked to the same identity, and an `identity.bound`
event records it. They come from one authenticated stanza, so they are the same account. The next message that
carries only the LID already finds the person. Two guards:

- **Aliases already on different identities:** nothing is merged or moved. The message resolves through the first
  match, and the conflict is logged for an admin.
- **Recycled phone numbers:** carriers give old numbers to new people, who get a new WhatsApp account with a new
  LID. If the identity an envelope resolves to already holds an alias with the same prefix as one of the envelope's
  aliases but a different value (`wa:lid:987` on the identity, `wa:lid:111` in the envelope), the match is not
  trusted. The sender is treated as unknown, and the conflict is logged. The LID is the stable id; the phone number
  is a hint.

> **Why one envelope and never more.** Two ids in one stanza are WhatsApp saying they are the same account. Two ids
> in two messages are a guess, however good: a shared group, the same push name, a reply that follows. HolaClaw never
> joins identities on a guess, because a wrong join hands one person's sessions and memory to another. So the
> adapter takes aliases from `Info.Sender` and `Info.SenderAlt` of the stanza it is reading, and does not consult
> whatsmeow's LID ↔ phone map, which is learned from earlier stanzas. Ids that never arrive together stay apart until
> an admin joins them with `holaclaw identity assign`.

This lives in core, not in the adapter: `internal/store` matches and links in one transaction, `internal/auth` applies
the same guard to guests and invites, and the router (§6) emits the event. The adapter only declares the `wa:` and
`wa:lid:` namespaces (`channel.AliasNamespacer`). It works the same for every channel whose
senders carry more than one alias, Google Chat's user id and email among them.

## 5. The adapter

### 5.1 Contract

| Contract | WhatsApp |
|---|---|
| `Run` | Opens the session database. No device in it: reports `unlinked` and waits for a login. Otherwise connects. `Connected` reports `connected` with the account's number as the detail (`+34600000000`, which invite links use). The adapter drives reconnects with capped backoff (2 s to 5 min, with jitter), and whatsmeow's own auto-reconnect is off, so one place owns the policy and reports it. §5.3 maps every disconnect. |
| Store, then ack | `SynchronousAck` is on and the handler is registered with `AddEventHandlerWithSuccessStatus`. It returns `false` when `Inbound` fails, so whatsmeow sends no ack and the server redelivers. A Signal message can be decrypted only once, so `EnableDecryptedEventBuffer` keeps the plaintext until the ack. The redelivery then reads it from there. |
| Idempotency | `PlatformMsgID` = `<chat JID>/<message id>`. Message ids are unique per sender, not globally, so the chat is part of the key. The dedupe keys (`Envelope.DedupeKeys`) are the sender's ids with the message id instead, never the chat, so a phone-addressed and a LID-addressed copy of the same retry are one message; `PlatformMsgID` keeps the chat, since replies and reactions need it. The access gate records the keys for 14 days, also across restarts, and a duplicate is acked with no read receipt. The adapter keeps no dedupe of its own. |
| History | The phone pushes chat history to a newly linked device. `HistorySync` events are dropped unread: never stored, never routed, never answered. Messages that queued while holaclaw was offline arrive as ordinary messages after a reconnect, and they are answered like Telegram's queued updates. |
| Order | The websocket delivers in order per chat. The session debounce absorbs bursts. |
| Sender | §4. Aliases come from `Info.Sender`, plus `Info.SenderAlt` only when the sender is LID-addressed (otherwise whatsmeow filled it from its LID map). |
| Mentioned | The bot's phone JID or LID is in `ContextInfo.MentionedJID`, or the message quotes one of the bot's messages. The bot's own `@<number>` token is stripped from the text. |
| Join a group | `JoinedGroup` raises `OnJoined` with the group JID and the adder's aliases, when the event names one. The gate decides, the same as Telegram and Google Chat. |
| Text in | `conversation`, `extendedTextMessage.text` and media captions. Ephemeral and view-once wrappers are unwrapped. |
| Files in | Image, video, audio (voice notes included), document and sticker go through `Download`, capped like Telegram, into a blob. View-once media is not downloaded; a stub says it was not shown. Location and contact cards become a one-line text stub. |
| Reactions in | The adapter turns a `ReactionMessage` into an envelope with a reaction (§6) and decides nothing else: core routes it. Only a reaction on the account's own message is routed: message ids are chosen by their senders, so a group member could post a decoy with the id of the bot's approval prompt. The key must name the bot (in a group `Participant` is the bot's PN or LID; in a DM `FromMe` is false), and in a group a `RemoteJID` must be the group; anything else is dropped. An emoji over 64 bytes, or with anything but symbols, marks, format characters and keycap digits, is dropped. The envelope's `PlatformMsgID` is the reaction's own id tagged `r:`, `<chat JID>/r:<reaction id>`, so it never collides with a message's; the dedupe uses it. The target is `<chat JID>/<message id>`, the format of a message's `PlatformMsgID`. Empty text means the reaction was removed. A reaction is not a message: it sets no disappearing timer and queues no read receipt. |
| Reactions out | `Reactor` (§6): whatsmeow's `BuildReaction` sent with `SendMessage`. Any emoji is allowed (`Limits.Reactions` is nil), an empty one removes the reaction. In a group WhatsApp needs the target's sender, so the adapter remembers the sender of every message it stored (up to 8192 messages, first writer wins, never overwriting its own) and marks the ones it sent as its own. A group message it does not know is refused with an error; in a DM an unknown message is taken to be the peer's. |
| Dropped | Edits, deletions, protocol messages, encrypted reactions (`EncReactionMessage`) and reactions without a target, polls, `status@broadcast`, broadcast lists and newsletters. Logged at debug level with the id only, never the content. |
| Calls | Every incoming call is rejected through `RejectCall`, off the event-handler goroutine so a network round trip never blocks it. A caller who already had a turn with the bot — recorded when `Typing` marks their messages read — hears "I can't take calls. Please write to me instead." in the DM; a stranger or a group call gets the reject and nothing else (§7). Logged at debug level with the call id only. |
| Text out | Canonical markdown is converted to WhatsApp's subset: `*bold*`, `_italic_`, `~strike~`, `` `code` ``, ```` ``` ```` blocks, `> ` quotes and lists. Headings become bold, and links become `text (url)` because WhatsApp links bare URLs itself. Text is split at 4000 characters. Each part gets its message id before the first try, and a retry reuses it. |
| Files out | Image, video and document through `Upload`, with captions up to 1024 characters. Audio goes out as a voice note when it is already Ogg/Opus, and as a document otherwise: holaclaw has no ffmpeg. `Limits.MaxFileBytes` is 100 MB, because `Upload` takes the whole file in memory. |
| Reply-to | When core sets `Target.PlatformMsgID` (`reply_to`, issue #93), the first part quotes that message: `ContextInfo.StanzaID`, `Participant` and `QuotedMessage`. A message the adapter can no longer quote goes out plain. |
| Disappearing messages | If the inbound message carried `ContextInfo.Expiration`, the reply carries the same, so the bot respects the chat's timer. |
| Typing | `Typer`: `SendChatPresence(composing)` every 8 s until stop, then `paused`. The adapter sends `available` presence once on connect, which typing needs. |
| Read receipts | A message is marked read when it starts a turn, with the typing indicator. Dropped messages stay unread, so whoever holds the phone can still see them. |
| Approvals | No `ApprovalPrompter`: WhatsApp gives unofficial clients no buttons. Core sends the prompt as text and the approval is answered with a reaction (§6). |
| Secrets | The session database is the credential (§5.2). The pairing code is returned to the admin who asked for it and never goes into events, traces or logs. whatsmeow's logger is bridged to slog at warn and above: its debug level dumps decrypted stanzas. |

### 5.2 Session store

whatsmeow's `sqlstore` on the `modernc.org/sqlite` driver, already a dependency. whatsmeow parses the `sqlite`
driver name as its SQLite dialect, so no CGO is needed. Layout:

- **One file per instance:** `<data_dir>/whatsapp/<instance>.db`. The directory is 0700 and the file 0600. The
  database runs in WAL mode, so while it is open SQLite keeps `<instance>.db-wal` and `<instance>.db-shm` next to
  it. They hold key material too, so they are 0600 like the file (SQLite copies its mode), never backed up, and
  deleted with it on logout.
- **Its own file, not holaclaw's database.** whatsmeow runs its own migrations, and the file holds the device's
  identity keys, Signal sessions and the LID map.
- **Never in a backup.** Backups name the database and the blobs explicitly, so this file is left out without any
  change. Restoring it would be wrong anyway:
  - Signal ratchets move with every message, so a restored copy is stale and breaks decryption.
  - If the original device is still running, the restored copy is a second device with the same keys. WhatsApp
    answers that with `StreamReplaced`.

  Relinking costs one code, so a restored daemon comes up `unlinked`.
- **Deleted on logout,** whether holaclaw asked for it or the phone did.

### 5.3 Lifecycle

| whatsmeow event | State | Detail and action |
|---|---|---|
| No device in the store | `unlinked` | "not linked: run `holaclaw login whatsapp --name <name> --phone …`", without `--name <name>` when the instance is called `whatsapp` |
| Login started | `linking` | "waiting for the code on the phone". Ends in `connected`, or back to `unlinked` when the code expires. |
| `PairSuccess`, then `Connected` | `connected` | The account's number. |
| `Disconnected`, `KeepAliveTimeout`, transient `ConnectFailure` | `reconnecting` | Backoff, then connect. |
| `LoggedOut` | `logged_out` | "unlinked from the phone, or WhatsApp logged this device out; run login again". The store is wiped. |
| `StreamReplaced` | `logged_out` | "another client is using this session". No reconnect: two copies fighting over one session is the pattern most likely to get the number flagged. |
| `TemporaryBan` | `logged_out` | "WhatsApp banned this number temporarily: <reason>, for <duration>". The adapter retries once the ban expires. |
| `ClientOutdated` | `logged_out` | "WhatsApp rejected this client version; update holaclaw". whatsmeow carries the WhatsApp Web version it claims to be, so holaclaw releases have to track whatsmeow. |

### 5.4 Login inside the daemon

The flow runs in the daemon, the single owner of the session, like the Codex device-code login:

1. **Create.** A fresh device goes into the store, holaclaw sets its OS name to `holaclaw`, and the client connects.
2. **Request the code.** The adapter waits for the first QR event. That means the socket is up; the QR itself is
   ignored. Then it calls `PairPhone(phone, true, PairClientChrome, "Chrome (Linux)")`. whatsmeow returns the
   code, and `true` asks WhatsApp to notify the phone.
3. **Answer.** The code goes back to the caller with an expiry of now + 160 s.
4. **Finish.** `PairSuccess` stores the device and reports `connected`. An expired code, a `PairError` or a number
   that is not on WhatsApp fails the login with that reason, and the half-made device is deleted.

## 6. Changes outside the package

All of these are contract changes or new dependencies, for the tech lead to approve.

- **`internal/channel/channel.go`:** a new optional interface, implemented only by WhatsApp:

  ```go
  // Linker links an adapter to an account interactively (WhatsApp pairing code).
  type Linker interface {
  	Link(ctx context.Context, phone string) (LinkState, error)
  	LinkState() LinkState
  	Unlink(ctx context.Context) error
  }
  ```

  `LinkState` holds a status (`pending | done | failed | none`), the code, its expiry and a detail.
- **Reactions, for every channel.** Reactions are a core feature, not a WhatsApp one. Adapters only translate, and
  core decides what a reaction means. WhatsApp and Telegram implement it first, and the client shows the agent's
  reactions. Google Chat and Slack follow later.
  - **`internal/channel/channel.go`:** a second optional interface, and the emoji a channel accepts:

    ```go
    // Reactor reacts to a message with one emoji. target.PlatformMsgID is the
    // message reacted to, set by core from the turn's inbound message.
    type Reactor interface {
    	React(ctx context.Context, target event.Origin, emoji string) error
    }
    ```

    `Limits.Reactions` lists the emoji the channel accepts; nil means any. Telegram needs it: bots react only from
    a fixed list.
  - **Tool `react`:** one parameter, `emoji`, within the 150-token schema cap. It is registered for a turn only
    when the session's channel implements `Reactor`, and core refuses an emoji outside `Limits.Reactions` with an
    error the model can read. The target comes from the session origin, never from the model. A turn that reacted
    and wrote no text sends nothing else, the same way a turn that only attached a file does. It is a contract
    change in `internal/tool`.
  - **Reactions in:** the envelope gains an optional reaction: the target's platform message id, the emoji, and
    whether it was removed. The sender is resolved and gated like any message. Core keeps the platform message ids
    it sent (from `Receipt.PlatformMsgIDs`) and routes by target:
    - **An approval prompt:** an answer to the approval (below).
    - **Another of the bot's messages:** stored in the conversation as a short line from that sender,
      `[👍 on: <first 80 characters of the message>]`. It starts no turn, otherwise the agent would answer every
      thumbs-up; the next turn sees it in its context.
    - **Anything else:** dropped.
  - **Approvals by reaction,** for a channel that implements `Reactor` and not `ApprovalPrompter`. Channels with
    buttons keep their buttons. Core sends the prompt as text and appends how to answer it:

    ```
    React to this message to answer: 👍 allow once · 🔁 allow for this session · ✅ always allow · 👎 deny
    ```

    Skin-tone variants count as the base emoji, and any other emoji is ignored. The reactor comes from the stanza,
    never from the message, and must be an owner or admin. The first accepted reaction resolves the approval;
    changing or removing it later does nothing. Core then replies to the prompt with who answered, by identity
    name, not push name: "Approved (once) by Angel", "Denied by Angel". A refused reaction gets no answer.
  - **API:** the event stream gains a reaction event with the session, the target message and the emoji, and the
    client shows it under that message.
- **`internal/event`:** a new `identity.bound` event type, for the alias linking in §4.
- **Router and `internal/auth`:** the alias linking and its two guards from §4. Each channel declares its alias
  prefixes (`wa:`, `wa:lid:`), so the recycled-number guard stays generic.
- **Control API.**
  - `POST /v1/channels/{name}/login` gains a body `{phone}` and answers with the `DeviceLogin` schema already used
    for Codex: `user_code` and `expires_at` are filled, and `verification_url` is left empty.
  - `GET /v1/channels/{name}/login` polls it.
  - `DELETE /v1/channels/{name}/login` unlinks.

  All three are admin scope. A channel without `Linker` keeps answering `501`.
- **CLI:** `holaclaw login` and `holaclaw logout` have one subcommand per type, each with its own flags. The instance
  is named with `--name`, which defaults to the type itself (`whatsapp` for the whatsapp subcommands, `codex` for
  codex); there is no positional argument.
  - `holaclaw login codex [--name <provider>]`: as today (Codex device code). The provider defaults to `codex`.
  - `holaclaw login whatsapp [--name <channel>] --phone <number>`: the pairing code of §3.
  - `holaclaw login telegram [--name <channel>]`: asks "Paste the bot token:" with hidden input, checks it as
    `auth set` does, and stores it in `auth.toml`. It is the cleaner path for an operator at a terminal. `auth set`
    stays for scripts and pipes.
  - `holaclaw logout whatsapp [--name <channel>]`: unlinks a WhatsApp instance. `holaclaw logout telegram
    [--name <channel>]` removes the token, the same as `auth rm`.
- **Config:** `whatsapp` joins the `type` enum. No new keys.
- **`holaclaw check`:** warns when a WhatsApp instance is not linked. This is the counterpart of SEC003's "credential
  missing", since there is nothing in `auth.toml`.
- **API invite link:** `inviteLink` gains a `whatsapp` case that builds the `wa.me` link from the status detail. Group
  invites have no link.
- **Daemon:** builds the adapter with the session path, `PutBlob`, `Blobs`, `OnApproval` and `OnJoined`.
- **Dependencies:** `go.mau.fi/whatsmeow`, which pulls in:
  - `go.mau.fi/libsignal` and `go.mau.fi/util`
  - `google.golang.org/protobuf`
  - `github.com/coder/websocket`
  - `github.com/rs/zerolog`
  - `golang.org/x/crypto`
  - two GraphQL helpers

  All pure Go. whatsmeow is MPL-2.0, which can be combined with holaclaw's Apache-2.0. whatsmeow's `go.mod` asks for
  Go 1.26, the same as holaclaw.
- **Docs:** internal/channel/README.md has a WhatsApp paragraph, and identity.md the alias forms of §4.

## 7. Ban risk

Meta does not publish its rules. HolaClaw's defaults already look like a person answering their phone, and the
adapter adds a little:

- **It never starts a conversation with a stranger.** It answers people who wrote first. Tasks deliver only to
  conversations that already exist.
- **Strangers are filtered before any reply** (`invite` by default). A dropped stranger gets nothing, not even a read
  receipt.
- **One send at a time per instance.** Split messages go out about 1 s apart, after a typing indicator and a read
  receipt, the way a person replies.
- **No history download, no presence subscriptions, no contact scraping.**
- **Calls are rejected, not ignored, but a stranger's call stays silent.** Answering would need a voice stack
  holaclaw does not have, so every call is declined through `RejectCall`; only a caller who already had a turn with
  the bot hears why, since telling a stranger the gate dropped anything at all is the one thing never to do.
- **One live session.** On `StreamReplaced` the adapter stops; it does not fight back.
- **No bulk features.** No broadcast lists, no status posts, no joining groups by link.

## 8. Limits

- **Unofficial.** A protocol change on WhatsApp's side breaks the channel until whatsmeow and holaclaw are updated
  (`ClientOutdated`).
- **The phone has to stay alive.** After about 14 days offline, WhatsApp unlinks the device.
- **No buttons.** Approvals are reactions with a legend.
- **Voice notes out** only when the audio is already Ogg/Opus. Other audio arrives as a file.
- **The bot cannot join a group by itself.** A member adds its number.
- **Four linked devices per account.** HolaClaw takes one.
- **In-process.** whatsmeow parses network input inside the daemon. It is memory-safe Go, but it shares the
  daemon's process and file access.

## 9. Manual tests

Checked against whatsmeow's source, but not yet against a real account. Once the adapter, the linking and the
reactions are merged, these run by hand with a throwaway number, never a personal one:

- **Linking.** The code arrives, the phone notification appears and opens the code screen, and the code lasts about
  160 s (whatsmeow's own comment says the expiry is unknown). Relinking an already linked instance is refused.
  Removing the device from the phone moves the instance to `logged_out` and wipes the session.
- **Direct messages.** Text, media in and out, a long reply split in parts, and a disappearing-messages chat.
- **Duplicate sends.** A resend with the same message id shows once.
- **Groups.** Mentions, a room code, and who added the bot: does `JoinedGroup` name the adder when the bot is
  added to an existing group, and not only when a group is created with it? If not, `OnJoined` gets no aliases, and
  a group can only be approved by a code.
- **Typing and read receipts.** Is the `available` presence needed for the typing indicator to show?
- **Ids.** How often a group message arrives with the LID alone, and that a later message with both ids links them
  (§4).
- **Reactions.** Reactions in and out, and an approval answered by reaction. Do reactions from a LID-only sender
  carry the same ids as their messages? Do skin-tone variants arrive as separate code points?
- **Session conflicts.** A second copy of the session gets `StreamReplaced` and stops.
- **Status and invites.** `holaclaw status` shows the number, and the invite link opens a chat with the code typed.
- **Ban behaviour.** Is a fresh number linked as a companion flagged faster than an aged one?
