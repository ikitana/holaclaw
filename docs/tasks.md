# Tasks

Work the agent does detached from a conversation: now, at a set time, or on a
cron schedule. "Research X and get back to me", "remind me on Friday at 17:00
to send the invoice", "every weekday at 08:00, summarise my notifications".

Who may do what in a run is in [security.md](security.md). How tasks fit
the rest of HolaClaw: [ARCHITECTURE.md](../ARCHITECTURE.md#2-the-big-picture).

## Contents

1. [Concepts](#1-concepts)
2. [Creating a task](#2-creating-a-task)
3. [Schedules](#3-schedules)
4. [Runs](#4-runs)
5. [Delivery](#5-delivery)
6. [Status and lifecycle](#6-status-and-lifecycle)
7. [API and events](#7-api-and-events)
8. [Limits](#8-limits)

## 1. Concepts

| Term | Meaning |
|---|---|
| **Task** | A row in `tasks`: agent, name, prompt, schedule, delivery target, missed-run policy, step limit, status. Rows are never deleted: traces refer to them. |
| **Schedule** | `now` (once, right away), `at` (once, at a time) or `cron` (recurring). |
| **Run** | One occurrence. The prompt is submitted as a user message to the task's session, under a new trace. |
| **Task session** | One per task, keyed `<agent>/task/<task id>`, with the task id as its session id. Every run of the task reuses it. |
| **Delivery** | Where a run's reply goes: `session` (the chat the task came from) or `store` (kept in the task session only). |
| **Creator** | The identity that created the task. Runs act as the creator. |

## 2. Creating a task

Two paths, one validator.

| | The agent, with `task.create` | An admin, with `POST /v1/tasks` |
|---|---|---|
| Agent | the calling agent | any configured agent, `main` by default |
| Delivery | the calling session, always | any chat session (`session`) or `store`; `store` by default |
| Grants | none | tool names of that agent, each a [task-scoped grant](security.md) |
| Missed-run policy | `skip` | `skip` or `run_once` |
| Step limit | the agent's | `max_steps`, up to 50; `0` means the agent's |
| Cron | at most every 5 minutes | anything cron can express |
| Active tasks | at most 20 per user | no cap |
| Refused for | guests, and runs of a task | — |

**`task.create`** takes `prompt` (required), `name`, and at most one of
`cron`, `at` (RFC 3339) and `in_minutes` (1 to 525600). None means now. The
model supplies text and a time only: the agent, the user and the delivery
target come from the call, and the task carries no grants, so every `ask` in
its runs is a deny (§4). The reply tells the model the task id and its next
run.

**`POST /v1/tasks`** takes:

```json
{
  "agent": "main",
  "name": "notifications",
  "prompt": "Summarise my unread GitHub notifications.",
  "schedule": { "kind": "cron", "cron": "0 8 * * MON-FRI" },
  "delivery": { "kind": "session", "session_id": "01J…" },
  "grants": ["fetch"],
  "missed": "run_once",
  "max_steps": 20
}
```

A `session` delivery copies the conversation's origin from the session row,
never from the request. The session must be a chat session. The `channel`
kind in the schema is refused: a target not derived from a session row would
be one the creator chose freely.

Every task is a row created at runtime. Nothing in the config declares one.

## 3. Schedules

**`now`** runs at the next tick of the scheduler. **`at`** must be in the
future and at most a year ahead.

**`cron`** takes five fields, minute, hour, day of month, month and day of
week:

- each field takes `*`, lists (`1,15`), ranges (`1-5`) and steps (`*/15`,
  `1-20/3`);
- months take `JAN`-`DEC`, days of week `SUN`-`SAT`, and `0` and `7` are both
  Sunday;
- `@hourly`, `@daily`, `@weekly` and `@monthly` are shortcuts;
- when both day fields are restricted, a day matching either fires, as in
  every cron;
- `0 9 31 * *` fires only in months that have a 31st;
- an expression that matches nothing in the next five years (`0 0 30 FEB *`)
  is refused.

Cron is read in the daemon's time zone and follows its wall clock across DST:
a time the spring change skips runs at the shifted instant, and a time the
autumn change repeats runs once.

## 4. Runs

The scheduler is one goroutine. It sleeps until the earliest `next_run`, a
minute at most, so a suspended host or a stepped clock is noticed. It claims
each due occurrence with a conditional `UPDATE`, so the same occurrence never
runs twice, and moves `next_run` to the following one.

**Overlap.** If the task's previous run is still going, the occurrence is
dropped, not queued, and `task.missed` records `reason=overlap`.

**Lane.** A run is a turn of a task-kind session, so it runs on the task lane:
at most 1 task turn per provider, beside 4 interactive ones. A busy task lane
never takes a chat's slot.

**What a run has:**

| | In a task run |
|---|---|
| Identity and role | the creator's |
| Policy | `policy.task` ([security.md](security.md)); `exec` is `deny` by default |
| `ask` | a deny, unless a grant of this task answers it; nobody is asked |
| Memory | none: no recall and no `memory.*` tools ([memory.md §7](memory.md#7-tasks)) |
| History | the task session: a recurring task's runs see the earlier runs, bounded by [compaction](context.md#4-compaction) |
| Skills | `skill.load`; `skill.save` and `skill.forget` are not offered |
| `react` | not offered: a run answers no message |
| `task.create` | refused: a task can't create a task |
| `browser.do` | runs detached ([browser.md](browser.md)) |
| Steps | the task's `max_steps`, or the agent's limit |

A task has no memory, so the agent writes whatever the task needs into the
prompt when it creates it.

A run cut short by a daemon stop goes back to the session's mailbox and is
replayed at the next start, like any interrupted turn.

## 5. Delivery

**`session`.** The reply is appended to the origin chat session as an
assistant message, `[task <name>] <reply>` (the task id when it has no name),
and pushed through that session's channel like any reply. A session with no
channel to push through, such as one opened over the API, gets a `reply`
event carrying the task id instead. A run with an empty reply delivers
nothing.

**`store`.** The reply stays in the task session. Read it there, or follow the
task's `last_trace_id`.

The target is fixed at creation and comes from a session row: a model can't
redirect a task's output.

A failed run is not told apart from a successful one yet: the turn's apology
is delivered as the result ([#199](https://github.com/ikitana/holaclaw/issues/199)).
An aborted run delivers nothing and emits no `task.finished`
([#200](https://github.com/ikitana/holaclaw/issues/200)).

## 6. Status and lifecycle

| Status | Meaning |
|---|---|
| `active` | armed, or recurring |
| `done` | a one-shot task whose run was submitted |
| `failed` | a one-shot task whose time passed while the daemon was down, under `skip`; or a task whose cron expression no longer parses |
| `cancelled` | deleted over the API, or its creator was deleted |

A one-shot task becomes `done` as soon as its run is submitted, before the run
finishes.

**Missed runs.** At start, the daemon applies each task's missed-run policy to
what came due while it was down, and emits `task.missed` with the count:

- `skip` forgets the missed occurrences and arms the next one. A one-shot task
  whose time passed becomes `failed`.
- `run_once` runs one catch-up run right away, never a burst.

**Creators.** A disabled creator's tasks don't run: to the scheduler they are
in downtime. Enabling the creator applies the missed-run policy first, as a
restart does ([identity.md](identity.md)). Deleting the creator cancels their
tasks and revokes their grants.

**Cancelling.** `DELETE /v1/tasks/{id}` sets `cancelled` and revokes the
task's grants. It doesn't abort a run already going; abort that run through
its session.

## 7. API and events

All four endpoints need the `admin` scope:

| Endpoint | |
|---|---|
| `GET /v1/tasks` | every task, newest first |
| `POST /v1/tasks` | create (§2) |
| `GET /v1/tasks/{id}` | one task, with its grants |
| `DELETE /v1/tasks/{id}` | cancel (§6) |

A task carries its `next_run`, `last_run` and `last_trace_id`.

| Event | Trace | Attributes |
|---|---|---|
| `task.scheduled` | the creator's turn, if any | `task`, `agent`, `schedule`, `delivery`, `next_run`, `grants` (a count) |
| `task.started` | the run | `task`, `agent`, `trace`, `parent_trace` |
| `task.finished` | the run | `task`, `status` (`ok`), `delivery` |
| `task.missed` | the creator's turn, if any | `task`, `count`, `policy`, and `reason=overlap` for an overlap |

## 8. Limits

| | |
|---|---|
| Prompt | 8000 bytes |
| Name | 100 bytes, whitespace collapsed |
| `at` | at most a year ahead |
| `max_steps` | 50 |
| Cron from `task.create` | at most every 5 minutes |
| Active tasks per user, from `task.create` | 20 |
| Task turns per provider | 1 |
