# Decision models: setup and use

This guide adds a decision model, such as Jev, to HolaClaw and gives it to an
agent: as the guard of its tool calls, as the scorer of untrusted content,
and as the model that answers the browser's questions. It is for whoever runs
HolaClaw. The design behind it is in
[models.md §7](../models.md#7-decision-models); you don't need it to follow
this guide.

**Why.** An agent that acts on its own needs a second opinion on what it is
about to do, and on what it has just read: is this command safe to run, is
this web page trying to give the agent orders, did that click do what was
meant. A chat model can give that opinion, but it takes seconds and a chat
call's price each time, and its answer is text that has to be read back. A
**decision model** only picks an answer from the ones it is offered, in about
0.4 s and for about $0.00002 a call, so it can be asked on every tool call
without slowing the agent down.

**When.** Add one when an agent:

- runs tools you don't want to approve one by one, and shouldn't run
  unchecked either: commands, writes, calls to outside services (§4);
- reads the web, connectors or other content it shouldn't take orders from,
  and holds anything private (§5);
- uses the browser, where it is the difference between one fast call and a
  chat turn per step (§6).

An agent that only chats, or whose every tool call you approve yourself,
doesn't need one.

## Contents

- [1. Requirements](#1-requirements)
- [2. Add the model](#2-add-the-model)
- [3. Give it to an agent](#3-give-it-to-an-agent)
- [4. Guard tool calls](#4-guard-tool-calls)
- [5. Score untrusted content](#5-score-untrusted-content)
- [6. Answer the browser's questions](#6-answer-the-browsers-questions)
- [7. Check and apply](#7-check-and-apply)
- [8. See it work](#8-see-it-work)
- [9. When a call fails](#9-when-a-call-fails)
- [10. What leaves the machine](#10-what-leaves-the-machine)

## 1. Requirements

- **HolaClaw installed and initialised**: see the
  [deployments](../deployments/README.md). Commands are written as
  `holaclaw …`; run them
  [the way your deployment does](../deployments/README.md#run-a-command).
  The files edited here are in the
  [config directory](../deployments/README.md#config-directory).
- **An endpoint that serves decision models**, and its API key if it needs
  one. This guide uses OpenRouter's alpha decisions API, the one HolaClaw is
  run against. Ollama (0.35 or newer) documents the same request and reply
  at `/v1/systemone`; HolaClaw has not been run against it (§2).

## 2. Add the model

A decision model is a `[models.<name>]` of `config.toml` with
`api = "decisions"`, on an `openai-compatible` provider that holds the key:

```toml
# config.toml
[providers.openrouter]
type     = "openai-compatible"
base_url = "https://openrouter.ai/api/v1"
auth     = "api-key"

[models.jev]
provider = "openrouter"
id       = "~typesafe/jev-latest"
api      = "decisions"
url      = "https://openrouter.ai/api/alpha/decisions"
```

- **`url`** is required: the decisions API has no standard path under the
  provider's `base_url`, so the model names its endpoint in full.
- **`provider`** only lends its key and its `max_concurrent` (2 calls in
  flight when unset). It must be `openai-compatible`: a `codex` provider
  serves no decision models. If your chat model is already on OpenRouter,
  reuse that provider.
- **`context`, `tools` and `vision`** don't apply, and are left out.
- **`timeout`** is optional. A decision call is given 2 seconds by default.

`jev` is only a name: the roles below refer to it.

**Another endpoint.** HolaClaw posts the questions to whatever `url` names,
so any endpoint that takes the same request and gives the same reply can
serve the model. Ollama's `/v1/systemone` is documented as one, which would
make a local decision model a `[models.<name>]` on your Ollama provider with
`url = "http://localhost:11434/v1/systemone"` and one of its decision models
as `id`. That pairing is untested: try it with `guard_mode = "recommend"`
first, where a failed call only means you are asked (§9). Ollama reports no
price, so its calls count as unpriced in a trace.

Store the key, unless the provider already has one. The command reads it from
stdin:

```
printf %s "$KEY" | holaclaw auth set providers.openrouter api_key
```

## 3. Give it to an agent

A model does nothing until an agent gives it a role, under `[models]` in the
agent's `agents/<name>/agent.toml`. A decision model fits two roles, and one
model can fill both:

```toml
# agents/main/agent.toml
[models]
chat   = "default"
guard  = "jev"
decide = "jev"
```

| Role | What the model does | Section |
|---|---|---|
| `guard` | scores each tool call the policy doesn't settle | §4 |
| `guard`, else `decide` | scores each untrusted tool result | §5 |
| `decide` | answers the browser's questions about each move | §6 |

`decide` accepts only a decision model. `guard` also accepts a chat model,
which is slower and answers only allow or ask. Any other role given a
decision model is a config error.

## 4. Guard tool calls

With `guard` set, each tool call the policy doesn't settle is scored, 0 to 1,
for how safe it is to run without asking, and labelled `benign`,
`destructive`, `exfiltration`, `escape` or `injection`. `read` calls are
never sent to it, and it never lifts a `deny`.

```toml
# agents/main/agent.toml
[policy]
guard_mode      = "recommend"  # recommend | auto
guard_threshold = 0.7
```

- **`recommend`** (the default) only adds questions: a call the policy would
  allow is asked about when it scores below the threshold, and the verdict is
  shown in the approval prompt. Start here.
- **`auto`** answers for you: a call at or above the threshold runs, and one
  below it is denied, or asked when it is `benign`. It needs a `guard` role.

The full rules, and what `auto` never does, are in
[security.md §6](../security.md#6-the-guard-model).

## 5. Score untrusted content

There is nothing to configure: an agent with a decision model as `guard`, or
else as `decide`, has its untrusted tool results scored. Each web page,
browser answer and connector result is graded once for whether it tries to
steer the agent. One at or above `policy.taint_threshold` (0.5) taints the
next tool calls of the turn.

Without a decision model every untrusted result counts as steering, so a turn
that fetched one ordinary page is tainted. With one, only content that reads
like an instruction is.

Tainting changes what the agent may do only with this, which is off by
default:

```toml
# agents/main/agent.toml
[policy]
taint_escalates = true   # a tainted exec or external call is asked, not allowed
```

Turn it on for an agent that reads the web and holds anything private. The
details are in [security.md §7](../security.md#7-untrusted-content).

## 6. Answer the browser's questions

With the [browser](../browser.md) on, `decide` is the model that binds each
move to an element of the page and verifies that it worked: several questions
per move. Without it the chat model answers them, one chat call each, and
`holaclaw check` warns with `BRW006`.

```toml
# agents/main/agent.toml
[models]
decide = "jev"
```

## 7. Check and apply

```
holaclaw check
```

`check` reports, as `CFG` errors, a decisions model without `url`, a role
that names an unknown model, a role given a model of the wrong API, and
`guard_mode = "auto"` without a `guard` role. `SEC002` reports a provider
whose key is missing.

It doesn't verify the `id` of a decision model: `MOD003` only checks the
models served at their provider's `base_url`. A wrong id shows up as a failed
call (§9).

The daemon reads its config only at start, so
[restart](../deployments/README.md#restart) it.

## 8. See it work

Ask the agent for something that calls a tool, then open the trace of the
turn:

```
holaclaw chat --trace
holaclaw trace <id>
```

| Event | What to look for |
|---|---|
| `guard.decision` | one per guarded call: `tool`, `mode`, `score`, `decision`, the label in `reason`, the call's tokens and cost |
| `tool.result` | `taint_score` on an untrusted result, with the scorer's tokens and cost |
| `browser.task.started` | `decider` is `decision`, not `chat` |
| `browser.task.finished` | `decision_calls`, and the usage of the whole run |

A verdict is cached for 10 minutes, so a repeated call shows a
`guard.decision` without usage: it wasn't asked, or paid for, again.
`holaclaw trace <id>` sums the turn's cost, decision calls included
([traceability.md](../traceability.md)).

## 9. When a call fails

The decisions API is an alpha, and its transport is experimental: a call is
never retried, and a reply that doesn't match the questions asked is thrown
away. Each caller fails safe, so a broken decision model makes the agent ask
more, never less:

| Caller | On an error or a timeout |
|---|---|
| The guard, `recommend` | the call is asked |
| The guard, `auto` | the call is denied |
| The taint scorer | the result counts as steering |
| The browser | the answer counts as unreadable |

A failed guard call is a `guard.decision` whose `reason` starts with
`guard error`, followed by the kind of error. If every call fails, check the
`id` and `url` of the model and the provider's key. The daemon's
[log](../deployments/README.md#logs) has the provider's own error.

## 10. What leaves the machine

A decision model on a cloud endpoint is sent, on every call:

- **as the guard**: the tool's name, description and class, and up to 1500
  bytes of the call's arguments (paths, URLs, commands);
- **as the taint scorer**: the untrusted result itself, up to 256 KiB;
- **for the browser**: the request, an excerpt of the page and the run's
  history.

Give the roles only to agents whose tool calls and pages may go to that
provider.
