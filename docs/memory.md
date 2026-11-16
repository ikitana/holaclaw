# Memory

What the agent remembers across conversations: who can read a memory, who
can write or forget one, how recall summaries are made, and how memory
follows an identity across its bindings.

This document describes memory once
[#147](https://github.com/ikitana/holaclaw/issues/147) has landed. Identities and
bindings are defined in [identity.md](identity.md). The contract is
`internal/memory/memory.go`.

## Contents

1. [Concepts](#1-concepts)
2. [Partitions](#2-partitions)
3. [Reading](#3-reading)
4. [Saving notes](#4-saving-notes)
5. [Forgetting](#5-forgetting)
6. [Recall](#6-recall)
7. [Tasks](#7-tasks)
8. [Lifecycle](#8-lifecycle)
9. [Examples](#9-examples)
10. [CLI and API](#10-cli-and-api)
11. [Limits and out of scope](#11-limits-and-out-of-scope)

## 1. Concepts

| Term | Meaning |
|---|---|
| **Agent** | Memory belongs to one agent. Nothing crosses agents. |
| **Layer** | `note`: a fact saved on purpose with `memory.save`. `recall`: a summary of a conversation, written by code. |
| **Partition** | The space a memory lives in: one per user identity, one per room identity. |
| **Visibility** | `private` (the default): readable only from its partition. `shared`: readable from every partition of the agent, so in every DM and every room, pending identities included. Only notes can be shared. |
| **Provenance** | Columns written by code, never by the model: agent, partition, author identity, author role, session, trace, time, and whether the text is tainted. |

## 2. Partitions

| Conversation | Partition |
|---|---|
| A DM with a user identity, through any of its bindings, `holaclaw` included | `dm/<identity>` |
| A room | `room/<room identity>` |
| A task run | none: tasks have no memory (§7) |

A user identity bound on Telegram, Slack and `holaclaw` has one partition. A room
identity has its own, whoever speaks in it: nothing a user identity says in a
room reaches its DMs, and nothing from its DMs reaches a room.

## 3. Reading

A conversation reads **its own partition plus every shared note** of the
agent. The store applies this rule in SQL on every read path (automatic
recall, `memory.search`, get, forget, `GET /v1/memory`). Agent and partition
are mandatory, so no call can read everything.

- **Automatically.** Before each interactive turn, the daemon searches with
  the incoming text and injects up to 5 hits as `memory (layer, date): …`
  lines. Trivial messages ("hi", "ok", an emoji) skip the lookup. Tainted
  hits are wrapped as untrusted and taint the turn.
- **On demand.** `memory.search(query, about)` returns notes and recall mixed,
  each with its id.
- **Ranking.** Deterministic, with no model call: full-text search plus
  vectors when an embedder is configured, then recency decay (half-life 30
  days for recall, 120 for notes), then a diversity pass (MMR).

## 4. Saving notes

`memory.save(text, about?, visibility?)` stores one fact.

- **Private by default** for every role, in every context.
- **`visibility: "shared"`** is accepted from an owner or admin only; anyone
  else gets an error. The tool description tells the model what sharing
  means: everyone this agent talks to can read it, in every DM and room,
  guests included. Share only facts meant for everyone, never anything
  personal.
- **Who may save.** Pending senders can't. A turn that answers several
  senders takes the narrowest role among them, so one guest in a coalesced
  room turn blocks saving for that turn.
- **Budget.** Notes share a budget of 2000 tokens per agent across all
  partitions. Over budget, the save fails and the model is told to merge or
  forget first. Recall doesn't count against it.

## 5. Forgetting

`memory.forget(id)` deletes one memory:

| Item | Who can forget it |
|---|---|
| A private note or recall row | Anyone reading its partition, except pending senders |
| A shared note | Only the owner or admin who wrote it |

Anyone can read a shared note, but nobody else can delete it.

## 6. Recall

Recall is short-term memory: summaries of past conversations. It lives in
the same store and index as notes, with three differences: it is always
private, code writes it, and it fades faster in ranking.

- **When it is made.** When a session is compacted, which happens once its
  history fills a set share of the context window
  ([context.md §4](context.md#4-compaction)). The compaction summary
  (goal, decisions, open items, files, preferences) is stored as a recall row
  in the session's partition. A conversation that never compacts leaves no
  recall.
- **Duplicates.** Text identical to a row already in the partition is skipped.
- **Never for tasks.**
- **A pending identity's recall** stays in its `dm/` partition and moves with
  it on assign.

## 7. Tasks

Task runs have no memory: no `memory.*` tools and no recall
([tasks.md](tasks.md)). The agent that creates a task writes whatever the
task needs from memory into its prompt. An unattended run gets fewer tools,
and no partition fills up that nothing reads.

## 8. Lifecycle

| Event | Effect on memory |
|---|---|
| Session deleted | Its recall rows are forgotten. Notes stay. |
| Identity deleted | Its `dm/` partition is forgotten (private notes and recall, every agent) unless `--keep-memory` is passed. Shared notes it wrote stay, with their author. Notes it wrote in rooms stay with the room identity. |
| Pending identity assigned | Its `dm/` partition moves to the target. A recall row the target already holds is dropped, and rows written by others keep their author. |
| Binding removed | Nothing moves. The memory stays with the identity. If a pending identity is left with no bindings, it is deleted with its memory. |
| Time | Decay only lowers the ranking. Nothing expires. |

## 9. Examples

**The cast.** Ana is the owner, bound on Telegram and `holaclaw`. Bob is an admin.
Carla is a user, bound on Telegram and Slack. Dan is a pending identity.
**Team** is a Telegram group, **#ops** a Slack channel.

| What happens | Stored | Who sees it |
|---|---|---|
| Ana, in a DM: "remember I'm allergic to peanuts" | private note in `dm/ana` | Ana, in her DMs on any channel and in the app |
| Ana, in a DM: "remember: the office closes on Fridays", shared | shared note in `dm/ana` | everyone, everywhere, Dan included. Only Ana can forget it. |
| Carla, in a Slack DM: "answer me in Spanish" | private note in `dm/carla` | Carla, on Slack, Telegram and the app. Not in rooms. |
| Bob, in #ops: "we deploy on Thursdays" | private note in `room/ops` | anyone talking in #ops. Not Bob's DMs. |
| Carla, in Team: "remember I'm vegetarian" | private note in `room/team` | anyone in Team. Not Carla's DMs. |
| Carla, in a DM, asks to forget Ana's shared note | refused | |
| Dan tries to save | refused | |
| Ana and Dan mention the bot in Team in one turn | the turn runs as pending: no saves | |
| Ana's DM session compacts | recall row in `dm/ana` | Ana only |
| A task Ana scheduled runs | nothing read or written | |

## 10. CLI and API

```
holaclaw memory search <query> --agent A --partition P [--layer note|recall] [--limit N]
holaclaw memory add <text> --agent A --partition P [--about X] [--visibility private|shared]
holaclaw memory forget <id> --agent A --partition P
```

`GET POST /v1/memory`, `GET /v1/memory/partitions` and
`DELETE /v1/memory/{id}` need `admin` scope, apply the same rules, and act as
the caller's identity. The memory explorer shows partitions by identity name.

## 11. Limits and out of scope

**Recall limits**, kept as they are for now:
- Each compaction summary includes the previous one, so successive recall
  rows of a session overlap and compete for the 5 slots.
- Nothing is summarised at session end, and recall has no retention, cap or
  merge.
- `memory.search` can't filter by layer or date.
- A compaction summary ends with "Older messages are searchable with
  memory.search", but only summaries are searchable, never the transcript.

**Out of scope:**
- **A personal note said in a room following its writer into other rooms** (an
  origin rule). Today a room's notes belong to the room identity.
- **A `sensitive` flag** for keeping a note said in a room out of other
  places.
- **Extra global writers per agent** (`memory.global_writers`).
