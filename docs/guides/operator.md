# Operator manual

Running HolaClaw day to day: the daemon, config changes, upgrades, people
and devices, backups, and what to do when something fails. It is for
whoever runs the daemon.

This manual starts with HolaClaw initialised and running: see the
[deployments](../deployments/README.md). Commands are written as
`holaclaw …`; run them
[the way your deployment does](../deployments/README.md#run-a-command).
Where an answer depends on the deployment, such as a path or how to restart,
the link goes to the [table](../deployments/README.md#3-where-things-are)
that gives it for each one.

## Contents

1. [Day to day](#1-day-to-day)
2. [Config changes](#2-config-changes)
3. [Upgrades](#3-upgrades)
4. [People and devices](#4-people-and-devices)
5. [Backups](#5-backups)
6. [Common issues](#6-common-issues)

## 1. Day to day

| Task | How |
|---|---|
| Is it up | `holaclaw status`: version, uptime, active sessions, channels, pending approvals |
| Follow the logs | [per deployment](../deployments/README.md#logs) |
| Apply a config change | `holaclaw check --offline`, then [restart](../deployments/README.md#restart) |
| See what the agent did | `holaclaw trace`; `holaclaw trace <id> --json` for one run |
| Answer an approval | `holaclaw approvals`, then `holaclaw approve <id>` or `holaclaw deny <id>` |

The daemon reads its config only at start, so restart it after every change.
A restart is safe: a message interrupted by a stop is queued again and
replayed when the daemon starts.

The daemon logs to stderr, at the info level. `holaclaw run --debug` logs
more; there is no other log setting. When a turn fails, the daemon logs the
provider and its error.

## 2. Config changes

The files are in the
[config directory](../deployments/README.md#config-directory):
`config.toml`, `channels.toml`, `connectors.toml` and one directory per
agent under `agents/`. Secrets are in `auth.toml`, in the
[secrets directory](../deployments/README.md#secrets-directory), and only
`holaclaw auth set`, `holaclaw login` or the API write it
([security.md §8](../security.md#8-configuration-and-secrets)).

**`holaclaw check`** validates the config before you restart. It exits with
`0` when clean, `1` on warnings and `2` on errors. `--offline` skips the
calls to the model endpoints, and `--only` and `--skip` take rule ids or
prefixes. Each finding names a rule:
[reference/check-rules.md](../reference/check-rules.md).

| Command | |
|---|---|
| `holaclaw config env` | the keys an environment variable may set, their value, and where it comes from |
| `holaclaw config schema <file>` | the JSON Schema of `config.toml`, `channels.toml`, `connectors.toml` or `agent.toml`, for an editor |
| `holaclaw config migrate` | after an upgrade: prints the diff to the current format; `--write` applies it after taking a timestamped backup |

Every key is in [reference/config.md](../reference/config.md).

## 3. Upgrades

Install the new release [the way your deployment does](../deployments/README.md#upgrade),
then run `holaclaw config migrate`. It says whether the config format
changed; `--write` applies the diff. The database migrates by itself at
start.

## 4. People and devices

Everyone who talks to the agent is an **identity** with a role (`owner`,
`admin` or `user`) and one **binding** per account on a channel. You add a
person by creating their identity, then giving them a one-shot **invite**
code for each channel they use. The full rules are in
[identity.md](../identity.md).

**A person.** Create the identity and its first invite in one step, then
send the person the code or the link it prints:

```
holaclaw identity create --name Carla --role user --invite telegram
```

For another channel, mint another invite with the id from
`holaclaw identity list`:

```
holaclaw invite create --identity <id> --channel slack
```

The person sends the code to the bot, as `/start CODE` or by itself, and
from then on that account talks as Carla. A code works on one channel
instance, once, for 15 minutes by default (`--ttl`, at most 1 hour, or 24
hours for an identity with no bindings yet).

**A device.** The apps sign in with a `holaclaw` invite, which pairs the
device and gives it its own token, `client` scope unless `--scope admin`:

```
holaclaw invite create --identity <id> --channel holaclaw
```

It prints a `holaclaw://pair?…` link and a QR code when the daemon knows the
address devices reach it at (`server.public_url`, else a `server.listen` that
names a host other devices reach, not loopback or a wildcard); otherwise the person enters the code and the server address in
the app. A device already signed in can add another one itself.

**A room.** A group chat is answered only once it is approved, under the
channel's `rooms` setting. Approve it with a room invite, redeemed in the
room:

```
holaclaw invite create --room --channel telegram
```

**Strangers.** By default an unknown sender is dropped unless they send an
invite code. A channel's `dm = "open"` answers them instead, as pending
identities under the `guest` policy, until an admin confirms them or assigns
them to an identity ([identity.md §2](../identity.md#2-lifecycle)).

**Taking access away.** `holaclaw identity disable <id>` stops answering an
identity, its tasks and its tokens, and keeps everything it holds;
`identity enable` brings it back. `holaclaw identity unbind` removes one
account, and `holaclaw identity delete` removes the identity.

## 5. Backups

A running daemon takes and keeps its own backups, under `backups/` in the
[data directory](../deployments/README.md#data-directory). It takes one every
`backups.every` (default `24h`) and keeps the newest `backups.keep` (default
7; `0` turns them off). It never prunes the backups you asked for or
uploaded.

| Task | Command |
|---|---|
| Take one now | `holaclaw backup create [--wait]` |
| List, download, upload | `holaclaw backup list`, `backup download <id>`, `backup upload <file>` |
| Verify | `holaclaw backup verify <id\|file>` |
| Delete | `holaclaw backup rm <id>` |
| Without a daemon, for a stopped or broken installation | `holaclaw backup [--out path]` |

The same operations are in the API, under `/v1/backups` (`admin` scope).

An archive holds the database, the blobs and the config. It includes the
secrets only with `--include-secrets`, and then whoever has the archive holds
every credential. In a container, copy the archive out of the data volume
([container.md](../deployments/container.md#backups)).

### Restore

`holaclaw backup restore <id>` replaces the database and the blobs with a
backup's and restarts the daemon on them. Everything stored since then is
gone, API tokens included. The config files and `auth.toml` are not touched.

### A new host from a backup

With no daemon running, `holaclaw backup restore <file>` recreates the whole
installation from an archive: the config files, `auth.toml` if the archive
holds it, the database and the blobs. `holaclaw init` is not needed: the owner
and the API tokens come with the database.

- The config files go into `/etc/holaclaw`, or into the directory that
  `--config` or `$HOLACLAW_CONFIG` names.
- The data and the secrets go where the archive's own `config.toml` says.
- It lists every directory and file it would create or replace, and asks
  before writing (`--yes` skips the question).
- It deletes nothing. A replaced config file is kept in
  `<config dir>/.backup-<timestamp>/`. A replaced `auth.toml` or database is
  kept beside the new one, as `.pre-restore`.
- It refuses to run while the daemon of that config directory is running.

The container's step-by-step restore onto empty volumes is in
[container.md](../deployments/container.md#backups).

### How backups work

- **The archive** is a gzipped tar of a consistent snapshot of the database,
  the blob store and the config files, with a `MANIFEST.json` of sizes and
  hashes that `backup verify` checks. The running daemon takes the snapshot
  through its own database handle (`VACUUM INTO`), so writes can go on
  meanwhile. `holaclaw backup` without a daemon refuses while a running daemon
  has data in the write-ahead log.
- **The records.** Each managed backup is `<id>.tar.gz` plus an `<id>.json`
  record: its origin (`manual`, `automatic` or `upload`), when it was taken,
  its size, the HolaClaw version, whether it holds secrets, and its status
  (`running`, `ready` or `failed`). The records are files, not database rows,
  so a restore doesn't replace the list of backups, and the `backups/`
  directory is never part of a backup.
- **One at a time.** A backup is written under a temporary name and renamed
  when it is complete, so a `ready` backup is always whole. One cut short by a
  restart is marked `failed`, and its partial file removed. An upload is
  checked against its manifest before it is listed.
- **A restore is two steps**, since the daemon can't swap the database it has
  open. The running daemon checks the archive (the manifest's hashes, and a
  database schema no newer than it knows), unpacks its data into `restore/` in
  the data directory, and restarts itself. The next start, before the
  database opens, adds the blobs (content-addressed, so nothing is
  overwritten), moves the old database aside as `holaclaw.db.pre-restore` and
  puts the restored one in. Each step can run again, so a crash halfway is
  finished by the next start, and a restore that can't be finished stops the
  start rather than open a half-replaced database. The restored database gets
  a `backup.restored` event.

## 6. Common issues

| Symptom | Cause | Fix |
|---|---|---|
| `CFG012 config.toml : missing or unreadable file` | the command ran as a user who cannot read the config directory, such as your own user on a system install | run it [as your deployment does](../deployments/README.md#run-a-command) |
| `holaclaw: daemon is not running (start it with `holaclaw run`)` | the daemon is stopped or failed, or still starting: it answers a few seconds after it starts | read the [logs](../deployments/README.md#logs) |
| `SEC002 … provider "openrouter" has auth = "api-key" but no api_key is set` | the key is not stored yet, as when `init` ran with no terminal | `holaclaw auth set` ([getting-started.md §3](../getting-started.md#4-change-the-model-or-the-provider)) |
| a turn fails with `provider codex: bad_request: … model is not supported when using Codex with a ChatGPT account` | the model id is retired or not on your plan | `holaclaw check` reports it as `MOD003`, the daemon logs a warning at start and every 24 hours, and `holaclaw models <provider>` lists the ids your account can use; or take a current id from OpenAI's models page ([deployments §2](../deployments/README.md#2-choose-a-provider)), then [restart](../deployments/README.md#restart) |
| `holaclaw login codex` says `no codex provider named codex on this daemon; pass --name <provider>` | `config.toml` has no `type = "codex"` provider of that name, or the daemon started before you added it | add it ([getting-started.md §3](../getting-started.md#a-chatgpt-subscription)) and restart; for a provider with another name, run `holaclaw login codex --name <provider>` |
| a config change has no effect | the daemon reads the config only at start | [restart](../deployments/README.md#restart) |
| Ollama does not answer | Ollama is not running, the model is not pulled, or, from a container, Ollama listens on loopback only | `holaclaw models <provider>` lists the models it serves |

`holaclaw trace` shows the whole run of a failed turn, and `holaclaw check`
explains a rejected config by rule id.
