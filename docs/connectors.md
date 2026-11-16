# Connectors

How an agent reaches an external service. This document covers connectors:
how an admin connects a remote MCP server, how agents are granted it, what
the model sees of its tools, and how the daemon keeps it healthy.

It describes the code as it is. To connect a server step by step, see the
[user guide](guides/connectors.md). How connectors fit the rest of
HolaClaw: [ARCHITECTURE.md](../ARCHITECTURE.md#6-extension-points).

## Contents

1. [Concepts](#1-concepts)
2. [Lifecycle](#2-lifecycle)
3. [Sign-in](#3-sign-in)
4. [Files and storage](#4-files-and-storage)
5. [Tools the agent sees](#5-tools-the-agent-sees)
6. [Permissions and trust](#6-permissions-and-trust)
7. [Health](#7-health)
8. [CLI and API](#8-cli-and-api)
9. [`check` rules](#9-check-rules)
10. [Events and `detail`](#10-events-and-detail)
11. [Security](#11-security)
12. [Limits](#12-limits)

## 1. Concepts

### Connector, grant and intent

- **A connector is *what* is reached.** One connection to one remote MCP
  server over Streamable HTTP, kept under an id the admin chooses:
  `github-admin`, `homes`.
- **A grant is *who* may use it.** An agent's `agent.toml` lists the
  connectors it may use: `connectors = ["github-admin"]`.
- **An intent is *how* a connector gets its credentials.** One open sign-in:
  the authorization URL a person must open, and what the daemon needs to
  finish it when the code comes back.

| | Connector | Grant | Intent |
|---|---|---|---|
| Answers | Which server, as which account? | Which agents may call its tools? | Is a sign-in waiting for a person? |
| Lives in | `connectors.toml`, `auth.toml`, the cache row (§4) | `connectors` in `agent.toml` | the daemon's memory only |
| Created by | `connectors create`, or by hand in `connectors.toml` | editing `agent.toml` | `create` or `reconnect` on an OAuth server |
| Removed by | `connectors delete` | editing `agent.toml` | the code, a failure, or 15 minutes |

The rules:
- **Connectors form one flat list at engine level.** An admin connects a
  server once; any number of agents may be granted it, and an agent may be
  granted any number of connectors.
- **A connector acts as one account.** The one that signed in when it was
  connected, either a person's own account or a bot account made for the
  purpose. It belongs to the connector, not to the agent or to whoever talks
  to it: every user of a granted agent acts as that account.
- **Agents name the connector, never the service.** `github-admin` and
  `github-bot` are two connectors to the same service with two accounts.
- **Agents granted the same connector share it**: one MCP client, one
  session, one set of tokens.
- **The daemon does the OAuth work.** Discovery, the token exchange, refresh
  and health all run in the daemon. A client only opens a browser and hands
  one authorization code back, so the daemon never has to be reachable from
  the internet.

### Examples

```
engine
├── github-admin   https://mcp.github.com/        oauth   connected   as @angel
├── github-bot     https://mcp.github.com/        oauth   connected   as @holaclaw-bot
└── homes          https://homes.example.com/mcp  none    connected   trusted

agents/main/agent.toml     connectors = ["github-admin", "homes"]
agents/support/agent.toml  connectors = ["github-bot"]
```

`main` sees `github-admin.create_issue` and `homes.lights_on`; `support` sees
`github-bot.create_issue`. Whoever talks to `support` opens issues as
`@holaclaw-bot`.

### Other terms

| Term | Meaning |
|---|---|
| **Open server** | A server that answers without a token (`auth = "none"`). It connects at once and never has an `auth.toml` entry. |
| **OAuth server** | A server that answers `401` and publishes authorization metadata (`auth = "oauth"`). It needs a sign-in. |
| **Client identity** | How holaclaw names itself to an authorization server: a pre-registered app, the metadata document, or a registration (§3). |
| **Trusted** | A connector whose results are not scored for prompt injection (§6). |
| **`detail`** | A short daemon string saying why a connector is in its state, from a closed set (§10). |

## 2. Lifecycle

```
 create, open server ─────────────────────────────────────▶ connected
 create, OAuth ──▶ pending ──▶ authorizing ── code ──────▶ connected | degraded
                   (discovery)      └── fails or expires ─▶ deleted
 daemon start ───▶ pending ──── first check ─────────────▶ connected | degraded
                      └── no grant, or the URL changed ───▶ expired

 connected ── 3 failed checks, or 401 on an open server ──▶ degraded
 degraded ─── one good check ─────────────────────────────▶ connected
 any state ── refresh rejected ───────────────────────────▶ expired
 any state ── reconnect, OAuth ──▶ authorizing ── code ───▶ connected | degraded
                                        └── fails or expires ─▶ the state before
 any state ── delete ─────────────────────────────────────▶ deleted
```

| State | Meaning | Leaves it when |
|---|---|---|
| `pending` | Discovery is running inside a create, or the connector was loaded at start and not checked yet | discovery finishes; the first check succeeds, or fails three times |
| `authorizing` | An intent is open: a person must open the authorization URL. A reconnect shows it too | the code is exchanged, or the intent fails or expires |
| `connected` | The handshake and the tool list succeeded | three checks fail in a row, a refresh is rejected, or an open server answers `401` |
| `degraded` | Something failed and is retried; calls are still sent | one check succeeds, or a refresh is rejected |
| `expired` | The credentials are dead: the refresh was rejected, an OAuth entry has none, or the URL changed | `reconnect` |

`deleted` is not a state a connector stays in: it is the last
`connector.status` event of a connector that is gone.

### Creating

`create` takes an id, a URL and optionally the auth method, scopes, a
pre-registered client and the redirect the client listens on. The daemon
refuses an id in use and an id that is a built-in tool or group name, then
calls the server without a token (§3):

- **An open server** is connected in the same request: the daemon writes the
  `connectors.toml` entry, caches the tools and answers `connected`.
- **An OAuth server** gets an intent. The daemon runs discovery, picks the
  client identity and answers `authorizing` with the authorization URL and
  when it expires. Nothing is written to `connectors.toml` yet.

`--auth` (the API's `auth`) states what the admin expects. Left out, the
daemon uses what the server answers; given, a server that disagrees fails
the create with `auth_mismatch`.

### Connecting

1. The client creates the connector and receives the authorization URL.
2. A person opens the URL in a browser and signs in. The authorization server
   redirects the browser to the client's loopback redirect with `code`,
   `state` and possibly `iss`.
3. The client posts them to the daemon. The daemon checks `state` and `iss`,
   exchanges the code, writes `auth.toml`, runs the handshake and lists the
   tools, writes `connectors.toml`, and answers with the settled connector:
   `connected`, or `degraded` when the server failed after the exchange.

The client may be the CLI, an app or any other API client: all of them use
the same API (§8).

### Reconnecting

`reconnect` opens a new intent on an existing connector, optionally with new
scopes. It is the fix for `expired`, the way to change scopes, and how an
entry written by hand gets its first credentials.

- The old tokens keep serving calls until the new code is exchanged, so
  agents keep working while an admin signs in again.
- While the intent is open, `list`, `show` and `status` show `authorizing`,
  and health checks skip the connector.
- An intent that fails or expires returns the connector to the state it had,
  with the failure as its `detail` (`intent_expired`, `state_mismatch`, …).
- A reconnect while another intent is being opened, or its code exchanged,
  is refused. A reconnect over an intent waiting for its code replaces it.
- An open server opens no intent: reconnect redials it and runs a check.

### When an intent ends

An intent lives 15 minutes from the moment its URL is handed out. Once its
code is posted it gets 5 more minutes to finish the exchange. There is no
timer: expired intents are dropped when the connector is next read (`list`,
`show`, `reconnect`, `code`) or checked (every 10 minutes), so a connector
may show `authorizing` past its expiry until then.

A failed intent, whatever the cause (expiry, a `state` or `iss` mismatch, a
refused code, a server that does not answer), ends the same way:

| Intent of | Ends with |
|---|---|
| a create | the connector is deleted: nothing of it is kept, and the event `state = "deleted"` carries the reason in `detail` |
| a reconnect | the connector returns to its previous state, with the reason in `detail`; the old tokens stay |

### Deleting

`delete` works in any state. It removes the `connectors.toml` entry, the
`auth.toml` keys and the cache row, and emits a final `connector.status` with
`state = "deleted"`. A `connectors.toml` that is a symlink or does not parse
stops it before anything is removed. If the entry is gone but the keys or
the cache row could not be removed, the connector is deleted and the id must
be deleted again before it is created again.

### What the model sees

A connector in any state keeps its tools registered, so the agent can tell
the person what is wrong instead of losing tools silently:

| Connector | A call answers |
|---|---|
| `expired` | `connector <id> needs to be reconnected by an admin` |
| being created | `connector <id> is not connected yet` |
| `connected` or `degraded` | the server's answer; on failure, `connector <id>: <cause>` |
| no longer configured | `connector <id> does not exist` |

`<cause>` is one of `timed out`, `unauthorized`, `cancelled`,
`response too large`, `session lost`, `client closed`,
`token refresh failed`, `request failed`. Server text never reaches it.

## 3. Sign-in

### Discovery

The daemon calls the server with no token:

- **`200`**: an open server.
- **`401`**: its `WWW-Authenticate: Bearer resource_metadata="…"` points at
  the Protected Resource Metadata
  ([RFC 9728](https://www.rfc-editor.org/rfc/rfc9728)): the
  `authorization_servers` and the `scopes_supported`. Without
  `resource_metadata`, the daemon tries the RFC 9728 well-known paths.

The first authorization server listed is used. Its own metadata
([RFC 8414](https://www.rfc-editor.org/rfc/rfc8414), or OpenID discovery)
gives the authorize, token and registration endpoints, the PKCE methods, and
whether it accepts a Client ID Metadata Document. Its `issuer` must equal
the URL listed, and it must offer PKCE `S256`; otherwise discovery fails.

The issuer, its endpoints and the scopes to ask for go to the cache row, and
`show` prints them. The full `authorization_servers` and `scopes_supported`
lists are not kept: only `inspect` shows them.

### Client identity

An authorization server will not talk to a client it cannot name. The daemon
picks the first of these that applies:

1. **Pre-registered app.** `client_id` in `connectors.toml`, and
   `client_secret` in `auth.toml` when the console issued one. For servers
   that require an app created in their console.
2. **Client ID Metadata Document**
   ([CIMD draft](https://datatracker.ietf.org/doc/draft-ietf-oauth-client-id-metadata-document/)).
   The client id is `https://holaclaw.ai/oauth/client-metadata.json`, a
   public document with holaclaw's name and logo, no secret, and the
   loopback redirect URIs on ports `26500` to `26504`. Used when the
   authorization server advertises `client_id_metadata_document_supported`,
   the intent's redirect is one of the pinned ones, and the server has not
   refused the document before.
3. **An earlier registration.** A client id this connector registered
   before, when the issuer and the redirect are the same.
4. **Dynamic Client Registration**
   ([RFC 7591](https://www.rfc-editor.org/rfc/rfc7591)). The daemon registers
   with the intent's redirect. The client id goes to the cache row and a
   `client_secret`, if one comes back, to `auth.toml`.

The pinned redirects are `http://127.0.0.1:26500/callback` to
`http://127.0.0.1:26504/callback`. The draft requires an exact match, so
holaclaw's clients listen on one of those ports and redirect to `127.0.0.1`.
The daemon accepts any loopback redirect (`127.0.0.1` or `[::1]`, any port);
one outside the pinned set simply does not use the document. The identity is
one for every installation, and a public client is protected by PKCE and the
loopback redirect, not by its id.

**A refused document.** When the token endpoint answers `invalid_client` or
`unauthorized_client` to the document, the intent fails with
`client_metadata_refused` and the daemon remembers it for that issuer, for
every connector on it, until it restarts. A reconnect, or a later successful
sign-in, also records it in the cache row, for good. The next attempt
registers instead. A refusal on the sign-in page itself never reaches the
daemon: the next attempt uses the document again, and the fix is a
pre-registered app.

### Scopes

| `scopes` | Asked for |
|---|---|
| given on create | those |
| absent or empty on create | the `scope` of the `401` challenge; else `scopes_supported`; else none |
| absent on reconnect | the connector's current scopes |

The granted scopes are what the token endpoint answers, or the scopes asked
for when it does not say. They are kept in the cache row and shown by `list`
and `show`.

### Binding the request

Always:
- PKCE with `S256` ([RFC 7636](https://www.rfc-editor.org/rfc/rfc7636)). The
  verifier never leaves the daemon.
- A fresh `state` per intent. It travels in the authorization URL and must
  come back with the code.
- The `resource` parameter ([RFC 8707](https://www.rfc-editor.org/rfc/rfc8707))
  on the authorization, token and refresh requests, set to the resource the
  metadata names. That is the server URL or, as Home Assistant publishes its
  origin for `/api/mcp`, a whole-segment prefix of it on the same origin; any
  other is refused.
- `iss` ([RFC 9207](https://www.rfc-editor.org/rfc/rfc9207)) is checked when
  the server promises it or the redirect carries it.

A `state` or `iss` mismatch drops the intent, so a code cannot be replayed
into another one.

## 4. Files and storage

| Where | Holds | Written by |
|---|---|---|
| `connectors.toml` | Intent: id, URL, auth method, scopes, a pre-registered client id, `trusted`, `allow_insecure_transport` | the daemon over the API, or by hand |
| `auth.toml` | `[connectors.<id>]`: `access_token`, `refresh_token`, and `client_secret` when one was issued | the daemon, or `holaclaw auth` |
| The `connectors` table | Cache: state, `detail`, server name and version, tools, auth metadata and client identity, granted client id and scopes, MCP session id, token expiry, last check, consecutive failures | the daemon |
| The daemon's memory | The open intent: PKCE verifier, `state`, expiry | the daemon; never stored |

```toml
# connectors.toml
version = 1

[connectors.github-admin]
url    = "https://mcp.github.com/"
auth   = "oauth"
scopes = ["repo", "issues:write"]   # absent = the server's default (§3)
# client_id = "Iv1.abc123"          # only for a pre-registered app

[connectors.homes]
url     = "https://homes.example.com/mcp"
auth    = "none"                    # an open server: never an auth.toml entry
trusted = true                      # default false (§6)

[connectors.lab]
url  = "http://10.0.4.20:8765/mcp"
auth = "none"
allow_insecure_transport = true     # default false (§11)
```

Eight keys in total: `version`; `connectors.<id>.url` and `.auth` (`none` or
`oauth`), both required; `.scopes`, `.client_id`, `.trusted`,
`.allow_insecure_transport`; and `connectors` in `agent.toml`.

- **The one config file the daemon writes.** The rule that the daemon only
  reads config is relaxed on purpose: everything must be possible from the
  API. Like `auth.toml`, it is a machine file: written whole under a lock,
  mode `0600`, comments not preserved. A `connectors.toml` that is a symlink
  is never written through, and the API answers `409` with what to do by
  hand.
- **Read at start.** An entry written by hand is loaded at the next start,
  and an OAuth one needs a `reconnect` before it has credentials
  (`needs_reconnect`). A URL changed by hand is `expired` with `url_changed`,
  since the tokens belong to the old one.
- **Ids.** `[a-z0-9-]`, at most 32 characters, and never a built-in tool or
  group name, because the id becomes a tool name prefix (§5).
- **Validation.** `check` validates the file like any other:

| Problem | Finds |
|---|---|
| `CFG006` | A `connectors` entry in `agent.toml` that names no connector |
| `CFG010` | A connector id in an agent's `tools`, with a hint pointing at `connectors` |
| `CFG011` | An id outside `[a-z0-9-]{1,32}`, an id that is a built-in tool or group name, or a connector granted twice to one agent |

The cache row's server-controlled JSON (the tool list and the auth metadata)
is capped before it is stored.

## 5. Tools the agent sees

### Names and schemas

- Every tool of a granted connector reaches the agent as `<id>.<tool>`, for
  example `github-admin.create_issue`. Connector tools are not registered in
  the tool registry: the daemon joins the agent's built-in tools and its
  connectors' tools into one executor, under the same policy gate. A name
  offered twice is refused, and an id that names a built-in tool or group
  cannot exist (§4).
- A server tool name must match `[A-Za-z0-9_-]{1,64}` and be unique; others
  are dropped and logged.
- The parameter schema is the server's. A missing one becomes
  `{"type": "object"}`, and one that is not an object schema becomes an empty
  object schema.
- The listing is bounded. A tool with a name over 128 bytes, a description
  over 16 KiB or a schema over 64 KiB is skipped. A listing of more than
  1000 tools, 20 pages or 4 MiB fails (`tools_too_large`).
- The schema cap of docs/context.md applies to holaclaw's own tools only.

### Descriptions

A description is the server's, up to 400 tokens (by holaclaw's estimate). A
longer one is cut on a character boundary when the tool list is cached and
ends with `[description cut at 400 tokens]`; the same marker at the end of
the server's own description is dropped first. The cache row, the API and the
prompt all carry the cut text, and a connected connector with a cut
description has the `detail` `descriptions_truncated` until a refresh no
longer cuts one.

Cutting rather than refusing keeps a connect from ever failing over the
server's prose: the model still gets the tool and the whole schema it needs
to call it, and an admin sees the cut in the status.

### Results

- Text parts are joined. Any other part becomes one line:
  `[image omitted]`, `[audio omitted]`, `[resource omitted: <uri>]`, or
  `[content omitted]`.
- A result over 4 MiB is cut and ends with `[result cut at 4194304 bytes]`.
  After that it is capped and spilled to a blob like any tool result.
- `isError` from the server becomes the tool result's error flag, so the
  model sees the server's own message and can recover.

### Calls

One MCP client per connector, shared by every agent, with 4 calls in flight
at most. Each request gets 60 seconds, including the wait for a slot, a
reconnect and the one retry; the same limit applies to tool listing and
pings. A timeout is an error result, not a state change.

### The prompt and the budget

The context builder adds one fragment, `connectors`, after the skills,
naming the agent's connectors and their tools so the model knows which
service a tool belongs to:

```
Connectors (remote services; their tools are named <connector>.<tool>):
- github-admin: create_issue, list_issues
- homes: lights_off, lights_on
```

Ids and names are sorted, only the tools the model is given are named, and
descriptions stay in the schemas.

Each connector schema is its own fragment, `connector:<id>.<tool>`. The
`connectors` line of `holaclaw prompt-size` and the `connectors` number of
`context.built` are every connector schema plus the fragment above; `system`,
`tool schemas` and `BUD001` leave them out. Offline, `check` and
`prompt-size` read the cached tool lists from the store, opened read-only and
never migrated; with no database they count nothing, and when it cannot be
read they say why on stderr.

- **Over a quarter of the window**: `BUD002` warns, in `check` and in
  `prompt-size`.
- **Over the window**: the context build fails. `check` reports it as
  `BUD001`, `prompt-size` exits with the budget error, and at run time every
  turn of the agent fails with `window too small` until a connector is
  ungranted.

Granting forty tools to a small model is allowed, and visible.

## 6. Permissions and trust

The agent loop, grants, approvals and the guard see a connector tool as any
other tool; the pipeline of docs/security.md is fed, not changed.

**Risk class.** From the MCP tool annotations: `readOnlyHint = true` is
`read`, anything else `external`. A server that sets no annotations gets
`external` for every tool. `destructiveHint = true` makes the gate ask
whatever the class policy says, unless a grant already covers the call. A
`deny` still wins.

**Per-connector policy.** A `policy.tools` key may name a connector or one of
its tools; the more specific key wins:

```toml
# agents/main/agent.toml
connectors = ["github-admin", "homes"]

[policy.tools]
"homes"                      = "allow"   # every tool of the connector
"github-admin.create_issue"  = "ask"     # one tool
```

An override can tighten or loosen, but never lifts a class `deny`. The guest
and task policies apply as for any tool.

**Trust.** A property of the server, so it lives in `connectors.toml` as
`trusted` (default false). By default every result is handled like a `fetch`
result: wrapped as untrusted content, scored once, and able to arm the taint
when the score says the text tries to steer the agent. A `trusted` connector
is one the operator runs or vouches for, an internal service whose content is
theirs.

Trust changes exactly one thing: the result is still wrapped as untrusted
content, so the model treats it as data and never as instructions, but it is
not scored (score 0, `taint_trusted` on `tool.result`) and does not arm the
taint. A turn that reads an internal wiki therefore does not escalate every
later `exec` or `external` call to `ask`. The risk class, the class policy,
grants, approvals and the guard are the same for every connector.

The daemon never infers trust. It is a choice per connector, shown by `show`
and reported by `check` as `CON004`, so it is never set and forgotten. An
internal server is only as trustworthy as whoever can write to it.

## 7. Health

### Protocol and sessions

The client offers protocol `2026-07-28` (stateless: `server/discover`, no
session) and falls back to `initialize` on a server that does not speak it.
Sessions exist on that older protocol only: the `Mcp-Session-Id` the server
returns is kept in the cache row and sent on every request. A `404`, or a
connection lost before the request was sent, makes the daemon run the
handshake again once and retry; a second loss is `session lost`. On a modern
server the session id stays empty.

### Tokens

Only the daemon refreshes tokens, one refresh at a time per connector:

- before a call, when the access token expires within 60 seconds;
- from a check, when it would expire before the next one;
- after a `401` on a call: one refresh and one retry.

A refresh that fails for a passing reason waits 30 seconds before the next
try (`refresh_failed`). A rejected refresh (`invalid_grant`) moves the
connector to `expired` at once (`refresh_rejected`). Every successful refresh
emits `auth.refreshed`.

### Checks

The daemon checks every connector at start and every 10 minutes, except
`expired` ones and those with an open intent. A check dials the server,
pings it (`server/discover` on a modern server, which has no `ping`), lists
the tools and refreshes the token when due.

- Three failed checks in a row move a `connected` or `pending` connector to
  `degraded`; one success moves it back to `connected`.
- A `401` from an open server, on a check or a call, moves it to `degraded`
  at once (`open_server_unauthorized`).
- On a modern server the check reconnects, reopening the
  `subscriptions/listen` stream that carries `list_changed`, so a silently
  dropped stream costs at most one interval.

### Tool list changes

`notifications/tools/list_changed` refreshes the cached tool list, at most
once every 10 seconds per connector, and so does every check. The tool specs
are rebuilt every turn, so a running session sees the new list on its next
turn.

## 8. CLI and API

```
holaclaw connectors create <id> <url> [--auth none|oauth] [--scopes a,b] [--listen]
                                      [--client-id id] [--client-secret-stdin]
                                      [--allow-insecure-transport]
holaclaw connectors reconnect <id>   [--scopes a,b] [--listen] [--client-secret-stdin]
holaclaw connectors code <id> <redirect-url>
holaclaw connectors inspect <url>
holaclaw connectors list
holaclaw connectors show <id>
holaclaw connectors delete <id>      [--yes]
```

Every subcommand also takes `--addr` and `--token` (`$HOLACLAW_ADDR`,
`$HOLACLAW_TOKEN`), like any API client.

- `--client-secret-stdin` reads the secret from stdin, so it never lands in
  a shell history.
- `delete` asks for a typed `yes` unless given `--yes`; with nothing on
  stdin it deletes nothing. It looks the id up first: an id the daemon does
  not list gets a shorter question, and the delete still removes an entry
  written by hand since the start, or what a failed delete left behind.
- `holaclaw status` lists every connector that is not `connected`. The list
  needs the `admin` scope, so with any other token `status` leaves these
  lines out.

### Receiving the redirect

The CLI connects in one of two ways. Both go through the same API; they
differ only in who receives the redirect.

**By default**, the command creates the connector, prints the authorization
URL and returns. The person opens it in any browser and signs in, and the
browser is sent to `http://127.0.0.1:26500/callback`, where nothing listens.
The person copies the address from the address bar and passes it to
`holaclaw connectors code`, which extracts `code`, `state` and `iss` and
posts them. An address carrying `error` is printed and nothing is posted.
Nothing has to reach holaclaw from the browser, so this works wherever the
daemon runs: a remote host, a container, a VM with no SSH server.

**With `--listen`**, the command binds the first free port of the pinned set
on `127.0.0.1` before it creates the connector, prints the authorization URL,
opens the browser when it can, receives the redirect itself and posts the
code. It takes only a redirect carrying the `state` of the URL it printed,
and gives up when the intent expires. This needs the browser on the same
machine as the CLI, or a tunnel to it:
`ssh -L 26500:127.0.0.1:26500 -L 26501:127.0.0.1:26501 -L 26502:127.0.0.1:26502 -L 26503:127.0.0.1:26503 -L 26504:127.0.0.1:26504 host`.
There is no port option: the ports are fixed by the metadata document (§3).

### Endpoints

Every endpoint needs the `admin` scope. The schemas are in
[`api/openapi.yaml`](../api/openapi.yaml).

| Method | Path | Does |
|---|---|---|
| `POST` | `/v1/connectors` | Creates a connector. Body: `id`, `url`, `auth?`, `scopes?`, `redirect_uri?` (required unless the server is open), `client_id?`, `client_secret?`, `allow_insecure_transport?`. |
| `GET` | `/v1/connectors` | Every connector: URL, auth, requested and granted scopes, client id, state, `detail`, server name and version, tool count, last check. |
| `GET` | `/v1/connectors/{id}` | One connector, plus `trusted`, its tools (name, description, risk class, whether every call asks, whether the description was cut) and the auth metadata: issuer, resource, endpoints, the scopes asked for, the client identity kind and the granted client id. |
| `POST` | `/v1/connectors/{id}/code` | Body: `code`, `state`, `iss?`. Finishes the open intent and answers with the settled connector. |
| `POST` | `/v1/connectors/{id}/reconnect` | Body: `scopes?`, `redirect_uri`, `client_secret?`. Answers like create. |
| `DELETE` | `/v1/connectors/{id}` | Deletes the connector (§2). |
| `POST` | `/v1/connectors/inspect` | Body: `url`. Stores nothing. Answers whether the server is open, its name, version and tools when an unauthenticated handshake and listing worked, the authorization servers, `scopes_supported`, the auth metadata, and the client identity a create with the default redirect would use. Bounded at 90 seconds, and never takes `allow_insecure_transport`. |

Create and reconnect answer `202` with the connector, its
`authorization_url` and `expires_at`, or `200` and `connected` for an open
server.

| Status | Create | Reconnect | Code | Delete |
|---|---|---|---|---|
| `400` | a bad field, a built-in name, an auth mismatch | a bad field, a missing or stray `client_secret` | a missing field; a `state` or `iss` mismatch, which drops the intent | |
| `404` | | no such id | no such id | nothing of the id is left |
| `409` | the id is in use; `connectors.toml` does not parse, or is a symlink when the entry must be written | an intent is being opened or its code exchanged | no open intent; `connectors.toml` is a symlink or does not parse | `connectors.toml` is a symlink holding the entry, or does not parse |
| `500` | | | | the entry is gone, but the keys or the cache row are left |
| `502` | discovery, client identity or the server failed | the same | the authorization server refused the code | |

An OAuth create writes `connectors.toml` only when the code arrives, so a
symlinked file gives its `409` at `code`. Then a create keeps nothing, new
tokens included: add the entry by hand, restart, and reconnect. A reconnect
that changed the scopes keeps the new tokens with the old scopes in the
entry: set them by hand and restart. A reconnect that keeps the entry as it
is does not write the file.

## 9. `check` rules

| Rule | Level | Finds | Fix |
|---|---|---|---|
| `CON001` | error | An `auth = "oauth"` connector with no `access_token` in `auth.toml` | `holaclaw connectors reconnect <id>` |
| `CON002` | warning | The cache row says `expired` | `holaclaw connectors reconnect <id>` |
| | | The cache row says `degraded` | `holaclaw connectors show <id>`; reconnect if it persists |
| `CON003` | error | A connector URL is plain `http` on a non-loopback host, without `allow_insecure_transport` | an https URL, or a loopback address |
| `CON004` | info | A connector is `trusted`: its results never arm the taint | confirm, or set `trusted = false` |
| `SEC004` | warning | An `auth.toml` entry for a connector no longer in the config | `holaclaw auth rm connectors.<id>` |
| `BUD002` | warning | An agent's connector schemas take over a quarter of the chat model's window (§5) | grant fewer connectors, or use a model with a larger window |

The `CON` rules skip ids that `CFG011` already reports.

## 10. Events and `detail`

| Event | When | Carries |
|---|---|---|
| `connector.status` | A connector's state or `detail` changes, and its last event as `deleted` | `id`, `state`, `detail` (empty when none), `scopes` when known |
| `policy.decision`, `guard.decision`, `tool.result` | Every connector call, as for any tool | `connector = "<id>"` next to the tool name; `tool.result` also `taint_trusted` |
| `auth.refreshed` | Every successful token refresh | `kind = "connector"`, `id`, `expires_at` when known |

Tokens, codes and the verifier never appear in an event. `detail` is one of
these daemon strings, never server text:

| `detail` | Set when |
|---|---|
| `intent_expired` | The sign-in was not finished within 15 minutes. |
| `state_mismatch` | A code came back with another intent's `state`. |
| `authorization_failed` | The code exchange failed: the authorization server refused it, or `iss` did not match. |
| `client_metadata_refused` | The token endpoint refused the metadata document (§3). |
| `discovery_failed` | The server publishes no usable authorization metadata. |
| `client_identity_failed` | No client identity worked: the server needs a pre-registered app, or registration failed. |
| `auth_mismatch` | `--auth` disagrees with the server. |
| `server_unreachable` | The server did not answer the MCP handshake. |
| `ping_failed` | A check's ping failed. |
| `tools_list_failed` | Listing the tools failed. |
| `tools_too_large` | The tool list is over the listing bounds (§5). |
| `refresh_failed` | A refresh failed for a passing reason. |
| `refresh_rejected` | The refresh was rejected: the sign-in is dead. |
| `unauthorized` | The server still answered `401` after a refresh. |
| `open_server_unauthorized` | A server connected as open now answers `401`. |
| `needs_reconnect` | An OAuth entry has no credentials, such as one written by hand. |
| `url_changed` | The URL changed and the tokens belong to the old one. |
| `config_write_failed` | `connectors.toml` or `auth.toml` could not be written. |
| `cache_write_failed` | The cache row could not be written. |
| `descriptions_truncated` | A tool description was cut at 400 tokens (§5). |

## 11. Security

- **Secrets.** The `auth.toml` rules apply unchanged: write-only over the
  API, never in traces, prompts, results or exec children, out of backups
  unless asked.
- **The intent.** Single-use and short-lived; a `state` or `iss` mismatch
  drops it. The `code` endpoint needs the `admin` scope. While a sign-in is
  open its authorization URL is sensitive: whoever completes it binds their
  account to the connector.
- **One server per token.** `resource` binds each token to one server URL,
  so changing a connector's URL means a reconnect.
- **Transport.** The server URL must be https unless it is loopback
  (`CON003`). Authorization metadata, the authorization server and the token
  endpoint are https only, whatever the connector says. Discovery follows at
  most 5 redirects, for GET requests only and only to https; token and
  registration requests never follow one. The MCP client follows redirects
  only on the same origin.
- **`allow_insecure_transport = true`** lifts the https rule for one
  connector's MCP endpoint only: plain http to any host, or https with a
  certificate that does not verify, for a server on a network the operator
  controls. It is written by hand or by a create with
  `--allow-insecure-transport`; a reconnect keeps it, and only a hand edit
  removes it. `inspect` never takes it. A bearer token over plain http is
  readable on the way.
- **Bounded server input.** Authorization metadata is capped (64 items per
  list, 256 bytes per item, 2 KiB per URL, 256 KiB per body), as are tool
  listings (§5) and results (§5).
- **Untrusted results.** Results from every connector are wrapped as
  untrusted content. Those of an untrusted connector also follow the
  escalation rules of docs/security.md; `trusted` switches off the
  escalation only, explicitly and visibly (§6).

## 12. Limits

- Remote servers over Streamable HTTP only; no stdio servers.
- Tools only: no resources, prompts, sampling, elicitation or MCP Apps.
- Tool results are text: other content is replaced by one line (§5).
- `auth` is `none` or `oauth`.
- A connector acts as one account for everyone (§1).
- A call that lacks a scope is an ordinary failed call; the fix is a
  reconnect with wider scopes.
