# Traceability

How HolaClaw records what it did: one trace per inbound message, an event per
hop, and the usage and cost of every model call.

## Contents

1. [Traces and events](#1-traces-and-events)
2. [Usage and cost](#2-usage-and-cost)
3. [Reading a trace](#3-reading-a-trace)
4. [Retention](#4-retention)

## 1. Traces and events

Every inbound message, from a channel or the API, gets a `trace_id` (a ULID).
Every hop of its handling appends an event to that trace, with a `span_id`, a
`parent_id`, its type, its timing and its attributes. A turn that answers one
message reads like this:

```
channel.received → router.resolved → media.* → context.built → model.request
  → tool.call → policy.decision → approval.* → tool.result → model.response
  → reply → channel.sent
```

A task run, a compaction and a memory reindex get traces of their own.

- **One copy.** Messages, model replies, tool calls and results are stored as
  messages; events point at them by id rather than repeat them. A trace joins
  both.
- **Reconstructible prompts.** `context.built` lists the message ids, memory
  ids and fragment hashes a prompt was built from, with its token breakdown
  ([context.md](context.md)). The prompt itself is not stored.
- **What is never in an event:** a typed secret, a generated image's prompt,
  page text from the browser beyond titles and element names
  ([browser.md §9](browser.md#9-events-and-troubleshooting)), a screenshot.
- **The schema.** Events are append-only and schema-versioned, and field names
  follow OpenTelemetry. Types are added, never removed; consumers ignore
  fields they don't know. Every type is listed in
  [reference/events.md](reference/events.md).

## 2. Usage and cost

Every event that records a model call carries its usage:

| Attribute | |
|---|---|
| `prompt_tokens`, `completion_tokens` | always; `0` when the upstream reported none |
| `cost_nanousd` | the cost the upstream reported, as an integer of USD × 10⁻⁹, so every sum is exact |
| `cached_tokens`, `reasoning_tokens` | only when the upstream reported them |
| `unpriced_calls` | always: how many of the event's calls reported no cost |

A call without a cost is **unpriced**, never free: a local engine, a ChatGPT
subscription, a direct OpenAI key. A free model reports `0`. A call that was
not made, such as a cached guard verdict, writes no usage; a reply that was
answered and then thrown away still does, on the one event it gets (a dropped
stream on `model.error`, an empty description on `media.refused`).

**One accounting rule.** A trace's or a session's total sums the event types
that report each call exactly once (`internal/event.UsageTypes`):

| Counted | The calls |
|---|---|
| `model.response`, `model.error` | the agent loop's chat calls |
| `browser.task.finished` | a whole browser run: moves, extraction, vision, recovery and its decision questions |
| `guard.decision` | the guard's call |
| `tool.result` | the taint scorer's questions about that result |
| `context.compacted` | the summarize call |
| `media.described`, `media.transcribed`, `media.refused` | an attachment's vision or transcription call |
| `image.generated` | one generated image |
| `memory.embedded` | one embeddings call |

`run.finished` (the sum of a run) and `browser.move` (part of a browser run)
repeat numbers already counted and are left out. A total's cost covers the
priced calls, and its `unpriced_calls` says how many it doesn't. Over the API,
traces and sessions carry the total, with the cost as an exact decimal string
in USD (`"0.000123"`), absent when no call was priced.

## 3. Reading a trace

| Command | |
|---|---|
| `holaclaw trace` | browse recent traces |
| `holaclaw trace <id>` | every hop of one trace, with its timing, tokens and cost |
| `holaclaw trace <id> --json` | the same, as JSON |
| `holaclaw chat --trace` | print the trace id after each reply |
| `GET /v1/traces/{id}` | the trace over the API, with its messages and per-channel delivery |

`holaclaw chat -v` and `-vv` show a turn's events live as it runs.

## 4. Retention

`retention.events_days` (30 by default) is how long events are kept; `0` keeps
them forever. The ids of the messages the daemon sent, which route reactions back to
their turn, are kept as long. Deleting a session deletes its events, blobs and
recall with it.
