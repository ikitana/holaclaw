# Skills

What the agent can do. This document covers skills: what one is, where it
lives, when the model sees it, how the agent learns one, and how a person
installs, versions and removes them.

Memory, what the agent knows, has its own document, [memory.md](memory.md).

## Contents

1. [Concepts](#1-concepts)
2. [On disk](#2-on-disk)
3. [Eligibility](#3-eligibility)
4. [In the prompt](#4-in-the-prompt)
5. [Teaching the agent](#5-teaching-the-agent)
6. [Versions and trash](#6-versions-and-trash)
7. [Explorer, install, manage](#7-explorer-install-manage)
8. [Requirements and checks](#8-requirements-and-checks)
9. [Events](#9-events)
10. [Future additions](#10-future-additions)

## 1. Concepts

A skill is a procedure: an ordered way of doing one class of task, written in
markdown and loaded only when that task shows up. Memory holds facts and
preferences, which apply on every turn. A skill holds steps, which apply to
one kind of task.

**A skill grants nothing.** It can only tell the model to use tools the agent
already has and the policy allows. Whether a skill is offered at all is
decided by the daemon, never by the model (§3).

Skills belong to an agent, never to a person. What an owner teaches the agent
in one room, the agent does everywhere ([identity.md](identity.md) §7).

| Level | Lives in | Written by |
|---|---|---|
| Bundled | embedded in the holaclaw binary; none ship today | holaclaw |
| Agent | `agents/<name>/skills/<skill>/` | a person, by hand or with `holaclaw skills install`; the agent through `skill.save`, when an owner or admin taught it (§5) |

Both levels use the same folder format and one loader. An agent skill with a
bundled skill's name shadows it. A bundled skill is never written, forgotten
or removed.

The agent's skills folder is the one place under `agents/` the daemon writes.
It does so only when a person asks: `skill.save` and `skill.forget` in a
conversation, or install, revert, restore and remove through the CLI or the
API. Every write keeps the version it replaces (§6).

| Term | Meaning |
|---|---|
| **Index** | The list of eligible skills in the system prompt, one line each (§4). |
| **Eligible** | In the index for this run: requirements met, not denied for the sender, not disabled (§3). |
| **Learned skill** | An agent skill the agent saved, carrying who taught it and the trace that saved it. |
| **Provenance** | The line that opens a loaded body: bundled, agent, or learned from whom and when (§4). |
| **Version** | One state of an agent skill's folder. Earlier ones are kept in `history/` (§6). |
| **Trash** | `.trash/` under the skills root: forgotten skills, whole, until restored or removed (§6). |

## 2. On disk

The layout is the [AgentSkills](https://agentskills.io) one, so skills written
for other tools drop in:

```
agents/main/skills/release/
  SKILL.md              # the current version
  references/           # optional, loaded on demand by name (§4)
  scripts/              # optional, listed but never run
  history/3/            # earlier versions, the whole folder as it was (§6)
  .holaclaw/origin.toml # where an installed skill came from (§7)

agents/main/skills/.trash/old-release/   # a forgotten skill, whole
```

```yaml
---
name: release
description: "Cut a release of a Go service: tests, version bump, tag, announce."
license: MIT               # spec keys: license, compatibility, allowed-tools; read, never interpreted
metadata:
  holaclaw:                # everything of holaclaw's, in the spec's namespace slot
    version: 1             # format of this block; required when the block is present
    skill_version: 4       # which version of the skill this file is (§6)
    requires:
      tools: [files]       # agent tools it needs (§8)
    learned:               # present when the agent saved this version; never on a bundled skill
      on: 2026-09-12T14:03:05Z  # when it was saved, UTC; a bare date reads as midnight
      by: 01J9PX…          # the identity that taught it
      from: 01J9Q4…        # the trace that saved it
      origin: correction   # correction | explicit (§5)
---
```

- **Frontmatter** is the AgentSkills spec: `name`, `description`, and the
  optional `license`, `compatibility`, `allowed-tools` and `metadata`. Any
  other top-level key is an error. holaclaw reads only `metadata.holaclaw`,
  which is strict: an unknown key inside it is an error. Other namespaces
  under `metadata` are parsed and ignored, so a skill written for another
  tool loads, and one of holaclaw's is valid for them. `allowed-tools` is
  ignored: requirements come only from `metadata.holaclaw.requires`.
- **`metadata.holaclaw.version`** names the format of the block, `1` today.
  It is required once the block is present; a skill with no block loads as
  is. A format this holaclaw does not read is an error, never a guess.
- **Body**: markdown after the frontmatter, never preprocessed. A skill the
  agent saves is rendered as `# Title`, a fixed intent line, `## When`,
  `## Steps`, `## Pitfalls` and `## Verify` (§5). A hand-written body has no
  required shape.

| Limit | Value |
|---|---|
| `name` | 1–64 of `[a-z0-9-]`, equal to the folder name |
| `description` | one line, at most 1 024 characters; the trigger goes first, since the index keeps 160 (§4) |
| Body | at most 8 000 characters; detail goes in `references/` |
| `SKILL.md` | at most 64 KiB |
| One file in `references/` | at most 16 KiB, refused whole above that (§4) |
| `references/` and `scripts/` listed | 200 files each; hidden entries and symlinks skipped |

A folder that breaks any rule is not loaded, and `holaclaw check` names it
with its code (§8). Nothing about a skill is stored outside its folder: load
counts come from trace events (§9). The roots are walked once per run, with
no watcher, so a hand edit is seen by the next run. `agents/` is the owner's
to keep in git, and `holaclaw backup` includes it.

## 3. Eligibility

A skill is in the index for a run only when, checked in this order at prompt
build:

1. every `requires.tools` entry is in the agent's `tools`;
2. none of those tools is denied for the sender's role and run kind. A
   pending sender runs under the `guest` policy block
   ([identity.md](identity.md) §1), so it never sees a skill that needs a tool
   the guest block denies;
3. it requires no binary (§8);
4. it is not in the agent's `skills.disabled`.

```toml
# agents/main/agent.toml
[skills]
disabled = ["deploy"]    # kept out of this agent's index at every level
```

The set is decided once per run and cached, so the index the prompt shows and
the skills `skill.load` serves are the same. `holaclaw skills list` and the
explorer show whether each skill is eligible and, if not, why; `holaclaw
check` reports the reasons that hold for every sender (§8).

## 4. In the prompt

**Index.** A `skills` system fragment lists every eligible skill, bundled and
agent mixed with no marker:

```
Skills (procedures you can load):
- release: Cut a release of a Go service: tests, version bump, tag, announce.
- pr-review: Review a pull request against the team's checklist.
If one clearly matches the task, load it with skill.load before acting. At most one up front. If none matches, load none.
```

A description over 160 characters is cut there and marked with `…`, so a long
one doesn't grow every turn's prompt. The fragment may take at most an eighth
of the model's context window. It changes only when the skill set does, so it
stays in the cached prefix.

**Body.** `skill.load(name, file?)` is a `read`-class tool. Without `file`, it
returns the body, opened by its provenance line:

```
[skill: release · agent skill]
[skill: triage · bundled skill]
[skill: pr-style · learned from Angel on 2026-09-12]
[skill: pr-style · learned on 2026-09-12]       # the teacher no longer resolves
```

The model reads a rule learned last week more critically than doctrine. The
index never says which kind a skill is, so loading is decided on the
description alone.

`file` reads one file under `references/`, with or without that prefix, under
its own header line (`[skill: release · references/checklist.md]`). A file
over 16 KiB, outside `references/`, or not text is refused, never truncated.

- `skill.load` is hidden from a run with no eligible skill.
- The result arrives as a tool result, so the cached prefix survives. Loading
  a skill never changes the tool set.
- When compaction summarises a turn that loaded a skill, the summary ends
  with a note to load it again if it is still needed, rather than work from a
  paraphrase.

## 5. Teaching the agent

The content decides where something goes, not how it was learned:

| | Memory | Skill |
|---|---|---|
| Shape | a fact or a standing preference | an ordered procedure, two steps or more |
| Applies | every turn, by search | one class of task, by name |
| Correction | "stop being so verbose" | "when you deploy, run the tests first" |

The core prompt carries one line for it:

> Facts and preferences go to memory.save, fixed procedures to skill.save last; load the skill first and ask if the fix is for this task only. No skill.save: suggest it to an owner or admin.

### Saving

The model never writes markdown. It calls `skill.save` with a structured
draft, and the daemon renders `SKILL.md`, checks it and versions it:

| Argument | Rule |
|---|---|
| `name` | 1–64 of `[a-z0-9-]` |
| `title` | one line, at most 128 characters; the `# ` heading |
| `when` | one line, at most 1 024 characters, the trigger first; becomes the `description` and the `## When` section |
| `steps` | 2 to 24, each one line of at most 300 characters |
| `pitfalls` | optional, at most 6, each one line of at most 300 characters |
| `origin` | `correction` (the person said how it should have been done) or `explicit` (the person asked to keep it) |

The rendered file must also fit the 8 000-character body limit. A draft that
breaks any rule is refused with every problem listed, so the model fixes them
in its next call. The checks cover shape and length only. Wording is the
model's job and a person's to review.

The daemon fills in the rest: `learned.by` is the sender, `learned.from` the
trace, `learned.on` the time of the call. The model supplies none of them.

**Last call of the turn.** A procedure is right only once the task ended
without a correction. `skill.save` followed by other calls in the same
response is refused. After a successful save, the tools are withdrawn and the
model answers.

**Who.** A skill changes the agent for everyone it serves, so only an owner or
admin teaches one. `skill.save` and `skill.forget` check the sender's role on
each call, in a DM or a room. A turn that coalesced messages from several
senders runs under the strictest role among them, so a user's message can't
ride on an admin's. A task run never writes skills. For anyone else, the two
tools are not offered at all.

**Replacing.** `skill.save` with an existing name makes a new version of that
skill (§6), whether it was installed, written by hand or saved before. Its
`requires` carry over unchanged (§8). A skill that can't be read, or whose
folder is a symlink, is not replaced: a person fixes it first. Saving under a
bundled skill's name creates an agent skill that shadows it.

### Forgetting

`skill.forget(name)` moves the agent skill, whole, to the trash (§6). Only a
person deletes it for good, from the CLI or the explorer (§7). A bundled skill
can't be forgotten; disable it with `skills.disabled` instead (§3).

## 6. Versions and trash

Every write the daemon makes to an agent skill is versioned in place. A
version is the whole folder: `SKILL.md`, `references/`, `scripts/` and any
other file, since a body and the references it points at change together.
Hidden entries, such as `.holaclaw/`, are not part of it.

- **Numbering.** `history/<N>/` is version `N`. The current folder is the
  version after the newest in `history/`, or 1 when there is none. This number
  is the version everywhere: the API, `holaclaw skills show`, the explorer
  and `revert --to`.
- **Writing.** The current folder is copied into `history/<N>/` (built under
  a hidden name and renamed, so `history/` never holds half a version). The
  new `SKILL.md` lands by temporary file and rename as version `N+1`, and the
  rest of the folder carries over.
- **Limit.** `history/` keeps the last 256 versions, dropping the oldest. Each
  version is a full copy, so the cap bounds the disk one skill can take.
- **Hand edits.** A hand edit keeps the current number and is the one
  unversioned write, until the next daemon write retires it into `history/`.
- **Revert** copies an earlier version into a new one, retiring the current
  folder the same way. The earlier version stays in `history/`.

`metadata.holaclaw.skill_version` is something else: it records which version
of the skill the file's content is. A skill the agent saves carries the
number it was saved as. A reverted copy keeps the `skill_version` of the
version it restores, while it gets the next history number.

**Trash.** Forgetting moves the whole folder, history included, to
`.trash/<name>/`, which nothing indexes. Restore brings it back, and is
refused while a live skill has that name. Remove deletes it for good. A second
forget of the same name replaces the earlier trashed copy.

Nothing edits, archives or removes a skill on its own.

## 7. Explorer, install, manage

`holaclaw skills` with no subcommand opens the explorer, a terminal screen
over the agents' skills. It shows each skill with its index line,
eligibility, versions and who taught each version, and it can revert, remove
and restore. It connects to the daemon with `--addr` and `--token`, as the
other screens do.

The subcommands do the same for scripts:

```
holaclaw skills list [agent]
holaclaw skills show <name> [--agent a]
holaclaw skills install <path|git-url> [--git-ref r] [--git-path p] [--agent a] [--as name] [--all] [--force] [--yes]
holaclaw skills remove <agent> <name>            # live: to the trash; trashed: for good
holaclaw skills restore <agent> <name>
holaclaw skills revert <agent> <name> --to <version>
```

`--agent` defaults to `main`.

| Endpoint | Scope |
|---|---|
| `GET /v1/skills?agent=`, `GET /v1/skills/{id}?agent=[&version=]` | client |
| `POST /v1/skills` (install or preview), `POST /v1/skills/upload` | admin |
| `POST /v1/skills/{id}/revert`, `POST /v1/skills/{id}/restore`, `DELETE /v1/skills/{id}` | admin |

A token bound to an identity sees only the agents that identity may use;
any other agent is a `404`. A write aimed at a bundled skill is a `403`. None
of these is reachable by the model.

### Installing

Sources are a local folder or a git repository. There is no hub or registry
and no auto-update.

- **Local.** The argument is a folder, or its `SKILL.md`. The CLI reads it,
  not the daemon, which may be on another host and never reads a path from
  the API. The CLI checks the folder is a skill or a collection, packs it as
  a gzipped tar (`.git` and `.holaclaw` left out, symlinks refused) and posts
  it to `POST /v1/skills/upload`. An upload waits an hour for its install.
- **Git.** The argument is a URL: `https`, `ssh`, `git` or `file`, or an
  scp-like `user@host:path`. `--git-ref` names a tag, branch or commit, and
  the repository's default branch is used without one. `--git-path` names the
  folder in the repository. The daemon fetches that one ref shallowly, within
  two minutes, with terminal prompts, hooks, fsmonitor and LFS filters off.

A folder without `SKILL.md` whose subfolders, or whose `skills/` subfolders,
are skills is a **collection**. `install` lists its skills with the command
that installs one, and `--all` installs every one. Limits per install: 256
files and 128 MiB in total.

Install goes in this order:

1. Fetch or upload, and stage the folder under `<config>/tmp/`, on the same
   filesystem as the skills root. `--as` renames it.
2. Scan the files and parse the skill.
3. Show the preview: body, `requires`, scan findings. Ask, unless `--yes`.
4. Rename the folder into place, recording `.holaclaw/origin.toml`: the
   source (the URL, or the folder the CLI read), ref, path and commit for git,
   the hash and the time.

Install never overwrites: a live skill of the same name is a `409`. If the
source changed between the preview and the install, that is also a `409`.
`holaclaw check` warns when an installed skill's files no longer match its
hash (SKL006).

The **install scan** looks for the obvious and refuses without `--force`:

| Code | Finding |
|---|---|
| `SCN001` | a pipe into a shell or an interpreter (`curl … \| sh`) |
| `SCN002` | ssh material (`~/.ssh`, `id_rsa`, `authorized_keys`) |
| `SCN003` | hidden or bidirectional Unicode |

The scan is a hint for the person reading the skill, not a boundary. The
boundary is eligibility (§3) and the policy on every tool call.

## 8. Requirements and checks

`metadata.holaclaw.requires` is what a skill needs, never what it grants:

```yaml
requires:
  tools: [exec, files]           # agent tools; eligibility rules 1 and 2 (§3)
  bins: [gh, {name: rg, pkg: ripgrep}]   # binaries, by name, never a path
  net: [gh]                      # which of those bins may open the network
```

- `tools` names tools or tool groups from the agent's `tools`.
- `bins` and `net` are parsed and validated, but holaclaw doesn't run skill
  binaries yet. A skill that requires any binary is never eligible (§10).
- `skill.save` declares nothing. A new skill the agent saves has no
  `requires`. A new version keeps the block of the version it replaces
  unchanged. Only install and a hand edit can widen it.

`holaclaw check` loads every agent's skills as the daemon does and reports:

| Check | Level | When |
|---|---|---|
| `SKL1xx` | warn | a skill folder that does not parse, with its own code (hidden) |
| `SKL001` | warn | a skill requires a tool the agent does not list (hidden) |
| `SKL002` | warn | a skill requires a binary (hidden) |
| `SKL006` | warn | an installed skill's files differ from the hash recorded at install |

A skill hidden only for some senders (rule 2 of §3) is not a check finding.
`holaclaw skills list` and the explorer show it.

## 9. Events

| Event | Carries |
|---|---|
| `skill.loaded` | agent, name, level, file (for a reference), body tokens, provenance |
| `skill.saved` | agent, name, by, origin, version, the version it replaced, versions in history, body tokens |
| `skill.forgotten` | agent, name, versions in history; who, when a person did it |
| `skill.restored` | agent, name, who |
| `skill.reverted` | agent, name, from, to, the new version, who |
| `skill.installed` | agent, name, source, ref, path, commit, hash, number of scan findings, who |

`skill.loaded` is part of the turn's trace and the body is the tool result,
so `holaclaw trace` shows what the model read. A version the agent saved names
its trace, which the explorer shows, to open with `holaclaw trace <id>`.

## 10. Future additions

Not built yet. The entry says what it needs and what in this design it relies
on.

### Binaries

A skill that runs a CLI (`gh`, `rg`) declares it in `requires.bins`, and the
declaration, accepted by a person at install, becomes the allow-list for
`exec`: no policy to write by hand, and nothing a skill the agent saves can
widen.

- **Where a binary comes from.** Built from a nixpkgs revision the deployer
  pins in `config.toml`, or an absolute path the deployer names and the daemon
  hashes. Never looked up on the PATH by name.
- **Where it runs.** A sandbox sidecar, its own process and user, restricting
  each call with Landlock and, where user namespaces work, bwrap: the
  binary's files and the workspace readable, the network only when `net`
  declares it.
- **Its credentials.** One home per binary per agent, so a token one CLI
  keeps is never visible to another.
- **Checks.** A binary that failed to build, a sidecar that isn't available,
  and a binary that no longer matches what was accepted each hide the skill
  and get a `check` rule.

It relies on: the `requires.bins` and `net` fields already parsed, eligibility
decided by the daemon (rule 3 of §3 is where an accepted binary gets checked),
`requires` never widened by `skill.save`, and the `exec` policy class.
