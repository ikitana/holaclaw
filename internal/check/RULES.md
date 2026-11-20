# `holaclaw check` rules

Stable ids, never reused or repurposed (ARCHITECTURE.md §7). `CFGxxx` codes are
defined and documented in `internal/config/problem.go`; every one of them
passes straight through as a finding with the same code. `SKL1xx` codes are
the second pass-through: defined in `internal/skill/skill.go`, each one a
warning for a skill folder that does not parse.

| ID | Severity | Checks | Fix |
|---|---|---|---|
| CFG001..012 | error/warning | Every problem `config.Load` / `config.Validate` found (unknown key, wrong file, bad type, bad enum, missing required key, dangling reference, bad/mismatched version, literal secret, unknown tool, malformed value, missing file) | Whatever `config.Problem.Hint` says |
| SEC001 | error | `auth.toml` is group/world accessible or not a regular file | `chmod 600 <path>` |
| SEC002 | error | A provider with `auth = "api-key"` has no resolvable `api_key` (`env:VAR` set, or an `auth.toml` entry) | `holaclaw auth set providers.<name> api_key <value>` |
| SEC003 | error | A channel instance has no bot credential in `auth.toml` (`bot_token` for telegram; `bot_token` + `app_token` for slack; `service_account` for googlechat) | `holaclaw auth set channels.<name> <key> <value>` |
| SEC004 | warning | `auth.toml` has an entry for a provider/channel/connector no longer in the config | `holaclaw auth rm providers.<name>` / `channels.<name>` / `connectors.<id>` |
| SEC005 | error | The config directory or a file under it is world-writable | `chmod o-w <path>` |
| SEC006 | warning | A `whatsapp` channel instance is not linked: its session database `<data_dir>/whatsapp/<name>.db` is missing or holds no device (the counterpart of SEC003: WhatsApp keeps nothing in `auth.toml`) | `holaclaw login whatsapp [--name <name>] --phone <number>` |
| SEC007 | warning | A provider without `auth = "api-key"` has an `api_key` (in `auth.toml`, or an `env:VAR` in the config): it is never sent | set `auth = "api-key"`, or `holaclaw auth rm providers.<name> api_key` |
| NET001 | error | `server.listen` is non-loopback with no `tls_cert`/`tls_key`, and `allow_insecure_transport` is not set | set both, bind loopback, or set `allow_insecure_transport = true` |
| NET002 | error | `tls_cert`/`tls_key` is set but the file is missing or unreadable | point it at a readable file |
| NET003 | error | an authenticated provider (`base_url`, or a model `url`) is reached over plain `http` on a non-loopback host | use https, or a loopback address |
| ACC001 | error | A channel has `dm = "open"` or `rooms = "open"` while its agent's guest policy allows `exec`, `write` or `external` (images and fetches cost money and reach out) | deny all three in `policy.guest`, or close access |
| ACC002 | warning | `reply = "always"` with `rooms = "open"` | set `reply = "mention"` |
| ACC003 | warning | A `googlechat` channel has `reply = "always"`: Chat only delivers @-mentions in spaces, so it can never fire in a room | set `reply = "mention"` |
| POL001 | warning | Agent policy has `exec = "allow"` | use `"ask"` plus `policy.exec_allow` |
| POL002 | info | Agent lists the `exec` tool while `policy.exec = "deny"` | remove `"exec"` from `tools` |
| MOD001 | warning | `model.context` is under 4096 | raise it, or use a bigger model |
| MOD002 | error/info | Configured `context` exceeds what the runtime (Ollama/llama.cpp) actually allocated; needs `Input.Probe` | raise `num_ctx`/`n_ctx`, or lower `models.<name>.context` |
| MOD003 | error/warning/info | The provider does not serve `models.<name>.id`: it is not in the list an OpenAI-compatible endpoint answers at `<base_url>/models`, or in the one a signed-in codex subscription may use (a warning when that is the pinned list, not the account's own). A model with its own `url` is not checked; info when the list cannot be read; needs `Input.Models` | set `models.<name>.id` to an id the finding lists |
| BUD001 | warning/error | Agent's fixed prompt overhead (system + tool schemas) is over 1500 tokens, or over 1/4 of the chat model's window; also reports a `BudgetError` from the builder. Connector schemas are left out | drop tools or shorten the persona |
| BUD002 | warning | Agent's connector tools (the `connectors` fragment plus every connector schema, from the cached tool lists) are over 1/4 of the chat model's window | grant fewer connectors, or use a model with a larger window |
| DAT001 | error | The data directory cannot be created or is not writable | make it writable, or set `server.data_dir` |
| DAT002 | warning | The database file's mode allows group/other access | `chmod 600 <path>/holaclaw.db` |
| BRW001 | error | `[browser]` is configured but no usable browser: `browser.binary` missing or not executable, nothing on PATH, or a major below the minimum | install chromium, or set `browser.binary` |
| BRW002 | error | `browser.headless = false` (the default) while `Xvfb` is not on PATH | install xvfb, or set `browser.headless = true` |
| BRW003 | error | Chromium's sandbox cannot start here: `kernel.unprivileged_userns_clone = 0`, `user.max_user_namespaces = 0`, or AppArmor's `apparmor_restrict_unprivileged_userns = 1` | the `sysctl` in the finding, or an AppArmor profile for the binary |
| BRW004 | warning | Nothing is listening on `browser.socket`; skipped with `--offline` | `systemctl start holaclaw-browser` |
| BRW005 | warning | An agent lists `browser.do` with no `browser.allow`: it may open any site | set `browser.allow` in `agent.toml`, or accept it |
| BRW006 | warning | An agent lists `browser.do` with no `models.decide`: the chat model decides every step, a model turn per click | set `models.decide` to a decision model in `agent.toml` |
| CON001 | error | An `auth = "oauth"` connector has no grant (no `access_token`) under `connectors.<id>` in `auth.toml` | `holaclaw connectors reconnect <id>` |
| CON002 | warning | A configured connector's cache row in the store says `expired` (the refresh token is dead) or `degraded` (checks keep failing; the message carries the row's `detail`) | `holaclaw connectors reconnect <id>`; for `degraded`, `holaclaw connectors show <id>` first |
| CON003 | error | A connector `url` is plain `http` on a non-loopback host, without `allow_insecure_transport`: its results, and its token, cross the network in clear text | use https, or a loopback address |
| CON004 | info | A connector is `trusted`: its results never arm the taint | confirm, or set `trusted = false` |
| SKL101..116 | warning | A skill folder in any of an agent's roots that `skill.Load` cannot parse (no frontmatter, bad YAML, unknown key, bad name, too long, …): hidden | fix the `SKILL.md` the message names |
| SKL001 | warning | A skill (not in `skills.disabled`) requires a tool the agent does not list: hidden | add the tool to `tools` in `agent.toml`, or add the skill to `skills.disabled` |
| SKL002 | warning | A skill (not in `skills.disabled`) requires a binary: binaries are not supported yet, hidden | remove the skill, or add it to `skills.disabled` |
| SKL003 | error | Reserved (exec sidecar, not implemented): exec is `ask`/`allow` and the sidecar or Landlock is unavailable | — |
| SKL004 | warning | Reserved (exec sidecar, not implemented): a binary without `net` is in use and bwrap is absent | — |
| SKL005 | warning | Reserved (deferred, not implemented): an agent skill not loaded in 30 days | — |
| SKL006 | warning | An installed agent skill whose files no longer hash to its `.holaclaw/origin.toml`, or whose origin file or files cannot be read | `rm -r <skill dir> && holaclaw skills install <source> --agent <agent>` |
| SKL007 | warning | Reserved (exec sidecar, not implemented): a binary no longer matches its record | — |

`--only` / `--skip` match a finding id by exact value or prefix (`SEC` selects
every `SEC00x` id). Exit code: 2 if any error, 1 if the worst is a warning, 0
otherwise (info-only counts as 0).
