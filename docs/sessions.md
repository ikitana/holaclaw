# Sessions and messages

How a message becomes a turn: the session it lands in, how sessions run side
by side, what a message carries, and how a reply gets back.

## Contents

1. [Concepts](#1-concepts)
2. [Session keys](#2-session-keys)
3. [Turns and concurrency](#3-turns-and-concurrency)
4. [Rooms](#4-rooms)
5. [Messages and attachments](#5-messages-and-attachments)
6. [Replies and reactions](#6-replies-and-reactions)
7. [Inbound delivery](#7-inbound-delivery)

## 1. Concepts

- A **session** is one conversation with one agent: a DM, a room, a thread in
  a room, or one conversation of an app. It holds the transcript, and it lives
  until it is deleted: there is no time-based reset, and compaction keeps a
  long one inside the model's window ([context.md](context.md)).
- A **turn** is one run of the agent loop in a session: it reads the messages
  that arrived, calls the model and the tools, and ends with a reply.
- Each session has one **actor**: a goroutine with a persisted mailbox. Turns
  run in strict order inside a session and in parallel across sessions.
- The **router** resolves who sent a message ([identity.md](identity.md)),
  picks the agent the channel serves, and derives the session key.

## 2. Session keys

The key is one pure function of the agent, the channel instance, the
conversation and the sender's identity:

| Conversation | Key |
|---|---|
| a DM on a channel | `<agent>/<channel>/dm/<identity>` |
| an app or the CLI | `<agent>/holaclaw/dm/<identity>/<conversation>` |
| a room | `<agent>/<channel>/room/<conversation>` |
| a thread in a room | `<agent>/<channel>/room/<conversation>/<thread>` |

- **Isolated by default.** One session per DM identity, per room and per
  thread; there is no shared-DM mode. The identity is resolved before the key
  is derived, so two accounts of one person on one channel instance share a
  session. An app opens as many conversations as it likes, each its own
  session, and never one of another identity's.
- **A room belongs to its room identity**, never to whoever spoke first.
- **A thread starts with its question.** A new thread session whose first
  message hangs off an earlier one starts with a copy of the user and
  assistant messages of that earlier turn, without its tool traffic, so a
  follow-up in the thread keeps the question it answers.
- **Tasks** run in sessions of their own ([tasks.md](tasks.md)).
- Assigning a pending identity to a confirmed one ends its sessions rather
  than moving them ([identity.md](identity.md#assigning)).

## 3. Turns and concurrency

- **Stored first.** A message is stored when it arrives, before any turn
  runs, so a restart replays it instead of losing it.
- **Coalescing.** An actor waits 600 ms after a message before it starts a
  turn, up to three times that while messages keep coming, so rapid messages
  become one turn. In a room, a turn ends at the first message that mentions
  the bot, so each mention gets its own reply.
- **Mid-turn messages.** A message that arrives while a turn runs waits for
  the next turn, and is placed after the running turn's answer in the
  transcript. A prompt never ends with an assistant message.
- **Abort.** `POST /v1/sessions/{id}/abort`, or `Ctrl-C` in `holaclaw chat`,
  stops the running turn.
- **Lanes, not priorities.** Interactive turns and task turns run in separate
  lanes, each capped per provider (4 interactive turns and 1 task turn at
  once), on top of the provider's own `max_concurrent`. A busy task lane never
  holds up a person.
- Each turn is bounded by a step limit and the context budget, and every hop
  of it is a trace event ([traceability.md](traceability.md)).

## 4. Rooms

In a room the bot answers on a mention, or on every message with `reply =
"always"` ([identity.md](identity.md#unknown-senders-the-access-knobs)).

- **The transcript fills between mentions.** A message that mentions nobody
  starts no turn, but it is stored; the next turn sees the last 30 of them.
- **Verified senders.** Each message in a room prompt is prefixed with its
  sender's sanitised name, so a display name can't forge a section of the
  prompt.
- **Passing.** With `reply = "always"`, a turn whose messages mention nobody
  is told it may answer exactly `NO_REPLY`; that answer is stored and never
  delivered. A mention is always answered.

## 5. Messages and attachments

A message is markdown text, zero or more attachments, and optionally the
message it replies to. Attachments are the only way files and media travel,
in either direction: an image is always an attachment, and a URL in the text
stays text.

```
Message    { text, attachments: [Attachment], reply_to? }
Attachment { id, blob_id, mime, name, size, sha256,
             kind: image | audio | video | document,
             derived: { text, by: transcribe | vision }? }
```

- **`kind` comes from the MIME type**, decided by HolaClaw, never by the
  sender. The channel or app decides how to show it: a `report.md` is a
  `document`, sent as a file on Telegram and previewed in an app. An
  `audio/ogg; codecs=opus` file goes out as a voice note where the platform
  has them.
- **Blobs** are content-addressed. Clients upload with `POST /v1/blobs`, or
  as multipart on send; a blob is kept as long as the messages that use it.
- **Towards the model**, every attachment yields a one-line stub, then:
  - an image goes as an image when the chat model reads images, else as its
    description;
  - audio goes as its transcript;
  - a text-like document is inlined, up to 6000 characters each and 12000
    per turn;
  - anything else is reached through the `files` tool.

  The description and the transcript are made once and stored with the
  message ([models.md §6](models.md#6-images-and-audio)).
- **From the agent**, a file travels as an attachment on its reply: `attach`
  adds a workspace file, `image.generate` a generated image. The model's text
  is never parsed for media.
- **Markdown is canonical.** Each channel converts it to its own format and
  splits long replies. A channel declares the kinds and sizes it takes; an
  attachment it can't deliver becomes a text notice and a
  `delivery.degraded` event.

## 6. Replies and reactions

- **The reply goes back where the message came from**: the same channel
  instance, conversation and thread. The target comes from the message's
  origin, never from the model. The API delivers to API clients and lets an
  app watch other conversations.
- **Native replies.** In rooms, and in DMs too with `reply_to = "always"`,
  the answer is a platform reply to the message that started the turn: a
  Telegram reply, a Slack thread, a Google Chat quote. Notices and task
  results are plain messages, and so is an answer whose message is gone.
- **Reactions.** On a channel that takes them, `react(emoji)` puts one emoji
  on the turn's message; a turn that reacted and wrote nothing sends nothing
  else. A person's reaction to a bot message is stored as
  `[👍 on: <its first 80 characters>]` from that person, and starts no turn.
- **One message per reply.** A typing indicator shows while the turn runs,
  where the platform has one, and the reply is sent once it is done; replies
  are not streamed into an edited message. `holaclaw chat` and the apps
  stream the text as it is written, over the API.

## 7. Inbound delivery

Every channel delivers at least once, so the same message can arrive twice:
a redelivery, a resend, a restart before the acknowledgement. Before it
handles a message, the access gate records its dedupe keys (hashed, kept 14
days); a message seen before is dropped as `router.dropped` with reason
`duplicate`, and acknowledged to the platform. What each channel does on top
is in its own doc: [Telegram](channels/telegram.md),
[Slack](channels/slack.md), [Google Chat](channels/googlechat.md),
[WhatsApp](channels/whatsapp.md).
