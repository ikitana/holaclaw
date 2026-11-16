# Context window

How holaclaw spends a model's context window: what goes into a prompt, what
is cut when a turn does not fit, how compaction shortens a session, and where
that stops working. It matters most for small windows (a local model at 8k).

## Contents

1. [Concepts](#1-concepts)
2. [Building a prompt](#2-building-a-prompt)
3. [Fitting history](#3-fitting-history)
4. [Compaction](#4-compaction)
5. [Stops and warnings](#5-stops-and-warnings)
6. [Limits](#6-limits)
7. [Sizing a small window](#7-sizing-a-small-window)
8. [Thinking models](#8-thinking-models)
9. [Commands and events](#9-commands-and-events)

## 1. Concepts

### Where the window goes

```
window = system + tool schemas + connectors + reply reserve + volatile + history
```

| Part | Holds | Sized by | Cut |
|---|---|---|---|
| **System** | the core prompt, the persona, the skills index ([skills.md §4](skills.md#4-in-the-prompt)) | core ≤ 300 tokens, persona ≤ ¼ of the window, skills index ≤ ⅛ | never: over a cap, the turn fails (§5) |
| **Tool schemas** | one schema per built-in tool the agent is granted | ≤ 150 tokens each | never: over the cap, the turn fails |
| **Connectors** | every connector tool schema, plus the `connectors` fragment naming them ([connectors.md §8](connectors.md#5-tools-the-agent-sees)) | the servers' schemas | never |
| **Reply reserve** | room for the answer, and for a thinking model its reasoning (§8) | `limits.reply_reserve`, 1024 by default, never more than ¼ of the window | never |
| **Volatile** | this turn's injections, such as the time and recalled memory ([memory.md §3](memory.md#3-reading)) | what is injected | never; counted before history |
| **History** | the session's transcript since its last compaction | what is left | the only part that is cut (§3) |

**`models.<name>.context` must be the window the server actually runs**, not
the model's maximum or a guess. If it is lower than the server's real window
(for example `context = 8192` while Ollama runs with `num_ctx = 131072`),
holaclaw cuts prompts for no reason. If it is higher, the server rejects the
prompt, or silently drops part of it.

### Terms

| Term | Meaning |
|---|---|
| **Fixed overhead** | System plus built-in tool schemas: what every turn pays however short the conversation. Connectors are counted apart. `holaclaw prompt-size` and `BUD001` use this term. |
| **Group** | A message, or a tool call with all its results. A group is kept or dropped whole: a call is never split from its results. |
| **Newest turn** | The newest user message and everything after it: the tool steps of the running turn. |
| **Bulk** | What may be cut from the newest turn: the text of the user message, and every tool result of the turn. |
| **Glue** | What is never cut: tool-call messages, the assistant's text between steps, and images. |
| **Floor** | The least a piece of bulk keeps when it is cut. |

## 2. Building a prompt

Every model call builds its prompt in the same steps:

```
1. system      core, persona, skills index; any over its cap: the turn fails
2. schemas     built-in tools (any over 150 tokens: the turn fails), then connector tools
3. reserve     limits.reply_reserve, or 1024; at most window/4
4. min tail    system + schemas + connectors + reserve + 512 ≥ window: "window too small"
5. volatile    this turn's injections, taken off what is left
6. history     the newest whole groups that fit in the rest (§3)
7. compaction  requested if a group was dropped or cut, or history is over 70% of the rest
```

- **The system prompt and the schemas are a stable prefix.** Nothing in them
  changes within a session, not even the date, so a provider's prompt cache
  and a local server's KV cache can reuse them. Their hash is `prefix_hash`
  on `context.built`.
- **The volatile part goes into the last user message**, after a
  `--- context ---` line, never into the system prompt.
- **A compaction summary is replayed as a user message** starting with
  `[summary] `.

## 3. Fitting history

History is kept newest first, whole groups at a time. Older groups that do
not fit are dropped, and compaction is requested.

The newest turn is always kept. When it alone does not fit, holaclaw cuts its
bulk and never its glue. One common cap is lowered over all the bulk, largest
first, until the turn fits. Anything already under the cap is left alone, so
a one-line question or a small result is never touched.

| Part | Floor | How it is cut |
|---|---|---|
| Tool result | 128 tokens (`MinResultTokens`) | keeps its head, then `[… full result truncated, N tokens]`, where N is the size of the stored result. An untrusted result is wrapped in `<untrusted>` again ([security.md](security.md)). A connector's `[result cut at N bytes]` at the end of the stored result is kept after the pointer |
| User text | 256 tokens (`MinUserTokens`), on top of its images | loses its middle, replaced by `[…truncated]` on its own line, so a question asked before or after a paste survives |

The cut exists only in the prompt. The transcript keeps the full text, and
the cut requests compaction.

Results arrive already capped by their tool, in bytes, before they are
stored: 4 MiB for a connector, 16 KiB for `files`, 8000 bytes for
`browser.do`, 1500 bytes for `memory.search`.

## 4. Compaction

Compaction replaces the oldest part of a session's transcript with a summary.
It runs right after a turn whose prompt requested it (§2), inside the
session, so it never overlaps the session's next turn. A failure is logged
and never fails the turn: the reply is already out. Each session compacts
on its own ([identity.md §4](identity.md#4-sessions-and-what-each-token-sees)
lists the sessions).

```
1. floor      window − system − schemas − connectors − reserve − 512 under max(1024, 15% of window):
              fails with window_too_small
2. counter    3 failed or ineffective attempts since the last commit: off for the session
3. trigger    history under 70% of window − system − schemas − connectors − reserve: nothing to do
4. re-arm     after a commit, history must grow by 20% of that budget before the next attempt
5. cut        the newest whole groups from a user message, at least 512 tokens, are kept
6. stage 1    on what is summarised: results over 2000 tokens cut (the full text kept in a blob),
              a repeated result replaced by a pointer to the first, images dropped except in
              the newest 2 user messages
7. stage 2    one summarize call
8. commit     only if it reclaims at least 25% of the history; otherwise "ineffective"
```

- **The summary** is the model's text, an index of the ids, paths, URLs and
  error strings found in what it covers, a note naming the skills to reload
  ([skills.md §4](skills.md#4-in-the-prompt)), and the line *Older messages
  are searchable with memory.search*.
- **It is untrusted** when anything it covers was, so compaction cannot
  launder injected content into trusted context.
- **It becomes recall.** The summary is stored as a recall row in the
  session's memory partition ([memory.md §6](memory.md#6-recall)), except
  for tasks.
- **Nothing is deleted.** The transcript stays in the store; the session's
  compaction point moves, and the next prompt starts from the summary.
- **The summarize call** uses the agent's `summarize` model, or `chat`. Its
  reply reserve is 1024 tokens, at most a quarter of that model's window.
  A transcript too large for its window loses its middle.
- **Turned off** after three failures, a session no longer compacts: old
  history is only dropped (§3).

## 5. Stops and warnings

| Condition | Where | Effect |
|---|---|---|
| Core prompt over 300 tokens | every call | the turn fails: `context budget: core is N tokens, cap 300` |
| Persona over ¼ of the window | every call | the turn fails (`persona`) |
| Skills index over ⅛ of the window | every call | the turn fails (`skills`) |
| A built-in tool schema over 150 tokens | every call | the turn fails (`tool schema <name>`) |
| System + schemas + connectors + reserve + 512 ≥ window | every call | the turn fails (`window too small`) |
| Compressible budget under max(1024, 15% of the window) (§4) | compaction | compaction fails with `window_too_small`; turns still run, and old history is only dropped |
| Fixed overhead over ¼ of the window, or any of the failures above | `holaclaw check`, `BUD001` | error |
| Fixed overhead over 1500 tokens | `holaclaw check`, `BUD001` | warning |
| Connectors over ¼ of the window | `holaclaw check` and `prompt-size`, `BUD002` | warning |

The rules are listed in [check-rules.md](reference/check-rules.md).

## 6. Limits

- **When even the floors do not fit, the floors win.** The prompt goes over
  the history budget, into the reply reserve, rather than lose the question.
  The model then has less room to answer, and a large enough turn goes over
  the window itself. Twelve results at their floor already cost about 1500
  tokens. A turn like that means the window is too small for the tools the
  agent was granted.
- **A cut result cannot be read back.** The pointer tells the model that the
  result was cut and by how much, but no tool returns the rest. `files.read`
  can read a file again by range (`offset`, `limit`); any other tool can only
  be called again, which adds bulk and lowers the common cap further.
- **The cut changes at every step.** Each tool step adds bulk, so the cap goes
  down, and earlier results and the user's text are cut again, harder. From
  the newest user message onwards the prompt changes every step: the cache is
  not reused past it, and the model sees a slightly different question each
  step.
- **Compaction does not shrink the running turn.** It summarises older
  history and always keeps the newest turn. A single turn that overflows will
  overflow again on the next step.
- **Only plain results are cut.** A tool result with attachments, or one that
  names a sender, is glue: it is never cut.
- **Images are never cut.** Each image sent to a vision model costs a flat
  800 tokens, so a few images can fill a small window on their own.
- **Estimates, not exact counts.** Tokens are estimated from the text, not
  counted with the model's tokenizer, so a cut turn can land a few tokens
  over the budget.

## 7. Sizing a small window

An example: window 8192, 38 tool schemas of which 5056 tokens were connector
tools, and a 1024-token reply reserve. That left about 500 tokens of history.
Two connector results (about 1100 tokens) already did not fit.

- **Set `context` to the window the server runs.** Check it first: it is the
  cheapest fix.
- **Keep the fixed overhead and the connectors under a quarter of the window
  each** (`BUD001`, `BUD002`). Together that still leaves half for the
  reserve, the volatile part and history. `holaclaw prompt-size --agent
  <name>` prints an agent's fixed overhead.
- **Leave room for a real turn**: a question plus two or three results of a
  few hundred tokens each, plus 800 tokens per image.
- **Grant a small model only the connectors it needs.** Every connector tool
  is paid on every turn, whether it is called or not.
- **For a thinking model**, see §8.

## 8. Thinking models

A thinking model spends its reasoning from the same `max_tokens` as its
answer. For the chat role that is the reply reserve; for the summarize call
it is 1024 tokens at most (§4). With thinking on, a long answer can come back
empty (`finish: length`, no text) at any window, and a summarize call that
thinks fails with "summarize model returned no text", which counts towards
the three failures that turn compaction off.

`models.<name>.reasoning` sets the effort of every call to that model:

| Value | Effect |
|---|---|
| unset | the model's default |
| `"none"` | thinking off |
| `"minimal"`, `"low"`, `"medium"`, `"high"`, `"xhigh"`, `"max"`, `"ultra"` | passed through; the model may refuse a level it does not support |

The levels are the union of what OpenAI, OpenRouter and Ollama accept, and
the config refuses any other value. holaclaw does not check the value against
the model, so a level the model does not support fails every call to it.

- **A call that sets its own effort keeps it.** Every call of the browser
  executor does: a move `low`, the rest `none`.
- **On the wire**, every provider but `codex` sends the effort twice: as
  `reasoning_effort`, which OpenAI reads, and as `effort` in OpenRouter's
  `reasoning` object (with `enabled: false` for `none`), which Ollama's `/v1`
  reads when both are present. `codex` does not send it.

To keep thinking for chat but not for compaction, declare the same model
twice:

```toml
# config.toml
[models.local]
provider = "ollama"
id       = "hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL"
context  = 147456
tools    = true

[models.local-fast]
provider  = "ollama"
id        = "hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL"
context   = 147456
tools     = true
reasoning = "none"
```

```toml
# agents/main/agent.toml
[models]
chat      = "local"
summarize = "local-fast"
```

Thinking on for chat still needs a `reply_reserve` large enough for the
reasoning and the answer.

## 9. Commands and events

```
holaclaw prompt-size [--agent A]    the prompt's parts offline, with no history, and BUD002
holaclaw check                      BUD001 and BUD002, among the other rules
```

`prompt-size` prints one row per fragment (`core`, `persona`, `skills`,
`connectors`, `tool:<name>`, `connector:<id>.<tool>`), then the totals:
system, tool schemas, connectors, fixed overhead, history, volatile, reply
reserve, total and window. See [cli.md](reference/cli.md).

At run time, every prompt emits `context.built` with the same numbers, and
the terminal UI draws them as the context bar under the prompt box.

| Event | Carries |
|---|---|
| `context.built` | `window`, `system`, `tool_schemas`, `connectors`, `history`, `volatile`, `reply_reserve`, `total`; each fragment's name, tokens and hash; `prefix_hash`; the message and memory ids in the prompt; `needs_compaction` |
| `context.compacted` | `outcome` (`committed`, `failed`, `ineffective` or `disabled`), `reason` and `detail`; `before`, `after` and `reclaimed` history tokens; `summarised_messages`, `summary_msg_id`, `compaction_point`, `anchors`; `compact_fails`; the summarize call's usage |

The events are listed in [events.md](reference/events.md), and the keys
(`models.<name>.context` and `models.<name>.reasoning` in `config.toml`,
`limits.reply_reserve` in `agent.toml`) in [config.md](reference/config.md).
