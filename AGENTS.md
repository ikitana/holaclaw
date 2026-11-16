# Working on HolaClaw

Read [ARCHITECTURE.md](ARCHITECTURE.md) first: it maps the project and links
the doc for each part. The docs under `docs/` describe the code as it is.

## Building and testing

- Pure Go, no CGO. Run everything through the dev shell:
  `nix develop -c <cmd>` (ignore the "Git tree is dirty" warning).
- `nix develop -c just check` builds, vets, lints and tests everything.
  `just test-short` skips the slow tests (real Chromium, docgen, benchmarks).
- Tests are mandatory, failure paths included. Use `httptest` servers and
  fixtures: **no real network calls, and never read or print `.env`**.
- After changing the config schema, the CLI, the API spec or the event types,
  regenerate the reference docs with `just docs` (and the API code with
  `just gen`); `just docs-check` and `just gen-check` fail on stale output.

## Models and data

- No private data goes to any model during development, free models above
  all: only synthetic scenarios and fixtures. Never HolaClaw or customer code,
  real conversations, names, credentials or repository contents.
- Prefer free models for checks; report any paid spend.

## Changing the code

- Minimal: no config keys, tools, endpoints or features beyond what the task
  needs. Every addition justifies its prompt tokens and config keys.
- Match the comment density, naming and idiom of the surrounding code.
- Delete code a change makes obsolete, and update the comments and docs it
  makes stale. Docs describe current behaviour, never planned work.
- New dependencies need a reason; prefer the standard library.
- The contracts are shared by many packages: `internal/event`,
  `internal/provider/{provider,decide,image}.go`,
  `internal/tool/{tool,policy}.go`, `internal/store/store.go` and its
  migrations, `internal/memory/memory.go`, `internal/channel/channel.go`,
  `internal/agent/contract.go` and `internal/tokens`. Change them
  deliberately, and keep the stability contract
  ([ARCHITECTURE.md §7](ARCHITECTURE.md#7-stability-contract)).
- Do not commit, push or rewrite history unless you are asked to.
- Write commit messages in the [Conventional Commits](https://www.conventionalcommits.org/)
  format (`feat: ...`, `fix(scope): ...`). Do not add AI attribution
  (`Co-Authored-By: Claude ...` or similar trailers) to commits.
