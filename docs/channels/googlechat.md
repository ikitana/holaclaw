# Google Chat channel

The spec for `internal/channel/googlechat`. docs/channels and
[internal/channel/README.md](../../internal/channel/README.md) follow it.

For the step-by-step operator guide — Google Cloud console setup, configuring
holaclaw, first contact, daily use, troubleshooting — see
[guides/googlechat.md](../guides/googlechat.md). This file stays the spec.

## 1. How it works

A Google Chat app that uses a **Cloud Pub/Sub connection**. HolaClaw needs no public URL and no open port:

```
person ──message──▶ Google Chat ──event──▶ Pub/Sub topic ──pull──▶ holaclaw
person ◀──reply──── Google Chat ◀──────── Chat REST API ◀───────── holaclaw
```

- **Inbound:** Chat publishes every event for the app (message, added to a space, removed, button click)
  to a Pub/Sub topic. HolaClaw pulls from a subscription on that topic, the same way the Telegram adapter
  long-polls `getUpdates`. It acknowledges an event only after the envelope is stored.
- **Outbound:** holaclaw posts replies with the Chat REST API (`spaces.messages.create`), authenticated
  as the app.
- **One credential:** a service-account key. It authenticates both the Pub/Sub pull and the Chat API.

This is the one supported way to connect Google Chat. Two others were considered and rejected:

- **HTTP endpoint:** Chat POSTs to a public HTTPS URL. It needs a tunnel or a reverse proxy in
  front of the daemon.
- **Workspace Events API:** it still needs Pub/Sub, and it can't stand alone (see §8).

## 2. What you need

- **A Google Workspace account** (Business or Enterprise) that is allowed to use Chat apps. Personal
  @gmail.com accounts can't host Chat apps.
- **A Google Cloud project with billing enabled.** Pub/Sub requires a billing account. A bot's traffic
  stays inside the free tier.
- **Permission to edit that project.** You need it once, to create what §4 lists.

## 3. Configure holaclaw

Two steps, like Telegram's bot token:

```toml
# channels.toml
[channels.googlechat]
type   = "googlechat"
agent  = "main"
[channels.googlechat.access]
dm     = "invite"
rooms  = "approved"
reply  = "mention"              # Chat only delivers @-mentions in spaces; see §6
```

```
holaclaw auth set channels.googlechat service_account < holaclaw-key.json
holaclaw identity list                         # the id for the next command
holaclaw invite create --identity <id> --channel googlechat
```

`service_account` is the JSON key file as Google issues it, read from stdin. It never reaches argv or
the shell history. The key also says which project it belongs to, and holaclaw always pulls from the
subscription named `holaclaw` in that project, so channels.toml needs no Google-specific key. Every Chat
app needs its own Cloud project anyway, so a second bot is a second project with its own `holaclaw`
subscription, and the fixed name never collides.

`auth set` checks the key before writing it. Each failure names the step of §4 to redo:

| Check | Failure message |
|---|---|
| The JSON is a service-account key | "this is not a service-account key (§4.3)" |
| Google issues a token for it | "Google rejected this key: it was deleted or disabled (§4.3)" |
| `testIamPermissions` on the subscription holds `pubsub.subscriptions.consume` (Subscriber cannot `get` it) | "no subscription `holaclaw` in project `p` (§4.4)" or "the service account cannot pull from it (§4.4)" |
| A Chat API call as the app | "the Chat app is not configured (§4.5)" |

If Google is unreachable, the key is stored unchecked with a warning, the same as Telegram. `holaclaw
check` (SEC003) fails when the key is missing.

**Who can talk to it.** Two layers decide:

- **The platform:** only the people and groups named in the app's **Visibility** (§4.5) can find the app
  and message it. On Telegram, `dm = "open"` means anyone on the internet. Here it means anyone you made
  the app visible to.
- **The access knobs,** the same as every channel ([identity.md](../identity.md) §2). The knob decides
  what happens to an unknown sender:
  - `invite` (the default): ignored unless they send a valid code.
  - `open`: answered at once as a pending identity. That means the agent's `pending` policy (read only
    by default), agent memory only, and rate limits.

  For a team bot, `open` with a narrow Visibility list is a sensible setup.

An invite is how an account gets bound to an existing identity, with its role, whatever the knob: that
is how the owner and admins get their own Chat accounts recognised.

Take the identity's id from `holaclaw identity list`, then `holaclaw invite create --identity <id>
--channel googlechat` mints a one-shot code and prints it with its expiry:

```
code     <code>
channel  googlechat
expires  2026-10-07 10:00:00Z
redeem   Send this code to the bot in a direct message.
```

Redeeming the code binds whoever sends it to that identity. It is valid for 15 minutes (`--ttl`, at
most 1 h, or 24 h for an identity with no bindings yet), and the daemon stores only a hash of it.
Telegram prints a `link` line instead, a deep link that sends the code in one tap. Chat has no deep
links, so the code is typed:

- **DM:** open Google Chat, search for the app's name, open a DM with it and send the code as the only
  text. The access gate reads a bare code as an invite, the same as Slack, and the message is never
  stored or shown to the agent.
- **Space:** `holaclaw invite create --room --channel googlechat` mints a room code (it needs
  `rooms = "approved"`). Add the app to the space and send `@app <code>` there. That approves the
  space and links nobody.

## 4. Get the key: Google Cloud setup

Done once, in the Google Cloud console, by someone who can edit the project. Each link opens the right
page; choose your project at the top.

1. **Project and billing.** Create a project (or reuse one) and link a billing account:
   <https://console.cloud.google.com/projectcreate>, then *Billing → Link a billing account*.
2. **APIs.** Enable the Google Chat API and the Cloud Pub/Sub API:
   <https://console.cloud.google.com/flows/enableapi?apiid=chat.googleapis.com,pubsub.googleapis.com>
3. **Service account and key.** *IAM & Admin → Service accounts → Create*, name it `holaclaw`, grant no
   project roles. Then *Keys → Add key → JSON* and download the file. If the console says key creation
   is disabled, your organisation enforces `iam.managed.disableServiceAccountKeyCreation` (the default
   for organisations created since 2024). An organisation-policy admin has to make an exception for this
   project (see §7).
4. **Topic and subscription.** *Pub/Sub → Topics → Create topic* `holaclaw` (untick "add a default
   subscription"). Then:
   - On the topic, *Permissions → Add principal* `chat-api-push@system.gserviceaccount.com`, role
     **Pub/Sub Publisher**. This lets Chat publish to the topic.
   - *Create subscription* `holaclaw` (exactly this name: holaclaw looks for it) on that topic:
     delivery type **Pull**, ack deadline 60 s,
     expiration **Never expire**.
   - On the subscription, *Permissions → Add principal* the service account from step 3, role
     **Pub/Sub Subscriber**.
5. **The Chat app.** *APIs & Services → Google Chat API → Configuration*:
   <https://console.cloud.google.com/apis/api/chat.googleapis.com/hangouts-chat>
   - Untick **Build this Chat app as a Google Workspace add-on**.
   - **App name, avatar URL, description:** what people see in Chat.
   - **Interactive features:** on. Tick **Join spaces and group conversations**.
   - **Connection settings:** **Cloud Pub/Sub**, topic `projects/<project>/topics/holaclaw`.
   - **Visibility:** the people or groups in your domain who may find the app.
   - Save.

Step 5 has no API, so it stays in the console whatever holaclaw does (§9). The others are ordinary Cloud
resources.

## 5. The adapter

| Contract | Google Chat |
|---|---|
| `Run` | A pull loop over Pub/Sub REST (`pull`, `acknowledge`). Before the first pull, and after any failure, it checks with `testIamPermissions` that the key may pull and reports `connected` (a pull waits until an event arrives, so it can't tell that quickly). It reconnects with capped backoff, and reports `logged_out` on 401/403. |
| Store, then ack | An event is acked once `Inbound` returns. If storing fails, the ack deadline is set to 0 so Pub/Sub redelivers it. An event that does not parse is acked and traced, otherwise it would come back forever. |
| Idempotency | `PlatformMsgID` = `message.name`. Pub/Sub delivers at least once. The access gate records `PlatformMsgID` for 14 days and drops a redelivery as a duplicate, which the adapter acks; the adapter also keeps a short in-memory window. |
| Order | Pub/Sub does not guarantee order, so each pulled batch is sorted by `createTime`. The session debounce absorbs the rest. |
| Origin | Conversation = `spaces/<id>`. Space type `DIRECT_MESSAGE` maps to `dm`, everything else to `room`. Every Chat message has a thread, so `Thread` is set only when `threadReply` is true; a space without threads is one session. Replies go to the same thread (`REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD`). |
| Reply-to | When core sets `Target.PlatformMsgID` (`reply_to`), the first message quotes it with `quotedMessageMetadata`. Chat refuses a quote whose `lastUpdateTime` is stale, so the adapter reads the message with `messages.get` first. A message that is gone, or a quote Chat refuses, leaves a plain message. |
| Sender | Aliases are `users/<id>` and the email. `Mentioned` comes from the `USER_MENTION` annotation for the app. The text is `argumentText`, with the mention stripped. |
| Join a room | `ADDED_TO_SPACE` (not for a DM) raises the adapter's `OnJoined` hook with the space and the adder's aliases, which the daemon hands to the access gate. With `rooms = "approved"`, an owner or admin already linked on this channel approves the space by adding the app: `access.granted` with `approved_by`, and "This room is approved. Say hi!" posted in the space. Anyone else adding it approves nothing and gets no reply, only a `router.dropped` (`room_not_approved`). The space is then approved by a message: an @-mention of the app from a linked owner or admin, or a room invite code sent with an @-mention. Adding the app *by* @-mentioning it raises the join first and then the message. |
| Approvals | A card with the four choices as buttons. The click arrives as `CARD_CLICKED` through Pub/Sub. The presser comes from the event, never from the button payload, and must be an owner or admin. When core accepts the press, the card is edited to show the outcome ("Approved (once) by …" / "Denied by …") and loses its buttons; a refused press leaves the card as it is. |
| Text out | Canonical markdown is converted to Chat's subset (`*bold*`, `_italic_`, `~strike~`, `` `code` ``, `<url\|text>`) and split at 4000 characters. Each message gets a client-assigned id (`client-<ulid>`), so a retried send never posts twice. |
| Files in | `media.download` with the app's credentials, capped like Telegram. Files that live in Drive are not downloaded; the stub says so. |
| Files out | Not possible: uploading needs a user's OAuth, not an app's. `Limits.Kinds` is empty, so each attachment goes to `Receipt.Degraded` with a text notice. |
| Typing | Chat has no typing indicator, so the adapter does not implement `Typer`, and it posts no placeholder message. |
| Secrets | The key is read through a func per request, never logged, and scrubbed from errors. Signing uses standard-library RSA (an RS256 JWT exchanged at `oauth2.googleapis.com/token`), so the package adds no dependency. |
| Invites | Chat has no deep links. A DM invite is the bare code sent to the app. A room invite is the code with an @-mention of the app. The access gate already reads both forms. |

There are no contract changes. Config only adds `googlechat` to the `type` enum.

## 6. Limits

- **In spaces, Chat only delivers messages that @-mention the app.** `reply = "always"` cannot work, and
  `holaclaw check` warns about it (new rule). DMs deliver everything.
- **The bot cannot send files.** Images and documents arrive as a text notice.
- **No typing indicator.**
- **Async replies only.** A Pub/Sub app cannot answer inline, so Chat dialogs are out. This doesn't
  affect holaclaw, which answers asynchronously anyway.
- **Pub/Sub keeps undelivered events for 7 days.** A daemon that is down longer loses what came in
  meanwhile.
- **Quota:** 60 messages per space per minute by default. Long replies split into several messages, so
  this can matter in a busy space.

## 7. Security

- **The key can pull from one subscription and post as the app, nothing else.** The service account
  holds no project roles; Subscriber is granted on the subscription alone. Leaking the key means someone
  can read the app's events and speak as the app until the key is deleted.
- **Store the key like any secret.** It lives in `auth.toml` (0600). It is never part of a backup unless
  asked, and never reaches the model, tools or traces.
- **Inbound events are trusted because of where they come from.** Only Chat can publish to the topic
  (IAM), and only the service account can read the subscription. Unlike a webhook, there is no request
  signature to verify.
- **Key creation blocked.** The policy exists to keep long-lived keys out. The exception is per project,
  so a dedicated project for the bot keeps it narrow. A keyless path is in §8.

## 8. Not in this version

- **`reply = "always"` in spaces.** The Workspace Events API can deliver every message in a space to the
  same topic. It needs one-time admin approval (`chat.app.messages.readonly`), one subscription per
  space, and renewal (4 h with message content, 7 days without). Worth it only if someone asks for it.
- **Keyless auth.** Impersonate the service account with a stored user token (IAM Credentials
  `generateAccessToken`). It avoids the key policy but stores a broader credential, so it's not the
  default.
- **Sending files.** Chat's upload endpoint (`media.upload`) only accepts user credentials, never the app's.
  There are two ways around it:
  - **Images through Cloud Storage:** upload the image to a bucket in the same project and send a card
    whose image is a signed URL. It needs one more setup step (a bucket the service account can write
    to), and a signed URL lives 7 days at most, so old images stop showing.
  - **Per-user OAuth:** upload as the person, so the file looks like it came from them, not the bot.
- **The add-on variant of Chat apps over Pub/Sub.** Still in developer preview.
- **HolaClaw creating the Cloud resources itself.** That would mean a new command (there is no `holaclaw
  setup` today), and it would need a Google token with the `cloud-platform` scope:
  - Google's device flow, which `holaclaw login codex` uses, allows only sign-in, two Drive scopes and
    YouTube: no Cloud, Pub/Sub, IAM or Chat scope.
  - A browser login needs holaclaw's own OAuth client, verified by Google for a sensitive scope.
    Without verification it shows a warning and is capped at 100 users.
  - Using gcloud's client ID is not allowed.

  The one easy route left is pasting a short-lived token from `gcloud auth print-access-token` run in
  Cloud Shell. Even then, step 5 of §4 stays in the console. Reconsider after the first real setup shows
  how painful §4 is.
