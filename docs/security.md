# Security

What the agent may do on its own, who decides when it may not, and where
secrets live. holaclaw runs on Linux, in a VM, on a local machine or on a
remote server, and never assumes it is sandboxed.

## Contents

1. [Concepts](#1-concepts)
2. [Identities and access](#2-identities-and-access)
3. [The control API](#3-the-control-api)
4. [Tools and policies](#4-tools-and-policies)
5. [Approvals and grants](#5-approvals-and-grants)
6. [The guard model](#6-the-guard-model)
7. [Untrusted content](#7-untrusted-content)
8. [Configuration and secrets](#8-configuration-and-secrets)
9. [Deployment](#9-deployment)
10. [Checks and events](#10-checks-and-events)
11. [Limits](#11-limits)

## 1. Concepts

Four layers, each enforced before the next one is reached:

| Layer | Enforces | Rules come from |
|---|---|---|
| Control API | A token and its scope on every request; the socket's file permissions | Tokens in the database (§3) |
| Router | Who may talk at all, and as which identity and role. It runs before any agent run exists | `channels.toml` access knobs, bindings in the database (§2) |
| Policy gate | Every tool call. The agent loop has no other way to reach a tool | `[policy]` in `agent.toml`, grants in the database (§4) |
| Process | What an allowed tool call can still reach: exec children, the daemon's own unit | The deployment (§9) |

| Term | Meaning |
|---|---|
| **Risk class** | What a tool can do: `read`, `write`, `external` (reaches outside the machine) or `exec`. Each tool declares one. |
| **Decision** | `allow`, `ask` or `deny`. `deny` always wins. |
| **Grant** | A person's approval kept for later: it turns an `ask` into an `allow`, never lifts a `deny` (§5). |
| **Untrusted** | Content from outside: a fetched page, a browser result, a connector result, an attached document. Always marked as such (§7). |
| **Tainted** | A turn where untrusted content scored as trying to steer the agent. It can raise `exec` and `external` calls to `ask` (§7). |
| **Guard** | An optional model that scores how safe one tool call is (§6). |
| **Pending** | A stranger admitted by open access, not yet confirmed. Runs under the `guest` policy block. |

## 2. Identities and access

Who may talk to the agent, and as whom, is [identity.md](identity.md): the
access knobs (`dm`, `rooms`), invites, pending identities, roles (owner,
admin, user) and room identities. What each identity's memory can reach is
[memory.md](memory.md).

What matters for the policy gate:

- **A turn runs under one role.** That of the sender of the messages it
  answers. A turn that coalesced messages from several senders runs under the
  strictest of them, so a user's message can't ride on an admin's.
- **A pending sender runs as `guest`.** So does a sender that no longer
  exists. A room identity holds no role and grants nothing.
- **Guests are judged by `policy.guest`** (§4), get no grants (§5), can't
  save or forget memory, and can't use the browser. Their memory is their own
  partition plus the agent's shared notes.
- **A disabled identity can do nothing.** Its messages are dropped, its
  tasks don't run and its tokens get `403 identity_disabled`; it keeps
  everything until an admin enables it again ([identity.md](identity.md)).
- **Open access is refused** by `holaclaw check` (ACC001) when the guest
  block of the channel's agent allows `exec`, `write` or `external`.
- **Every user of an agent shares its workspace and its browser logins.** The
  workspace is the agent's, not a person's.

## 3. The control API

Every client, the CLI included, talks to the daemon over the HTTP API. How
clients become bindings, and how tokens are minted, rotated and revoked, is
[identity.md §3](identity.md#3-apps-devices-and-tokens).

- **Tokens** start with `hb_` and carry 32 random bytes. The database keeps
  only their SHA-256 hash: the secret is shown once, when it is minted, and
  can't be read back.
- **Scopes.** `admin` and `client`, declared per endpoint in the OpenAPI
  spec; `admin` covers `client`. A token is `client` unless asked otherwise,
  and `admin` is refused for an identity whose role is `user`. A demotion to
  `user` lowers the identity's `admin` tokens to `client` in the same step,
  so a stored scope is never above the role and a later promotion raises none
  until `token set`. Every request also acts at the lower of the token's
  scope and the role.
- **Owner tokens.** Only the owner creates, changes, rotates or revokes a
  token of the owner: rotating one would hand its caller a secret that acts
  as the owner. Admins are not protected from each other: any admin can mint,
  raise or rotate another admin's tokens, and the token's `created_by` and
  its events record who did.
- **Traceability.** Revoked tokens are kept with who revoked them. A
  message's trace records the token that sent it (`channel.received` carries
  `token`), so the tokens sharing one binding stay distinguishable.
- **The unix socket** is the default transport. It arrives as `holaclaw:cli`,
  the owner, by file permission: on a system install its directory is mode
  `0700` and owned by the service user.
- **Pairing** (`POST /v1/pair`) is the one call that takes no token: the
  code of a `holaclaw` invite is the credential. Codes are 10 Crockford
  base32 characters (~50 bits), stored as hashes, single-use and short-lived
  (15 minutes by default, at most an hour). A wrong,
  spent or expired code gets one answer, and 5 failed codes within 10
  minutes from one source address (an IPv6 /64 counts as one) block that
  address for the rest of the window; the count is in memory, so a restart
  clears it. Behind a proxy every client shares the proxy's address. A
  pairing link (`holaclaw://pair?server=…&code=…`) carries the code, so treat
  it as a short-lived secret. Self-service links from `POST /v1/me/devices`
  never carry more than the caller's own scope.
- **TCP** (`server.listen`) is optional. A loopback address is served as is.
  Any other address is refused at start unless TLS is configured
  (`server.tls_cert`, `server.tls_key`) or `server.allow_insecure_transport`
  is set, and until `holaclaw init` has created the owner. A token is
  required either way, except for pairing. `$HOLACLAW_LISTEN` and
  `$HOLACLAW_ALLOW_INSECURE_TRANSPORT` override the config.

### Roles and scopes

A caller is an identity's role, `owner`, `admin` or `user`, seen through a
token's scope, `client` or `admin`. A `user` holds `client` tokens only; an
`owner` or `admin` holds either. A pending identity holds no token. The unix
socket is the owner with `admin` scope, and the only caller that sees past its
own identity.

| What | `client` token | `admin` token of an admin | `admin` token of the owner | Unix socket |
|---|---|---|---|---|
| Its identity's `holaclaw` conversations: open, read, send, delete, abort | yes | yes | yes | yes |
| Its identity's DMs on other channels and its task sessions | read-only ([identity.md §4](identity.md#4-sessions-and-what-each-token-sees)) | read-only | read-only | read and write |
| Other identities' sessions, and room sessions | no | no | no | yes |
| Event stream and blobs | its sessions' | its sessions', plus events of no session | the same | all |
| Traces | of its sessions | any, by id | any, by id | any |
| Status, agents, models, channels, skills; watching the browser | yes | yes | yes | yes |
| Rotate its own token | yes | yes | yes | no token |
| Approvals: list and answer | no | yes | yes | yes |
| Identities, invites and tokens | no | yes, except acting on the owner | yes | yes |
| Tasks, memory, connectors, credentials, channel and provider logins, skill changes, backups, browser takeover | no | yes | yes | yes |

Acting on the owner means assigning a pending identity onto it, unbinding one
of its bindings, minting an invite for it, and creating, changing, rotating
or revoking one of its tokens (owner tokens, above). Nobody updates, disables
or deletes the owner.

Inside a conversation, the role decides and the scope plays no part: an
owner or admin answers approvals in the chat (§5), may save shared memory
notes and gets `skill.save` and `skill.forget` (§4); a `user` gets none of
these.

## 4. Tools and policies

### Risk classes

| Tool | Class | Contained by |
|---|---|---|
| `memory.search` | read | the memory read set ([memory.md](memory.md)); never in a task |
| `memory.save`, `memory.forget` | write | the same; refused for guests |
| `files.read`, `files.list` | read | the workspace, opened as an `os.Root`: `..`, absolute paths and symlinks out of it are refused; reads capped at 16 KiB |
| `files.write` | write | the same |
| `attach` | read | the workspace |
| `react` | read | offered only on a channel with reactions; the target is the turn's own message |
| `skill.load` | read | the agent's skills ([skills.md](skills.md)) |
| `skill.save`, `skill.forget` | write | offered only to an owner or admin, never in a task |
| `task.create` | write | [tasks.md](tasks.md) |
| `fetch` | external | GET over `http` or `https`; private, loopback, link-local, CGNAT, reserved and NAT64 addresses refused at dial time, on every redirect too; at most 5 redirects; no proxy from the environment, no cookies; 256 KiB by default, 2 MiB at most |
| `image.generate` | external | present only when the `image` role is set; `limits.images_per_day` (20 per agent and UTC day) |
| `browser.do` | external | the sidecar ([browser.md](browser.md)); refused for guests |
| connector tools | the server's hints | [connectors.md §6](connectors.md#6-permissions-and-trust) |
| `exec` | exec | below |

**`exec`** runs `/bin/sh -c <command>` in the agent's workspace, with nothing
inherited from the daemon: `PATH=/usr/local/bin:/usr/bin:/bin`, `HOME` set to
the workspace, `LANG` and `TERM`, and stdin from `/dev/null`. It times out
after 30 seconds by default and 300 at most, then the whole process group is
killed. Output is capped at 16 KiB, head and tail kept. There is no OS
sandbox: what the command can reach is what the daemon's user can reach,
narrowed only by the systemd unit (§9).

**Targets come from the origin.** No tool takes a person or a conversation as
an argument. A reply, an attachment, a reaction and a task's result go where
the message came from, read from the trusted envelope, never from anything
the model can set.

**`browser.do`** is given to every agent once the browser is enabled in
`config.toml`, without being listed in `tools`. An agent keeps off it with
`[browser] enabled = false` in its `agent.toml`.

### The policy

```toml
# agents/main/agent.toml
[policy]
read     = "allow"
write    = "allow"
external = "allow"
exec     = "deny"
exec_allow = ["git status", "git log"]     # turns an exec ask into allow
tools      = { "files.write" = "ask" }     # per tool, or per connector id

[policy.guest]                             # pending senders
read = "allow"
write = "deny"
external = "deny"
exec = "deny"

[policy.task]                              # detached runs
read = "allow"
write = "allow"
external = "allow"
exec = "deny"
```

The values above are the defaults. With no `[policy]` table, an agent may
read, write and reach outside freely, and never run `exec`.

| Block | Judges |
|---|---|
| `[policy]` | owners, admins and users in a conversation |
| `[policy.guest]` | guests (§2) |
| `[policy.task]` | detached task runs; an `ask` there is a `deny` unless a grant answers it (§5) |
| both `guest` and `task` | a task run as a guest: the stricter decision of the two, per class |

### The pipeline

Every tool call goes through these steps, in order. Each step is a
`policy.decision` event.

```
1. class        the decision of the call's class in its block (role and run kind)
2. override     policy.tools: the exact tool name, else the connector id.
                It can tighten or loosen, but a class deny still wins, and
                for a guest it can only tighten
3. destructive  a connector tool the server marks destructiveHint: allow becomes ask
4. taint        with taint_escalates, a tainted exec or external call: allow becomes ask (§7)
5. rules        on an ask: exec_allow, then grants (§5)
6. guard        when a guard is set, for anything but read and deny (§6)
7. final        allow runs the tool; deny refuses it; ask asks a person (§5)
```

A refused call reaches the model as `denied by policy: <tool> (<reason>)`,
with a fixed reason, never model text.

### The exec allow-list

`exec_allow` entries are command prefixes, matched on whole words: `"git
status"` covers `git status -s`, not `git statusx`. A command that contains
any shell metacharacter (`` ; & | $ ` > < ( ) * ? { } ~ `` or a newline) is
never covered, by an entry or by a grant's pattern: the gate can't tell what
the rest of the line would run.

The allow-list is skipped in a tainted turn and for a destructive connector
tool. There only a grant, a decision a person made, can turn the `ask` into
`allow`.

## 5. Approvals and grants

An `ask` suspends only the session it belongs to, and is stored in the
database as an approval, with who answered it and how.

- **Who answers.** An owner or admin. In a channel conversation whose turn was
  started by an owner or admin, the prompt appears there: buttons where the
  platform has them (Telegram, Slack, Google Chat), else a text prompt
  answered by reaction, 👍 once · 🔁 session · ✅ always · 👎 deny. Anyone
  else's approvals go to the API only: `holaclaw approvals`, `holaclaw
  approve`, `holaclaw deny`, or the GUI (`admin` scope). The first answer
  wins.
- **The prompt** names the tool and, for `exec`, the command, capped at 200
  bytes. A guard verdict goes first: `[guard: 0.12 destructive] exec: rm -rf
  build`. Square brackets in the arguments are replaced, so they can't
  imitate a verdict.
- **Timeout.** An approval waits 5 minutes. A timeout is a deny. A restart
  expires every pending approval, since nothing is waiting on it any more.

An approval answered `session` or `always` is kept as a grant:

| Scope | Covers |
|---|---|
| `once` | this call only; nothing is kept |
| `session` | this tool in this session |
| `always` | this tool in any session of the agent, for **the person who approved it only** |
| `task` | this tool in one task, set when the task is created ([tasks.md](tasks.md)) |

For `exec`, a grant carries the command as a prefix pattern, matched like
`exec_allow` (§4). Grants never apply to guests, and never lift a `deny`.

A browser step approval ([browser.md](browser.md#risky-steps)) is once-only:
it offers `once` and deny, and refuses `session` and `always`, which would
grant every `browser.do` call rather than that step.

## 6. The guard model

The `guard` model role is optional. It scores each tool call, 0 to 1, for
how safe it is to run without asking, and labels it from a closed set:
`benign`, `destructive`, `exfiltration`, `escape`, `injection`. It is never
shown a `deny`, and never asked about a `read` call.

```toml
# agents/main/agent.toml
[models]
guard = "jev"                  # a decision model (recommended) or any chat model

[policy]
guard_mode      = "recommend"  # recommend | auto
guard_threshold = 0.7
```

- **A decision model** answers in about 0.4 s and gives a graded score; it
  times out after 2 s. **A chat model** answers one word, read as 1 (allow)
  or 0 (ask); it times out after 8 s, and a reasoning model adds seconds to
  every tool call. A score where the label isn't `benign` is capped at 0.5.
- **`recommend`** (the default): advice and an extra question, never an extra
  permission. Below the threshold, `allow` becomes `ask`. The verdict goes
  into the approval prompt. An error means `ask`.
- **`auto`**: nobody is asked for the clear cases. At or above the threshold
  the call runs. Below it, the call is denied, except a `benign` call that only
  fell short of the threshold, which is asked. An error or timeout is a deny.
  `auto` never:
  - lifts a `deny`;
  - allows a tainted `exec` or `external` call, whatever `taint_escalates`
    says: it goes to a person;
  - allows a destructive connector tool without a grant;
  - turns a task's `ask` into `allow`: that needs a grant;
  - overrules a person: a call a grant allowed and the guard dislikes is asked
    again, not denied.
- **The model's text never leaves the guard.** Traces and prompts get the
  score and the fixed label.
- **A verdict is cached** for 10 minutes (256 calls), so a retried call isn't
  classified, or paid for, twice.
- **What it sees.** The tool, its description, class, run kind, role, and up
  to 1500 bytes of the arguments. A guard on a cloud provider sends those
  arguments (paths, URLs, commands) off the machine.

## 7. Untrusted content

**Untrusted** is a fact about where content came from, and nothing removes
it. These are untrusted:

- `fetch` and `browser.do` results;
- connector results, `trusted` connectors included;
- documents attached to a message and inlined into the prompt;
- a recall summary of anything untrusted.

Untrusted text reaches the model inside `<untrusted>` … `</untrusted>`, with
any such tag inside it defused, so content can't close its own delimiter.
The mark follows it: a compaction summary or a recall row made from it stays
wrapped ([context.md §4](context.md#4-compaction)). A reply derived from it is
stored as tainted, but replayed unwrapped. `files.read` results are not
marked: the workspace is the agent's own.

**Tainted** is a judgement about whether that content tries to steer the
agent. Each untrusted tool result is scored once, when it arrives, by a
decision model: the `guard` role when it is a decision model, else `decide`.
"Ignore your instructions" scores 0.99, a price 0.02.

- **Arming.** A result at or above `policy.taint_threshold` (0.5) taints the
  call that read it and the next `policy.taint_decay_calls` (3) calls of the
  turn, refreshed by each steering result. A new turn starts tainted only
  while its history still holds a message that scored at or above the
  threshold.
- **Failing closed.** With no decision model, every untrusted result counts as
  1. So does a scorer error or timeout, and a result over 256 KiB.
- **Attached documents and untrusted recall** are not scored: they taint every
  call of their turn.
- **`trusted` connectors** are wrapped, but never scored, so they never taint
  (CON004).
- **What tainted does.** With `policy.taint_escalates = true`, a tainted
  `exec` or `external` call moves from `allow` to `ask`, and the allow-list no
  longer answers it (§4). In `auto` mode the guard never allows one (§6).

`taint_escalates` is **off by default**. With the default policy
(`external = "allow"`) and no guard in `auto`, a turn steered by a page can
call `fetch`, `browser.do` or a connector tool without asking. Turn it on for
an agent that reads the web and holds anything private.

The score reaches the trace (`taint_score` on `tool.result`), never the text.

## 8. Configuration and secrets

**Secrets live in `auth.toml`**, in `server.secrets_dir` (default
`/etc/holaclaw-secrets`; `holaclaw init` puts it inside the config
directory). It holds every provider key, channel token and connector token,
keyed like the config:

```toml
# auth.toml
version = 1

[providers.openrouter]
api_key = "sk-or-…"

[channels.telegram]
bot_token = "123456:…"

[connectors.github]
access_token  = "…"
refresh_token = "…"
```

- **Mode `0600`.** `holaclaw check` fails when it is group- or
  world-readable (SEC001), or the config directory is world-writable (SEC005).
- **Written** by `holaclaw auth set`, `holaclaw login`, the credentials API,
  or a deployer. The daemon rewrites it when it refreshes an OAuth token:
  re-read, only the one entry changed, temp file and rename, under a lock.
  Comments are not kept.
- **Write-only over the API.** A credential can be set, never read back,
  except inside a backup an admin asked to include it in.
- **Not in the database**, on purpose: a database backup, a snapshot or a
  trace export never carries a token, and restoring an old database can't
  bring back a spent single-use refresh token.
- **`env:VAR` references** are the other place a provider key can come from:
  `api_key = "env:OPENROUTER_KEY"` in `config.toml`. A literal secret in
  `config.toml` is a config error.
- **WhatsApp** keeps its device keys in its own session database,
  `<data_dir>/whatsapp/<name>.db`, not in `auth.toml`. It is not in backups:
  a restored host links WhatsApp again (SEC006).

**What never sees a secret.** The model, so no tool argument can carry one.
Exec children, whose environment is rebuilt from nothing. Trace events and
logs: channel and provider errors are scrubbed of tokens before they are
logged. Image prompts and what a person types into the browser never reach a
trace either.

**The database** holds no secret, only token hashes, but does hold every
conversation. `holaclaw check` warns when its mode allows group or other
access (DAT002).

**Backups** exclude `auth.toml` unless asked (`--include-secrets`, `admin`
scope, never for automatic backups). An archive with secrets holds every
credential. See [guides/operator.md](guides/operator.md#5-backups).

**Network.** An authenticated provider (NET003) or a connector (CON003)
reached over plain `http` on a non-loopback host is a check error: its token
would cross the network in clear text.

## 9. Deployment

How to run holaclaw so that an allowed tool call reaches as little as
possible, and how to check that a release is the one this repository built.
The ways to run it are in [deployments/README.md](deployments/README.md).

### Hardening

holaclaw never assumes it is sandboxed. The policy gate decides which tool
calls run (§4); the units below narrow what an allowed call can still reach.
They don't replace the gate. `deploy/holaclaw.service` and
`deploy/holaclaw-browser.service` are the hardened systemd units of a
[Linux install](deployments/linux.md); the NixOS module installs the same
ones, and the [container](deployments/container.md) runs the same two users
inside the image.

**The daemon** (`deploy/holaclaw.service`) runs as the `holaclaw` system
user, with no login shell:

- It can write only its state (`/var/lib/holaclaw`), its config directory
  (`/etc/holaclaw`, for `auth.toml`, `connectors.toml` and skills),
  `/etc/holaclaw-secrets` when that is the `secrets_dir`, and the browser
  sidecar's socket directory. The rest of the filesystem is read-only
  (`ProtectSystem=strict`), and `/home` is hidden.
- The control socket lives in `/run/holaclaw/`, mode `0700`. Everything the
  daemon creates is private to its user (`UMask=0077`).
- No capabilities, no new privileges, no namespaces, no writable-executable
  memory, a private `/tmp` and `/dev`, the `@system-service` syscall set, and
  only unix and IP sockets.

**`exec` children** inherit that unit: the same user, the same read-only
filesystem, the same syscall filter. Inside it, `exec` adds its own limits:
the agent's workspace as working directory and `HOME`, an environment rebuilt
from nothing (`PATH=/usr/local/bin:/usr/bin:/bin`), no stdin, a timeout of 30
seconds by default and 300 at most, after which the whole process group is
killed, and 16 KiB of output. There is no per-command sandbox: a command can
read whatever the `holaclaw` user can, the secrets included. Keep `exec` at
`ask`, allow-list only what you need, or leave it off.

**The browser sidecar** (`deploy/holaclaw-browser.service`) runs Chromium as
its own user, `holaclaw-browser`. Chromium can't run under the daemon's
unit: its JIT needs writable-executable memory, and its own sandbox needs
user namespaces. So the sidecar's unit allows those two and holds what a
compromised renderer must not reach: no provider keys, no `auth.toml`, no
database, no network listener, only its unix socket. It is capped at 2 GiB
of memory and 512 tasks. Chromium's sandbox is never turned off: when it
can't start, the browser fails to launch and `holaclaw check` says why
(BRW003). See [browser.md](browser.md#1-concepts).

### Verifying a release

Every release is signed keyless with [cosign](https://docs.sigstore.dev/):
the signature carries a short-lived certificate naming the release workflow
of this repository and the ref it ran from, and is recorded in Sigstore's
public transparency log. No signing key exists. The workflow signs every
file of the release, each with its own `<file>.sigstore.json`, and every
container image by digest, in a job that builds nothing, and checks each
signature before it publishes.

Check one file against its signature:

```
cosign verify-blob holaclaw-linux-amd64 --bundle holaclaw-linux-amd64.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/ikitana/holaclaw/\.github/workflows/release\.yml@refs/(heads/main|tags/v.+)$'
```

or, for several, `SHA256SUMS` against its signature and the files against it:

```
base=https://github.com/ikitana/holaclaw/releases/latest/download
curl -fL --remote-name-all "$base/SHA256SUMS" "$base/SHA256SUMS.sigstore.json"
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/ikitana/holaclaw/\.github/workflows/release\.yml@refs/(heads/main|tags/v.+)$'
sha256sum --check --ignore-missing SHA256SUMS
```

and an image, by tag or by digest:

```
cosign verify ghcr.io/ikitana/holaclaw:vX.Y.Z \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/ikitana/holaclaw/\.github/workflows/release\.yml@refs/(heads/main|tags/v.+)$'
```

The identity accepts a release made from a tag or from `main`, and nothing
else: a run from any other branch signs as that branch and fails the check.
Each architecture's image is signed too, so the digest `docker` pulled for
one platform verifies as well as the multi-arch tag.

Releases also carry GitHub build provenance, which names the commit and the
run that built each file and image:

```
gh attestation verify holaclaw-linux-amd64 -R ikitana/holaclaw
gh attestation verify oci://ghcr.io/ikitana/holaclaw:vX.Y.Z -R ikitana/holaclaw
```

## 10. Checks and events

| Rule | Level | When |
|---|---|---|
| `SEC001`, `SEC005` | error | `auth.toml` readable by others; the config directory world-writable |
| `SEC002`, `SEC003` | error | a provider or channel without its credential |
| `NET001`, `NET002` | error | a non-loopback `listen` without TLS or `allow_insecure_transport`; an unreadable certificate |
| `NET003`, `CON003` | error | a token sent over plain `http` off the machine |
| `ACC001` | error | open access while the guest block allows `exec`, `write` or `external` |
| `POL001` | warning | `exec = "allow"` |
| `POL002` | info | `exec` listed in `tools` while `exec = "deny"` |
| `CON004` | info | a `trusted` connector |
| `BRW003` | error | Chromium's sandbox can't start |
| `BRW005` | warning | `browser.do` with no `browser.allow`: any site |
| `DAT002` | warning | the database readable by others |

Every rule is in [check-rules.md](reference/check-rules.md).

| Event | Carries |
|---|---|
| `policy.decision` | tool, class, step, decision, fixed reason, connector; never the arguments |
| `guard.decision` | the same, plus mode, score, duration and the call's usage |
| `approval.requested`, `approval.resolved` | the approval, who answered and how |
| `tool.result` | `taint_score` of an untrusted result |

The events are listed in [events.md](reference/events.md).

## 11. Limits

- **No OS sandbox for `exec`.** A command runs as the daemon's user, in the
  daemon's unit. Run holaclaw on a machine you already trust with that
  access, keep `exec` at `ask`, and allow-list only what you need.
- **The workspace is shared.** Every user of an agent reads and writes the
  same files, and `files.read` content is trusted.
- **The guard sees arguments.** A cloud guard sends up to 1500 bytes of every
  non-read call's arguments to its provider. There is no switch to send only
  metadata.
- **Approvals wait for an owner or admin.** A user's call waits on someone
  else; nobody answering in 5 minutes is a deny.
- **The taint score is a model's opinion.** It is short-circuited to 1 on any
  failure, but a steering text that scores low still runs unescalated.
