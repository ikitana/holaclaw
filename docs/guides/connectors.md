# Connectors: setup and daily use

This guide connects a remote MCP server to holaclaw and gives its tools to an
agent. It is for whoever runs holaclaw. Every step is written out with the
command to type and what it prints. The design behind it is in
[connectors.md](../connectors.md); you don't need it to follow this guide.

A **connector** is one connection to one remote MCP server (Streamable HTTP,
tools only), kept under an id you choose, such as `tracker`. You connect it
once, signing in to the service if it needs a sign-in, and then grant it to
the agents that may use it. Every tool of the server reaches those agents as
`<id>.<tool>`, for example `tracker.create_issue`, and acts as the account
that signed in when the connector was connected, whoever is talking to the
agent.

## Contents

- [1. Requirements](#1-requirements)
- [2. Check a server first](#2-check-a-server-first)
- [3. Connect a server](#3-connect-a-server)
- [4. See the result](#4-see-the-result)
- [5. Grant it to an agent](#5-grant-it-to-an-agent)
- [6. Policy and trust](#6-policy-and-trust)
- [7. Reconnect](#7-reconnect)
- [8. Health](#8-health)
- [9. Delete](#9-delete)
- [10. holaclaw check](#10-holaclaw-check)
- [11. Troubleshooting](#11-troubleshooting)
- [12. Test it manually](#12-test-it-manually)

## 1. Requirements

- **holaclaw installed and initialised**, with the daemon running: see the
  [deployments](../deployments/README.md). Commands are written as
  `holaclaw …`; run them
  [the way your deployment does](../deployments/README.md#run-a-command).
  Every `holaclaw connectors` command talks to the daemon: it does the
  discovery, the sign-in exchange, token refresh and health checks. Its
  [log](../deployments/README.md#logs) says what failed.
- **Admin access to the daemon.** On the daemon's machine the commands use
  the control socket, `server.socket` in `config.toml`, or
  `/run/holaclaw/holaclaw.sock` when that is unset. The socket is admin by its
  file permissions, so there is nothing to set. From another machine, point the CLI at the daemon's TCP
  address (`server.listen`) with `--addr` (or `$HOLACLAW_ADDR`) and put an admin token in
  `$HOLACLAW_TOKEN`. That is the admin token `init` printed, or wrote to the
  file named by `--token-file` (on the daemon's machine) when you followed
  getting-started: copy it over and `export HOLACLAW_TOKEN=<token>`.
- **The URL of the MCP server**, its Streamable HTTP endpoint. It must be
  `https`, or plain `http` on a loopback address (`127.0.0.1`, `localhost`).
  A server on your own network can go without with `--allow-insecure-transport`
  (§3.4).
- **For a server that needs a sign-in**: an account on the service, and a
  browser. The browser can be on any machine (§3.2).

## 2. Check a server first

`inspect` asks the server what it is without connecting it. Nothing is
stored, and no client is registered with the server.

```
holaclaw connectors inspect https://mcp.example.com/mcp
```

An **open server** (no sign-in) answers with its name and tools (the values
below are examples):

```
url: https://open.example.com/mcp
open: yes, no sign-in needed
server: example-weather 1.4.0

tools (2):
  forecast  read      Get the weather forecast for a place
  report    external  Send a weather report to a subscriber
```

The second column is the tool's risk class, `read` or `external` (§6).

A **server that needs a sign-in** (OAuth) answers with where you will sign in
and how holaclaw will name itself there:

```
url: https://mcp.example.com/mcp
open: no, it needs a sign-in
authorization servers: https://auth.example.com
issuer: https://auth.example.com
authorization endpoint: https://auth.example.com/authorize
token endpoint: https://auth.example.com/token
registration endpoint: https://auth.example.com/register
scopes supported: issues:read issues:write
scopes offered: issues:read issues:write
client identity: cimd
```

The **client identity** line says what you have to do before `create`:

| `client identity` | Meaning | What you do |
|---|---|---|
| `cimd` | holaclaw names itself with its published client metadata document. The consent screen says "HolaClaw". | Nothing. |
| `dcr` | holaclaw registers itself with the server when you create the connector. | Nothing. |
| `pre-registered (the server offers no other: create needs --client-id)` | The server only accepts apps created in its own console. | Create an OAuth app in the service's console (§3.4), then pass `--client-id`. |

`scopes supported` and `scopes offered` are the permissions the server knows.
Without `--scopes`, holaclaw asks for all of them.

## 3. Connect a server

Choose an id: lowercase letters, digits and `-`, at most 32 characters, and
not the name of a built-in tool or group (`memory`, `files`, `fetch`, …). The
id becomes the prefix of every tool name, and agents refer to the connector
by it.

### 3.1 An open server

```
holaclaw connectors create weather https://open.example.com/mcp
```

There is no sign-in, so it connects at once and prints:

```
weather: connected to example-weather
```

### 3.2 A server that needs a sign-in: copy the address

This is the default mode. It works wherever the daemon runs (a remote host, a
container, a VM) and the browser can be on any machine.

1. Create the connector:

   ```
   holaclaw connectors create tracker https://mcp.example.com/mcp
   ```

   It prints the sign-in URL and returns:

   ```
   Open this URL in a browser and sign in:

       https://auth.example.com/authorize?client_id=…&state=…

   The browser then goes to http://127.0.0.1:26500/callback, which does not load. Copy the whole address
   from the address bar and run, right after signing in:

       holaclaw connectors code tracker '<address>'

   holaclaw takes the address until 14:37:05. The server may stop taking its code sooner.
   ```

   The time is when the daemon drops the sign-in, 15 minutes after it
   opened it by the daemon's clock, shown in your time zone. It is
   holaclaw's limit only. The service gives the code in the address a
   lifetime of its own, counted from the moment you sign in and often
   shorter (10 minutes at most is the usual advice), and nothing in the
   address says how long. So run `code` as soon as the browser lands.
   Restarting the daemon closes every open sign-in, whatever the time.
2. Open the URL in any browser and sign in with the account the connector
   should act as. Approve what the consent screen asks.
3. The browser is sent to `http://127.0.0.1:26500/callback?code=…&state=…`
   and shows an error page saying it cannot connect to `127.0.0.1` (the
   wording depends on the browser). That is expected: nothing listens there.
   If something on that machine does listen on port 26500, for example
   another `holaclaw connectors create --listen`, the page may say "This is
   not the sign-in holaclaw is waiting for." That is fine too.
4. Copy the **whole** address from the address bar and pass it to `code`,
   in **single quotes** (it contains `&` and `?`, which the shell would
   otherwise interpret):

   ```
   holaclaw connectors code tracker 'http://127.0.0.1:26500/callback?code=…&state=…'
   ```

   The address carries the authorization code, which works once and only
   for a few minutes, so leaving it in your shell history is acceptable;
   still, don't paste it anywhere others can read it.

   It returns once the daemon has exchanged the code and listed the tools:

   ```
   tracker: connected to Example Tracker, scopes issues:read issues:write
   ```

   The server name and the granted scopes show which account and permissions
   you got. If they are wrong, sign in again with `reconnect` (§7).

Whoever opens the sign-in URL binds their account to the connector, so do not
paste it anywhere others can open it while it is valid.

### 3.3 A server that needs a sign-in: `--listen`

With `--listen` the command receives the browser's redirect itself and waits
until the connector is connected. It needs the browser on the same machine
as the command, or a tunnel to it.

```
holaclaw connectors create tracker https://mcp.example.com/mcp --listen
```

```
Open this URL in a browser and sign in:

    https://auth.example.com/authorize?client_id=…&state=…

opened the browser
waiting for the browser on http://127.0.0.1:26500/callback… (Ctrl-C stops waiting)
```

The URL goes to stdout; `opened the browser` and `waiting for the browser on
…` go to stderr. `opened the browser` appears only when the command could
start one (with `xdg-open`, or `open` on macOS); otherwise open the URL
yourself. After you
sign in, the browser tab says "holaclaw got the sign-in. You can close this
tab and go back to the terminal." and the command prints:

```
tracker: connected to Example Tracker, scopes issues:read issues:write
```

If you decline on the service's page, the tab says "The sign-in was refused.
Go back to the terminal." and the command prints the refusal (§11).

The command listens on `127.0.0.1`, on the first free port of 26500 to 26504.
There is no option to pick another port: these five redirect addresses are
fixed in holaclaw's client metadata document, and authorization servers
accept only an exact match. If all five are taken, the command stops with
`--listen: ports 26500 to 26504 on 127.0.0.1 are all in use; free one, or
run without --listen`.

**Browser on another machine (SSH tunnel).** When you run the CLI over SSH on
the daemon's host and the browser is on your laptop, forward the port from
the laptop:

1. On the laptop, forward all five ports, because the command takes the
   first free one on the host:
   `ssh -L 26500:127.0.0.1:26500 -L 26501:127.0.0.1:26501 -L 26502:127.0.0.1:26502 -L 26503:127.0.0.1:26503 -L 26504:127.0.0.1:26504 host`
   (those ports must be free on the laptop).
2. In that SSH session: `holaclaw connectors create tracker https://mcp.example.com/mcp --listen`.
   The `waiting for the browser on …` line names the port it took.
3. Open the printed URL in the laptop's browser and sign in. The redirect to
   that port on the laptop goes through the tunnel to the command.

If you stop waiting with Ctrl-C, the sign-in stays open until its 15 minutes
pass: finish the sign-in in the browser, copy the address it lands on, and
pass it to `holaclaw connectors code` as in §3.2.

### 3.4 Options: `--auth`, `--scopes`, a pre-registered app

- `--auth none` or `--auth oauth` states what you expect. Without it,
  holaclaw uses what the server answers. Given, a server that disagrees fails
  the create with `server does not use the requested auth`.
- `--scopes a,b` asks for these scopes only, comma separated, for example
  `--scopes issues:read`. Without it, the scopes the server asks for when it
  answers without a sign-in, or else every scope it offers. A `reconnect`
  without it keeps the scopes the connector has.
- `--client-id <id>` uses an app you created in the service's console. Only
  needed when `inspect` says `pre-registered`. When you create the app, give
  it these five redirect URIs, exactly (`127.0.0.1`, not `localhost`):

  ```
  http://127.0.0.1:26500/callback
  http://127.0.0.1:26501/callback
  http://127.0.0.1:26502/callback
  http://127.0.0.1:26503/callback
  http://127.0.0.1:26504/callback
  ```

  They are the redirect URIs holaclaw's own client identity lists, in
  `https://holaclaw.ai/oauth/client-metadata.json`.

- `--client-secret-stdin` reads that app's secret from stdin, if the console
  gave it one. It never goes on the command line, where the shell history
  and `ps` would keep it. Pipe it from a file or a password manager:

  ```
  holaclaw connectors create corp https://mcp.example.com/mcp \
    --client-id my-app-id --client-secret-stdin < ~/.config/corp-mcp-secret
  ```

  ```
  pass show corp/mcp-client-secret | holaclaw connectors create corp https://mcp.example.com/mcp \
    --client-id my-app-id --client-secret-stdin
  ```

  Never `echo` the secret into the pipe. The daemon stores it in `auth.toml`.

- `--allow-insecure-transport` reaches a server on your own network over
  plain `http` on any host, or `https` with a certificate that does not
  verify, for example `http://10.0.4.20:8765/mcp`. The connector keeps it
  as `allow_insecure_transport = true`. Anyone on the way can read the
  traffic, a sign-in token included; the sign-in itself always needs https.

The client id goes to `connectors.toml`; the tokens and the secret go to
`auth.toml`, which no API answer, trace or prompt ever shows.

## 4. See the result

**Every connector:**

```
holaclaw connectors list
```

```
ID       STATE      SERVER           TOOLS  SCOPES                    LAST CHECK            DETAIL
tracker  connected  Example Tracker  3      issues:read issues:write  2026-10-22 09:14:03Z  descriptions_truncated
weather  connected  example-weather  2      -                         2026-10-22 09:14:03Z  -
```

With none, it prints `no connectors`.

**One connector, with its tools:**

```
holaclaw connectors show tracker
```

```
id: tracker
url: https://mcp.example.com/mcp
state: connected (descriptions_truncated)
server: Example Tracker 2.1.0
auth: oauth
scopes granted: issues:read issues:write
trusted: no
last check: 2026-10-22 09:14:03Z
resource: https://mcp.example.com/mcp
issuer: https://auth.example.com
authorization endpoint: https://auth.example.com/authorize
token endpoint: https://auth.example.com/token
registration endpoint: https://auth.example.com/register
scopes offered: issues:read issues:write
client identity: cimd
client id: https://holaclaw.ai/oauth/client-metadata.json

tools (3):
  close_issue   external, asks  Close an issue
  create_issue  external        Create an issue in a project
  list_issues   read            List the issues of a project [cut]

[cut]: the server's description was over 400 tokens; the agent sees it cut
```

A line with no value is left out (`scopes requested` appears only when the
connector asks for its own scopes, from `--scopes` or `connectors.toml`). In the tool list:

- `read` or `external` is the risk class (§6).
- `asks` means every call of that tool needs a person's approval (§6).
- `[cut]` means the server's description was over 400 tokens and the agent
  gets it cut, ending in `[description cut at 400 tokens]`. The connector's
  detail is then `descriptions_truncated`. Nothing is wrong: the agent still
  gets the tool and its full parameter schema.

**States:**

| State | Meaning |
|---|---|
| `pending` | Just created, or loaded at start and not checked yet. |
| `authorizing` | A sign-in is open and waiting for its code. |
| `connected` | Working. |
| `degraded` | Checks are failing; calls still try (§8). |
| `expired` | The sign-in is dead. Calls fail until you reconnect (§7). |

**`holaclaw status`** adds one line per connector that is **not** connected,
after the channels:

```
CONNECTOR  STATE    DETAIL
tracker    expired  refresh_rejected
```

With every connector connected, there is no such table. The lines need an
admin token, so with any other token `status` leaves them out. Every
`detail` is listed in §11.

## 5. Grant it to an agent

Create the connector **before** you name it in an agent (§10 says why).

1. Edit the agent's `agent.toml`, `agents/main/agent.toml` in the [config directory](../deployments/README.md#config-directory)
   for the default agent. Add a `connectors` line among the top-level keys,
   next to `tools` and **above** the first `[table]` such as `[models]`
   (a key below a table header belongs to that table):

   ```toml
   version    = 1
   tools      = ["memory", "files", "fetch", "attach", "task.create"]
   connectors = ["tracker", "weather"]

   [models]
   chat = "local"
   ```

   Do not put a connector id in `tools`: `holaclaw check` reports that as
   `CFG010`, with a hint saying to grant it in ``connectors``, not
   ``tools``.
2. Check the config:

   ```
   holaclaw check --offline
   ```

3. [Restart](../deployments/README.md#restart) the daemon. The
   daemon reads `agent.toml` only at start.
4. Talk to the agent through the daemon, with `holaclaw chat` (it attaches to
   the running daemon) or a channel. `holaclaw chat` with no daemon running
   has no connectors.

The agent now has every tool of each granted connector, named
`<id>.<tool>` (`tracker.create_issue`, `weather.forecast`), and its system
prompt carries one short list so the model knows which service a tool
belongs to:

```
Connectors (remote services; their tools are named <connector>.<tool>):
- tracker: close_issue, create_issue, list_issues
- weather: forecast, report
```

Agents granted the same connector share its sign-in. When the server changes
its tool list, holaclaw refreshes it on its own and agents see it from their
next turn; no restart is needed.

**Context cost.** Every connector tool adds its parameter schema and its
description (up to 400 tokens) to every request the agent makes. See how much
with:

```
holaclaw prompt-size --agent main
```

The `connectors` row is the list above plus every connector schema; `tool
schemas` and `fixed overhead` leave connectors out. The numbers come from the
tool lists the daemon cached, so connect the connector first. When the
connectors take more than a quarter of the chat model's window,
`prompt-size` and `holaclaw check` print a `BUD002` warning (§10). Granting
many tools to a model with a small window is allowed, but if they do not fit
at all, every turn of that agent fails with a `window too small` error until
you grant fewer.

## 6. Policy and trust

A connector tool goes through the same policy gate as any other tool. What
decides a call, in order:

1. **Risk class.** A tool the server marks read-only (`readOnlyHint`) is
   `read`; every other tool is `external`, including every tool of a server
   that sets no hints. `show` prints the class of each tool.
2. **The class policy** of the agent, `[policy]` in `agent.toml`. With the
   defaults, `read` and `external` are both `allow`, so the owner's calls run
   without asking. Guests have `external = "deny"` by default
   (`[policy.guest]`): their agent can only call the `read` tools. Detached task runs
   allow both by default.
3. **Per-tool overrides**, `[policy.tools]` (below).
4. **Always-ask tools.** A tool the server marks destructive
   (`destructiveHint`) shows `asks` in `show`: an `allow` becomes `ask` for
   every call, whatever the class and the overrides say. Only a person's
   approval answers it; a `deny` still wins. A detached task has nobody to
   ask, so it is denied.
5. **Taint.** With `taint_escalates = true` in `[policy]`, an `external`
   call made shortly after a connector (or `fetch`) result that looked like
   it tries to steer the agent becomes `ask`. `read` calls never escalate.

A call that asks shows up as an approval prompt in `holaclaw chat`, or in the
conversation's channel for an owner or admin. From another terminal,
`holaclaw approvals` lists the pending ones and `holaclaw approve <id>`
answers one (`--session` for the rest of the session, `--always` from now
on, for you only).

**Overrides for connector tools.** A key in `[policy.tools]` can name a
whole connector or one of its tools; the more specific key wins. Values are
`allow`, `ask` or `deny`. Quote every key. The `[policy.tools]` table goes
after the top-level keys, for example at the end of `agent.toml` after
`[policy]`; if the file already has one, add the lines to it:

```toml
# agents/main/agent.toml
version    = 1
tools      = ["memory", "files", "fetch", "attach", "task.create"]
connectors = ["tracker", "weather"]

[models]
chat = "local"

[policy]
read     = "allow"
write    = "allow"
external = "allow"
exec     = "deny"

[policy.tools]
"weather"              = "allow"   # every tool of the connector
"tracker"              = "ask"     # every tool of tracker…
"tracker.list_issues"  = "allow"   # …but this one runs unasked
"tracker.close_issue"  = "deny"    # and this one never runs
```

An override can loosen or tighten, but never lifts a class `deny`, and for
guests it can only tighten. `holaclaw check` refuses a key that names no
connector (`CFG010`), but it does not check the tool part: a typo in
`"tracker.lsit_issues"` silently matches nothing. Restart the daemon after
editing.

**Trust.** By default every connector result is handled like a `fetch`
result: the model sees it wrapped as untrusted content, and it is scored to
see whether it tries to steer the agent. A high score arms the taint, which
with `taint_escalates = true` turns the next `external` calls into `ask`, and
in `guard_mode = "auto"` sends them to a person instead of the guard. A
connector you run or vouch for can skip that:

```toml
# connectors.toml
[connectors.wiki]
url     = "https://wiki.internal.example/mcp"
auth    = "oauth"
trusted = true
```

Edit the file by hand (the CLI has no flag for it) and restart the daemon.
`show` then says `trusted: yes`. The results of a trusted connector are still
wrapped as untrusted content, so the model treats them as data and never as
instructions; they are just never scored and never arm the taint. Risk
classes, the class policy, overrides, approvals and the guard stay exactly
the same. Without `taint_escalates = true` or `guard_mode = "auto"`, the flag
changes nothing you would notice.

Do not set `trusted` on a server whose content other people can write: an
issue tracker that outsiders file issues in, a shared wiki, a mailbox, a
chat. An internal server is only as trustworthy as whoever can write to it,
and text planted there is exactly what the taint exists to catch. Leave it
off unless the content is yours.

## 7. Reconnect

`reconnect` opens a new sign-in on an existing connector. It works like
`create`, in both modes:

```
holaclaw connectors reconnect tracker            # then: holaclaw connectors code tracker '<address>'
holaclaw connectors reconnect tracker --listen
```

The output is the same as in §3.2 and §3.3. While the new sign-in is open,
`list` shows the connector as `authorizing`, and the old tokens keep serving
the agents until the new code is exchanged. If you abandon the sign-in, after
15 minutes the connector returns to its previous state with the detail
`intent_expired`.

**An expired connector.** `status` shows `expired` with `refresh_rejected`:
the service revoked or expired the sign-in. Its tools stay listed, and every
call answers the agent "connector tracker needs to be reconnected by an
admin", so the agent can tell the person. Run `reconnect` and sign in again.

**Other scopes.** `holaclaw connectors reconnect tracker --scopes issues:read`
asks for these scopes instead; once the code is exchanged they are written to
`connectors.toml`. Without `--scopes`, reconnect keeps the connector's
scopes.

**A pre-registered app.** Reconnect reuses the secret in `auth.toml` while
the server's issuer and the client id stay the same. When they changed, it
stops with `client_secret is required: the stored one belongs to another
issuer or client`: run it again with the secret on stdin:

```
holaclaw connectors reconnect corp --client-secret-stdin < ~/.config/corp-mcp-secret
```

**An open server.** There is no sign-in: `reconnect` runs one health check
at once and prints the state after it, such as `weather: connected to
example-weather`. One failed check does not change the state (it takes three
in a row, §8), so `connected` here does not prove the server answered:
compare `last check` in `show`, and read the daemon's log. Only a server that
now asks for a sign-in fails at once, with `weather: server does not use the
requested auth` (§8).

**An entry written by hand.** You can add a connector to
`connectors.toml`, in the config directory, yourself:

```toml
version = 1

[connectors.tracker]
url    = "https://mcp.example.com/mcp"
auth   = "oauth"                    # or "none" for an open server
scopes = ["issues:read"]            # optional; absent = the server's default
# client_id = "my-app-id"           # only for a pre-registered app
```

The daemon reads it at start, so restart it. An `auth = "none"`
entry connects on the first check, right after the start. An
`auth = "oauth"` entry has no credentials yet: `status` shows it `expired`
with `needs_reconnect`, and `holaclaw connectors reconnect tracker` gives it
its first sign-in (with `--client-secret-stdin` if its app has a secret).

The daemon rewrites `connectors.toml` whole when it changes it: other entries
and their keys, `trusted` included, are kept, but comments are not, and the
file ends up with mode 0600 whatever it had before.

**A changed URL.** The tokens are bound to the server URL. If you change
`url` of an OAuth connector by hand, after the restart it is `expired` with
`url_changed`: reconnect it.

## 8. Health

The daemon checks every connector when it starts and then every 10 minutes:
it pings the server, refreshes the access token when it is due, and
refreshes the cached tool list. From your side:

- **Three failed checks in a row** move a connected connector to `degraded`,
  with the reason in `detail` (§11). With the 10-minute interval that is
  20 to 30 minutes after the server goes away. Calls keep trying meanwhile,
  and a call that fails answers the agent an error it can read.
- **One successful check** moves it back to `connected`. You don't have to do
  anything for a server that comes back.
- **A refresh the service rejects** moves it to `expired` at once. Expired
  connectors are no longer checked: they need a `reconnect` (§7).
- **An open server that starts asking for a sign-in** is `degraded` with
  `open_server_unauthorized` at once. Delete it and create it again, so
  holaclaw discovers the sign-in (§9, §3.2).

There is no command to force a check. To check now, restart the daemon, or
for an open server run `holaclaw connectors reconnect <id>` (one check, §7). An OAuth
connector stuck `degraded` with `unauthorized` or `refresh_failed` that does
not recover is best fixed with a reconnect.

## 9. Delete

```
holaclaw connectors delete tracker
```

It asks first, on stderr:

```
Delete connector tracker?
Its entry in connectors.toml, its sign-in in auth.toml and its cached tools are removed.
Agents that list it lose its tools, and `holaclaw check` reports them until the id is removed.
Type yes to go on:
```

Type `yes` and press Enter: it prints `tracker: deleted`. Anything else
prints `not deleted` and exits with status 1. `--yes` skips the question,
for scripts; with nothing on stdin and no `--yes`, nothing is deleted. A
delete works in any state, an open sign-in included.

**If a delete fails halfway.** When the entry is gone but removing the
sign-in or the cached tools failed, the delete answers:

```
holaclaw: tracker: connector: deleted, but removing its sign-in or cache failed; delete tracker again before creating it again
```

The connector is gone from `list`, and the daemon's log says what failed.
Run `holaclaw connectors delete tracker` again before you create anything
named `tracker`, because that delete removes whatever has the id. Since the
id is no longer listed, it asks a shorter question first
(`tracker is not listed; whatever is left of it in connectors.toml, auth.toml
and the cache is removed.`), then `Type yes to go on:`.

**Then remove the id from every agent.** Running agents lose its tools at
once, but `connectors = ["tracker"]` in an `agent.toml` now names nothing,
and so does a `[policy.tools]` key for it. Both are config errors (`CFG006`,
`CFG010`): at the next start the daemon refuses to start, the commands
that load the config to find the daemon (`connectors`, `status`, …) print the
error instead of running, and so does `prompt-size`, which reads the config
offline (§10). `holaclaw check`
still runs and lists them. Remove the id from `connectors` and from
`[policy.tools]` in every agent, then run `holaclaw check --offline`.

**A symlinked `connectors.toml`.** If `connectors.toml` is a symlink (for
example, managed by home-manager or your dotfiles), the daemon never writes
through it, and delete stops without removing anything:

```
holaclaw: tracker: connector: connectors.toml is a symlink the daemon does not write: remove [connectors.tracker] there by hand, then delete again
```

Remove the `[connectors.tracker]` table from the file the symlink points to,
then run `holaclaw connectors delete tracker` again: it removes the sign-in and
the cached tools.

The daemon can't write the file for `create` either. With a symlinked file a
create's sign-in fails at the `code` step (or at the end of `--listen`) and
leaves nothing behind, its tokens included:

```
holaclaw: tracker: connector: connectors.toml is a symlink the daemon does not write: nothing was kept; add [connectors.tracker] there by hand, restart the daemon, then reconnect tracker
```

Write the entry by hand ("An entry written by hand" in §7), restart the daemon so it loads
it, then run `holaclaw connectors reconnect tracker`. For an open server there
is no sign-in: its create fails at once with the same message, and an
`auth = "none"` entry written by hand connects at the next start, with no
reconnect needed.

A **reconnect** works over a symlinked file as long as its entry does not
change. One that changes `--scopes` stores the new sign-in in `auth.toml` but
cannot save the scopes, and says so:

```
holaclaw: tracker: connector: connectors.toml is a symlink the daemon does not write: the new sign-in is stored, but [connectors.tracker] keeps its old scopes; set them there by hand and restart the daemon
```

The connector then shows the detail `config_write_failed` until you do. It
also keeps the state it had before, so a hand-written entry that was still
`expired` stays `expired`: set the scopes by hand, restart the daemon, then
run `holaclaw connectors reconnect tracker` again without `--scopes`.

## 10. holaclaw check

`holaclaw check` (or `holaclaw check --offline`) reports these connector
findings today. Each line reads `<rule> <severity>: <file>: <key>: <message>
(fix: <hint>)`; the `(fix: …)` part is left out when there is no hint.

| Rule | Example | Cause | Fix |
|---|---|---|---|
| `CFG006` error | `CFG006 error: agents/main/agent.toml: connectors[0]: no connector named "tracker" is defined (fix: defined connectors: "weather")` | An agent's `connectors` names a connector that is not in `connectors.toml`: deleted, misspelled, or granted before it was created. The hint is `did you mean "…"?` for a near miss, `defined connectors: …` otherwise, or `no connector is defined` when there is none. | Remove or correct the id in `agent.toml`, or create the connector first. |
| `CFG010` error | `CFG010 error: agents/main/agent.toml: policy.tools.tracker: unknown tool or group "tracker"` | A `[policy.tools]` key names no connector or tool. A near miss gets a `did you mean "…"?` hint. | Remove or correct the key. |
| `CFG010` error | ``CFG010 error: agents/main/agent.toml: tools: unknown tool or group "tracker" (fix: "tracker" is a connector: grant it in `connectors`, not `tools`)`` | A connector id put in `tools`. | Move it to `connectors`. |
| `CFG011` error | `CFG011 error: agents/main/agent.toml: connectors[1]: connector "tracker" is listed twice (fix: list each connector once)` | A connector granted twice to one agent. | List it once. |
| `CFG011` error | ``CFG011 error: connectors.toml: connectors."Tracker": connector id "Tracker" is not valid (fix: use lower-case letters, digits and `-`, at most 32 characters)`` | An id in `connectors.toml` outside `[a-z0-9-]` or longer than 32 characters. | Rename the table in `connectors.toml` and every agent's grant. |
| `CFG011` error | `CFG011 error: connectors.toml: connectors.memory: connector id "memory" is a built-in tool or group name (fix: choose another id)` | An id in `connectors.toml` that is a built-in tool or group name. | Rename it, as above. |
| `BUD002` warning | `BUD002 warning: agents/main/agent.toml: connectors: connector tools are <n> tokens, over 1/4 of the <model> window (<window>) (fix: grant fewer connectors, or use a model with a larger window)` | The agent's connector tools take more than a quarter of its chat model's window. Counted from the tool lists the daemon cached. | Grant fewer connectors, or use a model with a larger window. |
| `CON001` error | ``CON001 error: connectors.toml: connectors.tracker.auth: connector "tracker" has auth = "oauth" but no grant in auth.toml (fix: run `holaclaw connectors reconnect tracker`)`` | An `auth = "oauth"` connector has no `access_token` in `auth.toml`: the sign-in was never finished, or its entry was removed. | Sign in again with `holaclaw connectors reconnect tracker`. |
| `CON002` warning | ``CON002 warning: connectors.toml: connectors.tracker: connector "tracker" is expired: its sign-in can no longer be refreshed (fix: run `holaclaw connectors reconnect tracker`)`` | The server refused the refresh token, so the daemon marked the connector `expired` (§8). | Sign in again with `holaclaw connectors reconnect tracker`. |
| `CON002` warning | ``CON002 warning: connectors.toml: connectors.tracker: connector "tracker" is degraded: ping_failed (fix: run `holaclaw connectors show tracker`; reconnect if it persists)`` | Three health checks in a row failed, so the daemon marked the connector `degraded` (§8). The word after the colon is the row's `detail`, left out when there is none. Calls still try the server. | Look at `holaclaw connectors show tracker`; if the server stays down or refuses, reconnect. |
| `CON003` error | `CON003 error: connectors.toml: connectors.tracker.url: "http://tracker.example.com/mcp" sends the connector's results and tokens in clear text (fix: use an https URL, or a loopback address)` | The connector URL is plain `http` on a host that is not loopback, whatever its `auth`, and the entry has no `allow_insecure_transport = true`. `create` refuses such a URL without `--allow-insecure-transport`, so it comes from a hand edit of `connectors.toml`. | Put an `https` URL in `connectors.toml`, or a loopback one such as `http://127.0.0.1:8080/mcp`, and restart the daemon. For an `oauth` connector, then run `holaclaw connectors reconnect tracker`: a token is bound to one URL. |
| `CON004` info | ``CON004 info: connectors.toml: connectors.tracker.trusted: connector "tracker" is trusted: its results never arm the taint (fix: confirm it, or set `trusted = false`)`` | The connector is `trusted` (§6). Reported on every run so the choice stays visible; an info finding never changes the exit code. | Nothing if you meant it; otherwise set `trusted = false` in `connectors.toml`. |
| `SEC004` warning | ``SEC004 warning: auth.toml: connectors.tracker: auth.toml has a secret for connector "tracker", which is not configured (fix: run `holaclaw auth rm connectors.tracker` if it is no longer needed)`` | `auth.toml` still holds a sign-in for a connector that is gone from `connectors.toml`, most often after the file was edited by hand. The file part is the path of `auth.toml`, relative to the config directory when it is inside it. | Remove it with `holaclaw auth rm connectors.tracker`. |

`CFG` errors are config errors: the daemon refuses to start with one, and so
does every CLI command that finds the daemon by reading the config
(`connectors`, `status`, …; `check` itself still runs), and `prompt-size`,
which reads the config offline. Such a command prints each problem on two
lines instead, with no `holaclaw: ` prefix:

```
CFG006 agents/main/agent.toml connectors[0]: no connector named "tracker" is defined
    fix: defined connectors: "weather"
```

Fix the file. If you need to reach a running daemon meanwhile with
`connectors` or `status`, pass its socket path with `--addr` (`server.socket`
of `config.toml`, or `/run/holaclaw/holaclaw.sock` when that is unset): the
command then does not read the config. `prompt-size` has no `--addr` and
works again only once the file is fixed.

If `check` or `prompt-size` cannot read the daemon's database, it prints
`connector cache not read: …` on stderr, counts no connector tools and, for
`check`, reports no `CON002`.

## 11. Troubleshooting

Every error the CLI prints starts with `holaclaw: `, and most then name the
connector (`tracker: …`). Config errors are the exception: they print as
`CFG… <file> <key>: <message>` with an indented `fix:` line (§10).

### The `detail` of a connector

| `detail` | Cause | Fix |
|---|---|---|
| `descriptions_truncated` | A tool description was over 400 tokens and is cut (§4). | Nothing to fix. |
| `intent_expired` | The sign-in was not finished within 15 minutes. | `reconnect`, and finish within the time it prints. |
| `state_mismatch` | A code came back from another sign-in, usually an address copied from an older attempt. The sign-in is dropped. | Start again; paste the address of the newest sign-in. |
| `authorization_failed` | The service refused the code exchange. | `reconnect`. |
| `client_metadata_refused` | The service's token endpoint refused holaclaw's client metadata document. holaclaw registers itself instead next time: after a failed `create`, only until the daemon restarts; after a failed `reconnect`, for good. | Run `create` or `reconnect` again. If the refusal came on the sign-in page instead (the CLI prints `the authorization server refused: invalid_client` or `unauthorized_client`), holaclaw never learns it and the next attempt uses the document again: use a pre-registered app with `--client-id` (§3.4). |
| `discovery_failed` | The server publishes no usable sign-in metadata. | `inspect` the URL; check it is the MCP endpoint. |
| `client_identity_failed` | No way to name holaclaw to the service: it needs a pre-registered app, or registration failed. | Create an app and pass `--client-id` (§3.4). |
| `auth_mismatch` | `--auth` disagrees with the server. | Drop `--auth`, or give the right value. |
| `server_unreachable` | The server did not answer the MCP handshake. | Check the URL and that the server is up; wait for the next check. |
| `ping_failed` | A health check's ping failed. | Usually passes on its own (§8). |
| `tools_list_failed` | Listing the tools failed. | Usually passes on its own (§8). |
| `tools_too_large` | The server's tool list is over holaclaw's size cap. | The server offers too much; nothing to do on the holaclaw side. |
| `refresh_failed` | Refreshing the token failed for a passing reason. | Usually passes; `reconnect` if it stays. |
| `refresh_rejected` | The service rejected the refresh: the sign-in is dead (`expired`). | `reconnect` (§7). |
| `unauthorized` | The server still answered "unauthorized" after a refresh. | `reconnect`. |
| `open_server_unauthorized` | A server connected as open now asks for a sign-in. | Delete it and create it again (§8). |
| `needs_reconnect` | An OAuth entry with no credentials, such as one written by hand. | `reconnect` (§7). |
| `url_changed` | The URL changed and the tokens belong to the old one. | `reconnect` (§7). |
| `config_write_failed` | `connectors.toml` or `auth.toml` could not be written. | Check the files and their directory are writable; not a symlink (§9). |
| `cache_write_failed` | The daemon's database could not be written. | Check the data directory (`holaclaw check`, `DAT001`). |

### CLI messages

| The CLI says | Cause | Fix |
|---|---|---|
| ``daemon is not running (start it with `holaclaw run`)`` | No daemon on the socket or address. | Start the daemon. |
| `no connector named "tracker" on this daemon` | No such id, a `create` whose sign-in expired (it is gone), or an entry added by hand after the daemon started. From `delete`, nothing was left to remove. | `list`; `create` again; restart after a hand edit. |
| `tracker: id in use` | The id exists, in `connectors.toml` or as a sign-in still open. | Pick another id, or `reconnect` / `delete` the existing one. |
| `<id>: invalid request: id must be [a-z0-9-], at most 32 characters` | Bad id. | Lowercase letters, digits and `-`. |
| `memory: invalid request: id "memory" is a built-in tool or group name` | The id is taken by holaclaw. | Pick another id. |
| `tracker: invalid request: url must be https, or http on a loopback host` (from `inspect`: `connectors: …`) | Plain `http` to another host. | Use the `https` URL. |
| `tracker: server does not use the requested auth` | `--auth` disagrees with the server. | Drop `--auth`. |
| `tracker: discovery failed: oauth: no client identity available` | The server needs a pre-registered app. | `--client-id` (§3.4). |
| `tracker: server unreachable: …` | The server did not answer. | Check the URL, the network, the server. |
| `tracker: not the address the browser landed on: it should end in /callback?code=…&state=…` | `code` got something else, or the address lost its query in the shell. | Copy the address again and put it in single quotes. |
| `tracker: the address has no code: copy all of it from the address bar` (or `no state`) | Part of the address is missing. | Copy the whole address. |
| `tracker: the authorization server refused: access_denied`, then ``try again with `holaclaw connectors reconnect tracker` `` | You declined, or the service refused, on its page. Nothing was sent to the daemon. | `reconnect`. |
| `tracker: no open authorization: …`, then ``try again with `holaclaw connectors reconnect tracker` `` | No sign-in is open. What follows the colon says why: `it expired` (15 minutes passed); `its code was already posted`; `it ended with <detail>` (it failed: the detail is one of the table above, such as `state_mismatch` or `authorization_failed`); `none was opened since the daemon started at <time>` (the daemon restarted after the `create` or `reconnect`, which closes open sign-ins, or none was ever started); `it is still being opened`. The daemon's log has a `connector authorization ended` line with the same reason. | `reconnect`, and run `code` right after signing in. For `it is still being opened`, wait and run `code` again. |
| `tracker: authorization in progress` | A sign-in is being opened, or its code is being exchanged. | Wait, then `list`. |
| `tracker: state mismatch` | The address belongs to another sign-in. The sign-in is dropped. | Start again with the newest URL. |
| `tracker: authorization failed: oauth: authorization server answered invalid_grant` | The code was used, expired or refused. | `reconnect`. |
| `tracker: authorization failed: oauth: authorization server answered invalid_client` | The service refused holaclaw as a client. If it refused the metadata document, the next attempt registers instead. | Run `create` or `reconnect` again; if it repeats, use a pre-registered app. |
| `tracker: authorization failed: oauth: authorization server issuer mismatch` (or `discovery failed: …`) | The redirect or the metadata names another authorization server than expected. | Start again. If it repeats, the server's metadata is inconsistent: ask its operator. |
| `tracker: invalid request: client_secret is required: the stored one belongs to another issuer or client` | A pre-registered app's issuer or client id changed. | `reconnect --client-secret-stdin` (§7). |
| `tracker: invalid request: client_secret needs client_id` | `create --client-secret-stdin` without `--client-id`. | Add `--client-id`. |
| `tracker: invalid request: client_secret needs a pre-registered client_id` | `reconnect --client-secret-stdin` on a connector with no `client_id` in `connectors.toml`. | Drop `--client-secret-stdin`, or add `client_id` to the entry by hand and restart. |
| `--client-secret-stdin: stdin is empty` | Nothing was piped in. | Pipe the secret (§3.4). |
| `tracker: degraded (server_unreachable)` | The sign-in worked, but the server failed right after. The credentials are kept. | Wait for the next check (§8). |
| `--listen: ports 26500 to 26504 on 127.0.0.1 are all in use; free one, or run without --listen` | Five other listeners. | Free a port, or use the default mode. |
| `the authorization expired before the browser came back; run the command again` | `--listen` waited 15 minutes. | Run the command again. |
| Browser: `This is not the sign-in holaclaw is waiting for.` | The redirect reached a `--listen` that waits for another sign-in. | Use the newest URL; or copy the address and use `code`. |
| Browser: `This is not a sign-in redirect.` | A page that is not a sign-in redirect hit the listener. | Ignore it. |
| `tracker: connector: connectors.toml is a symlink the daemon does not write: …` | The daemon never writes through a symlink. The rest of the message says what to do. | See §9. |
| `tracker: connector: connectors.toml does not parse; fix it by hand` | The file has a syntax error or an unknown key. Nothing was changed. | Fix the file (`holaclaw check --offline` points at it), then restart. |
| `tracker: connector: deleted, but removing its sign-in or cache failed; delete tracker again before creating it again` | A delete removed the entry, then failed. | `delete tracker` again (§9). |
| `tracker: internal error` | Something failed inside the daemon; its log says what. | Read the daemon's [log](../deployments/README.md#logs). |

## 12. Test it manually

Pick two servers you are allowed to use: an **open** one (no sign-in) and
one that needs an **OAuth** sign-in, with an account you don't mind
connecting. Below, `https://open.example.com/mcp` and
`https://mcp.example.com/mcp` stand for their URLs. Keep the daemon
running.

- [ ] **Inspect.** `holaclaw connectors inspect <open url>` shows `open: yes,
      no sign-in needed` and tools; `holaclaw connectors inspect <oauth url>`
      shows `open: no, it needs a sign-in` and a `client identity` (§2).
- [ ] **Open server.** `holaclaw connectors create weather <open url>` prints
      `weather: connected to …`.
- [ ] **OAuth, default mode.** `holaclaw connectors create tracker <oauth url>`
      prints a URL; sign in; the browser fails to load `127.0.0.1:26500`;
      `holaclaw connectors code tracker '<address>'` prints `tracker:
      connected to …, scopes …` (§3.2).
- [ ] **List and show.** `holaclaw connectors list` shows both `connected`;
      `holaclaw connectors show tracker` shows the tools with `read` /
      `external` and `trusted: no`; `holaclaw status` shows no connector
      table.
- [ ] **Grant.** Add `connectors = ["tracker", "weather"]` to
      `agents/main/agent.toml` (§5), `holaclaw check --offline` is clean,
      restart the daemon. `holaclaw prompt-size --agent main` shows a
      `connectors` row above 0.
- [ ] **A tool call.** In `holaclaw chat`, ask the agent to use one of the
      tools, for example "list my issues with tracker". The agent calls
      `tracker.<tool>` and answers with the result. A tool marked `asks`
      prompts for approval first. `holaclaw trace` shows the call.
- [ ] **Policy.** Add this table at the end of `agents/main/agent.toml`
      (after `[policy]`; if the file has a `[policy.tools]` table already,
      add the line to it instead), restart, ask again: this time the call
      waits for your approval.

      ```toml
      [policy.tools]
      "tracker" = "ask"
      ```
- [ ] **Reconnect.** `holaclaw connectors reconnect tracker --listen`; while
      it waits, `holaclaw connectors list` in another terminal shows
      `authorizing` and the agent still works; after the sign-in it prints
      `tracker: connected to …`.
- [ ] **Delete.** `holaclaw connectors delete tracker`, type `yes`: it prints
      `tracker: deleted`. `holaclaw check --offline` now reports `CFG006` (and
      `CFG010` if you kept the policy key). Remove `tracker` from
      `agent.toml`, check again, restart. Delete `weather` the same way.
