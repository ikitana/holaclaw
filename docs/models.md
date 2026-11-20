# Models

How HolaClaw uses models: the providers it talks to, the model profiles it
keeps, and the roles an agent gives them. To change the model `init` wrote,
see [getting-started.md §4](getting-started.md#4-change-the-model-or-the-provider).

## Contents

1. [Concepts](#1-concepts)
2. [Providers](#2-providers)
3. [Models](#3-models)
4. [Roles](#4-roles)
5. [The chat model](#5-the-chat-model)
6. [Images and audio](#6-images-and-audio)
7. [Decision models](#7-decision-models)
8. [Usage and cost](#8-usage-and-cost)
9. [Checks and events](#9-checks-and-events)

## 1. Concepts

Three layers, each named, each pointing at the one below:

- A **provider** is a connection to an endpoint: its type, base URL, key and
  concurrency limit. `[providers.<name>]` in `config.toml`.
- A **model** is an id on a provider, plus what HolaClaw needs to know about
  it: its context window, whether it calls tools or reads images, which API
  it speaks. `[models.<name>]` in `config.toml`.
- A **role** is a job an agent gives a model: answer the chat, classify tool
  calls, describe images. `[models]` in the agent's `agent.toml`, each role
  naming a model.

```toml
# config.toml
[providers.openrouter]
type     = "openai-compatible"
base_url = "https://openrouter.ai/api/v1"
auth     = "api-key"
sort     = "throughput"

[models.default]
provider = "openrouter"
id       = "deepseek/deepseek-v4.1-flash"
context  = 128000
tools    = true

[models.jev]
provider = "openrouter"
id       = "~typesafe/jev-latest"
api      = "decisions"
url      = "https://openrouter.ai/api/alpha/decisions"
```

```toml
# agents/main/agent.toml
[models]
chat  = "default"
guard = "jev"
```

One model can fill several roles, in one agent or many. Only `chat` is
required: every other role has a defined behaviour when it is unset (§4).

## 2. Providers

Two types are compiled in. Anything else is reached through an
OpenAI-compatible endpoint, not a plugin.

| Type | Speaks | Auth | Use it for |
|---|---|---|---|
| `openai-compatible` | Chat Completions, and the embeddings, transcription, decisions and images endpoints | `none` or `api-key` | OpenRouter, OpenAI's API, Ollama, llama.cpp, vLLM, any proxy |
| `codex` | the Responses API | `subscription` or `api-key` | a ChatGPT subscription; with `api-key`, OpenAI's Responses API |

```toml
[providers.<name>]
type           = "openai-compatible"   # or "codex"
base_url       = "https://…/v1"        # openai-compatible only
auth           = "api-key"             # "none" (the default), "api-key" or "subscription"
max_concurrent = 4                     # optional: requests in flight at once
sort           = "throughput"          # optional: OpenRouter's routing ("throughput", "latency", "price")
```

- **Keys.** An `api-key` provider reads its key from `auth.toml`, set with
  `holaclaw auth set providers.<name> api_key` (it reads stdin), or from an
  `env:VAR` reference in `api_key`. A literal key in `config.toml` is a config
  error. The key is looked up on every request, so a rotated key needs no
  restart. A provider without `auth = "api-key"` never sends one
  ([security.md §8](security.md#8-configuration-and-secrets)).
- **A ChatGPT subscription** (`type = "codex"`, `auth = "subscription"`)
  signs in with a device code: `holaclaw login codex`
  ([getting-started.md §3](getting-started.md#3-sign-in-to-chatgpt)). The
  daemon keeps the tokens and refreshes them. When the plan's quota is spent,
  the run fails with a `model.usage_limit` event carrying the reset time; it
  never asks you to sign in again. This flow is unofficial: it uses the Codex
  client's endpoints, and OpenAI may change them.
- **`sort`** is sent to OpenRouter as its `provider.sort`. On a gateway that
  routes one model to many hosts, the route dominates latency more than the
  model does; `throughput` is the fast choice, and `init` writes it for
  OpenRouter. Other servers ignore it.

## 3. Models

A model is a profile: the id on the wire, and what HolaClaw needs to know to
use it.

```toml
[models.<name>]
provider       = "openrouter"      # required: a [providers.<name>]
id             = "vendor/model"    # required: the id the endpoint knows
api            = "chat"            # "chat" (the default), "decisions" or "images"
context        = 128000            # required for a chat model: the window in tokens
tools          = true              # it calls tools natively
vision         = true              # it reads images
reasoning      = "low"             # optional: the reasoning effort of every call
timeout        = "120s"            # optional: the whole-request timeout
strict         = false             # optional: promote a tool call it wrote as text (§5)
thinking_tags  = ["scratchpad"]    # optional: more reasoning tags to scrub (§5)
schema_profile = "llamacpp"        # optional: strip schema keywords llama.cpp's grammar can't take
url            = "https://…"       # required for a decisions model: its endpoint
```

- **`api`** is the wire API the model speaks. A `chat` model answers
  messages; a `decisions` model answers typed questions (§7); an `images`
  model generates images. Each role accepts only the APIs it can use (§4).
- **`context`** is what HolaClaw budgets every prompt against
  ([context.md](context.md)). Set it to what the endpoint really serves: for
  Ollama or llama.cpp, the context the runtime allocated, not the model's
  advertised maximum.
- **`tools`** must be `true` for the agent to use tools: a model without it is
  sent no tool schemas.
- **`vision`** declares that the model takes image parts. A chat model with
  it gets the user's images as they are; one without it gets a description
  (§6).
- **`reasoning`** is `none`, `minimal`, `low`, `medium`, `high`, `xhigh`,
  `max` or `ultra`. `none` turns thinking off; the others are passed through,
  and a model refuses one it doesn't support. Unset, the model uses its own
  default. How thinking spends the window is in
  [context.md §8](context.md#8-thinking-models).
- **`timeout`** bounds a whole request. The defaults are 10 minutes for a
  chat model, 2 minutes for an image and 2 seconds for a decision. While a
  slow model is still working, a `model.progress` event goes out every 10
  seconds.

## 4. Roles

Roles live in the agent's `agent.toml`, under `[models]`, each naming a
`[models.<name>]` of `config.toml`.

| Role | Used for | API | When unset |
|---|---|---|---|
| `chat` | the agent's main model: every turn and every task run | chat | required |
| `fallback` | a second chat model, tried when the first fails (§5) | chat | a failed call fails the run |
| `summarize` | compaction: condensing the old part of a conversation ([context.md §4](context.md#4-compaction)) | chat | the `chat` model |
| `guard` | classifies each tool call the policy doesn't settle ([security.md §6](security.md#6-the-guard-model)) | decisions or chat | the policy alone decides |
| `decide` | the browser's per-step questions ([browser.md](browser.md)) | decisions | the `chat` model answers them, slowly |
| `plan` | writes the browser's moves | chat | the `chat` model |
| `vision` | describes images for a chat model without `vision` (§6), and reads the screen for the browser | chat | images are refused, unless the chat model has `vision` |
| `transcribe` | turns audio into text (§6) | chat, on an OpenAI-compatible provider | audio is refused |
| `embed` | vectors for memory search ([memory.md](memory.md)) | chat, on an OpenAI-compatible provider | memory search is full-text only |
| `image` | generates images for the `image.generate` tool | images | the tool is not offered |

A few roles do more than their row says:

- **The taint scorer** grades untrusted tool results
  ([security.md §7](security.md#7-untrusted-content)). It has no role of its
  own: it uses the `guard` model when that is a decision model, else the
  `decide` model. With neither, every untrusted result counts as steering.
- **`image`** turns the tool on: setting it adds `image.generate` to the
  agent's tools, and listing the tool without the role is a config error.
  `limits.images_per_day` (20 by default) caps it.
- **`embed`** is shared: memory belongs to the daemon, so it uses the `embed`
  model of the first agent, by name, that sets one.
- **`transcribe`** and **`embed`** call the provider's `/audio/transcriptions`
  and `/embeddings` endpoints, which only an `openai-compatible` provider
  serves. The model profile still says `api = "chat"`.

## 5. The chat model

Every turn is a loop of calls to the `chat` model: the prompt, the tools the
agent may use, then a tool call or an answer. HolaClaw assumes little of the
model, so that a small local one works too.

- **Tools are native only.** Tool schemas go in the request's tools field,
  never in the prompt. With `schema_profile = "llamacpp"`, keywords that
  llama.cpp's grammar can't handle (`pattern`, a large `maxLength`) are
  stripped from them first.
- **A tool call written as text is never run.** A model that writes a call in
  its answer instead of making it gets the call stripped, a `tool.rejected`
  event (`reason: text_leak`), and one nudge to make the call properly. With
  `strict = true`, a single leaked call is promoted to a real one instead:
  turn it on only for a model you have seen do this with well-formed calls.
- **Reasoning is scrubbed.** `<think>`, `<thinking>`, `<reasoning>`,
  `<thought>` and `<reflection>` blocks are removed from the stream before
  anyone sees them, even when a tag is split across chunks. `thinking_tags`
  adds names to that list.
- **The fallback** is opt-in. When a call fails on a timeout, a server or
  network error, a rate limit or a spent quota, and nothing has streamed yet,
  the run switches to the `fallback` model for the rest of the turn and says
  so with a `model.fallback` event (`from`, `to`, `reason`). Any other error,
  such as a refused request, fails the run.

What goes into each call, and how a conversation is fitted into the window,
is in [context.md](context.md).

## 6. Images and audio

Attachments that arrive on a channel or through the API are turned into
something the chat model can read before the turn starts.

- **An image** goes to the chat model as an image part when the model has
  `vision = true`. Otherwise the `vision` model describes it in under 120
  words, visible text verbatim, and the description, marked
  `[image description]`, takes its place. The description prompt tells the
  model to treat text in the image as data, not instructions.
- **Audio** is transcribed by the `transcribe` model, and the transcript takes
  its place.
- **Video** is not supported.

Each is capped: images at 8 MiB, audio at 25 MiB and two minutes of
transcription time, and the text derived from either at 8000 characters. An
attachment that can't be handled, because the role is missing, the file is
too large or the call failed, gets one fixed sentence in the reply ("I can't
see images here: …") and a `media.refused` event; the turn goes on without
it. A derived text is stored with the message, so it is made once.

## 7. Decision models

A decision model doesn't chat. It is asked typed questions about a piece of
text, and answers each from a closed set:

- `noul`: yes or no, answered as the probability of yes;
- `choice`: one of the keys the question offers, with the distribution;
- `score`: a position on an ordered scale.

HolaClaw keeps the workflow and the model only answers, so an answer can
never carry free text back. That makes it fast and cheap enough to sit on the
path of every tool call: the guard asks one question per call, the taint
scorer one per untrusted result, and the browser several per move.

```toml
[models.jev]
provider = "openrouter"
id       = "~typesafe/jev-latest"
api      = "decisions"
url      = "https://openrouter.ai/api/alpha/decisions"
```

The only endpoint today is OpenRouter's alpha decisions API, so the model
names it in `url`, and its provider must be `openai-compatible`. The transport
is experimental: a call is not retried, and every caller has a defined
behaviour when it fails. The guard asks a person (`recommend`) or denies
(`auto`); the taint scorer counts the result as steering; the browser treats
the answer as unreadable.

## 8. Usage and cost

Every model call reports its tokens, and its cost when the upstream prices
it. `model.response` and the events of the guard, the taint scorer, media,
memory and the browser carry `prompt_tokens`, `completion_tokens` and
`cost_nanousd` (billionths of a dollar). A call the upstream didn't price
counts in `unpriced_calls`, so a total with unpriced calls is a lower bound.

`holaclaw trace <id>` sums a turn: its wall time, tokens and cost, call by
call. `holaclaw prompt-size --agent <name>` prints the fixed part of the
agent's prompt (system prompt, persona and tool schemas), the part every call
pays for.

## 9. Checks and events

`holaclaw check` covers the models
([reference/check-rules.md](reference/check-rules.md)):

| Rule | Level | Checks |
|---|---|---|
| `CFG` | error | a model names an unknown provider, a role names an unknown model, a role names a model of the wrong API, a chat model has no `context`, a decisions model has no `url` |
| `SEC002` | error | an `api-key` provider has no key |
| `SEC007` | warning | a provider without `auth = "api-key"` has a key, which is never sent |
| `NET003` | error | a provider with a key is reached over plain `http` on a non-loopback host |
| `MOD001` | warning | a model's `context` is under 4096 |
| `MOD002` | error | a model's `context` is more than Ollama or llama.cpp allocated; skipped with `--offline` |
| `MOD003` | error | a model's `id` is not one its provider serves; skipped with `--offline` |
| `BUD001` | warning, error | an agent's fixed prompt is over 1500 tokens, or a quarter of its chat model's window |
| `BRW006` | warning | an agent with the browser has no `decide` model |

`holaclaw models <provider>` prints the ids a provider serves, the list
`MOD003` checks against.

The events, all in the trace ([reference/events.md](reference/events.md)):

| Event | When |
|---|---|
| `model.request`, `model.response` | each call, with the model, its usage and its duration |
| `model.progress` | every 10 seconds while a model is still answering |
| `model.error` | a call failed, with its kind (`timeout`, `rate_limit`, `server`, …) |
| `model.fallback` | the run switched to its fallback model |
| `model.usage_limit` | a subscription's quota is spent, until `reset_at` |
| `media.described`, `media.transcribed`, `media.refused` | an attachment was described, transcribed or refused |
| `memory.embedded` | one embeddings call |
