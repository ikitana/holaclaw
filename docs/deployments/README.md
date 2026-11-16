# Deployments

The ways to run HolaClaw, and where each one keeps things. Every other guide
starts where these end: HolaClaw initialised, the daemon running, and a
`holaclaw` command you can run. The guides write commands as `holaclaw …`;
[run them](#run-a-command) the way your deployment does.

## Contents

1. [Choose a deployment](#1-choose-a-deployment)
2. [Choose a provider](#2-choose-a-provider)
3. [Where things are](#3-where-things-are)

## 1. Choose a deployment

| Deployment | Runs as | What `exec` reaches | Guide |
|---|---|---|---|
| **Container** (`ghcr.io/ikitana/holaclaw`) | the image's own users | the container only | [container.md](container.md) |
| **Linux system service** | the `holaclaw` system user, under a hardened systemd unit | the host, read-only outside its own directories, `/home` hidden | [linux.md](linux.md) |
| **As your own user** | you, as a user service or in a terminal | your home | [linux.md §9](linux.md#9-as-your-own-user) |
| **NixOS** | the module's hardened units, on the host, in a container or in a VM | the host, or the container or VM | [nix.md](nix.md) |

The container is the recommended one when the machine does anything else:
the agent, its tools and `exec` see only the container. The installer script
of the [README](../../README.md#quickstart) sets up any of the first three
for you, and each guide gives every command it runs.

## 2. Choose a provider

`holaclaw init` writes a config for one provider and one model, points the
agent at that model, stores the provider's API key, and creates the owner.
Each guide shows the `init` command for its deployment; the provider flags
are the same everywhere:

| Provider | Flags | Asks for |
|---|---|---|
| OpenRouter | `--provider openrouter` | the API key; the model, `deepseek/deepseek-v4.1-flash` by default |
| OpenAI | `--provider openai` | the API key; the model, `gpt-6.1-sol` by default |
| A ChatGPT subscription | `--provider openai-codex` | nothing: you sign in once the daemon runs |
| Any OpenAI-compatible endpoint | `--provider openai-compat --base-url <url> --model <id>` | the API key, if the endpoint needs one |

`--model` sets the model without the question, for every provider. With
`--token-file`, `init` also writes an admin token for the owner to that file.
Without a terminal on stdin, `init` asks nothing: it reads the API key from
stdin, or, with nothing on stdin, prints the `holaclaw auth set` command that
stores it later. OpenRouter is tested; `api.openai.com` is not yet.

**A ChatGPT subscription.** HolaClaw signs in by itself, with a device code,
so **the Codex CLI is not needed** and the machine needs no browser: run
`holaclaw login codex` once the daemon runs
([getting-started.md](../getting-started.md#3-sign-in-to-chatgpt)). The
provider type is called `codex` only because it talks to the backend that
Codex uses with a ChatGPT account. OpenAI retires the model ids a ChatGPT
account may use every few months; take a current one from
[OpenAI's Codex models page](https://learn.chatgpt.com/docs/models) and pass
it with `--model`. On 2026-10-16 those were:

| Id | Plans | |
|---|---|---|
| `gpt-6.1-sol` | Plus, Pro, Business, Enterprise, Edu | recommended; still rolling out |
| `gpt-6-luna` | the same, and Free/Go in the desktop app | lighter; use it when Sol is not on your plan yet |
| `gpt-6-astra` | all paid plans | most capable; needs ChatGPT credits |

This flow is **unofficial**: OpenAI does not document it for third-party
clients and can change it at any time.
`internal/provider/codex/UPSTREAM.md` lists every assumption it depends on.

**An OpenAI-compatible endpoint.** [Ollama](https://ollama.com),
llama.cpp's `llama-server`, LM Studio, vLLM or a hosted service, wherever it
runs, as long as HolaClaw can reach its URL. `--base-url` ends in `/v1`, and
`--model` is a model id it serves; `curl <base-url>/models` lists them. Press
Enter when `init` asks for an API key if the endpoint needs none, as Ollama
does. A key is only ever sent over https, or over plain http to a loopback
address (rule `NET003`).

- `init` writes `context = 65536` for the model. That must be the window the
  endpoint actually runs ([context.md](../context.md)); with Ollama, start it
  with `OLLAMA_CONTEXT_LENGTH=65536` or more. `holaclaw check`, without
  `--offline`, asks the endpoint and reports `MOD002` when its window is
  smaller.
- A model that thinks by default (Qwen on Ollama) spends its reasoning from
  the reply budget: set `reasoning = "none"` on it, or on a second model used
  for `summarize`. See [context.md §8](../context.md#8-thinking-models).

To change the model or the provider later, see
[getting-started.md §3](../getting-started.md#4-change-the-model-or-the-provider).

## 3. Where things are

The guides link here when the answer depends on the deployment.

| | Linux service | Your own user | Container | NixOS |
|---|---|---|---|---|
| <a name="run-a-command"></a>**Run a command** | `sudo -u holaclaw holaclaw …` | `holaclaw …` | `docker exec holaclaw holaclaw …`, with `-it` for one that asks or opens a screen and `-i` for one that reads stdin | `sudo -u holaclaw holaclaw --config /etc/holaclaw …`, or the [wrapper](nix.md#running-isolated) of a NixOS container |
| <a name="config-directory"></a>**Config directory** | `/etc/holaclaw` | `~/.config/holaclaw` | `/etc/holaclaw`, in the `holaclaw-config` volume | `/etc/holaclaw` |
| <a name="secrets-directory"></a>**Secrets directory** (`auth.toml`) | `/etc/holaclaw/secrets` | `~/.config/holaclaw/secrets` | `/etc/holaclaw-secrets`, in the `holaclaw-secrets` volume | `/etc/holaclaw/secrets`, which the module's `secretsDir` must name |
| <a name="data-directory"></a>**Data directory** (database, blobs, backups) | `/etc/holaclaw/data` | `~/.config/holaclaw/data` | `/var/lib/holaclaw`, in the `holaclaw-data` volume | `/etc/holaclaw/data` |
| <a name="owner-token"></a>**The owner's token** (`--token-file`) | `/etc/holaclaw/token` | `~/.config/holaclaw/token` | `/etc/holaclaw-secrets/token` | `/etc/holaclaw/token` |
| <a name="edit-the-config"></a>**Edit the config** | [`sudoedit`](linux.md#6-edit-the-config) | [your editor](linux.md#9-as-your-own-user) | [copy out and back](container.md#edit-the-config-by-hand) | [on the machine, or in Nix](nix.md#declaring-the-config) |
| <a name="restart"></a>**Restart** (the config is read only at start) | `sudo systemctl restart holaclaw` | `systemctl --user restart holaclaw`, or stop and start `holaclaw run` | `docker restart holaclaw` | `sudo systemctl restart holaclaw` |
| <a name="logs"></a>**Logs** | `journalctl -u holaclaw` | `journalctl --user -u holaclaw`, or the terminal of `holaclaw run` | `docker logs holaclaw` | `journalctl -u holaclaw` |
| <a name="start-at-boot"></a>**Start at boot** | the unit is enabled | [`loginctl enable-linger`](linux.md#9-as-your-own-user) | [`--restart always`, and the engine at boot](container.md#start-at-boot) | the module |
| <a name="upgrade"></a>**Upgrade** | [linux.md §7](linux.md#7-upgrade) | [linux.md §9](linux.md#9-as-your-own-user) | [container.md](container.md#upgrade) | update the flake input and switch |
| <a name="remove"></a>**Remove** | [linux.md §8](linux.md#8-remove-it) | the same, in your home | [container.md](container.md#remove-it) | `enable = false`, then switch |
| <a name="browser"></a>**The browser** | [a second unit](../browser.md#2-setting-it-up) | [a second user service](../browser.md#2-setting-it-up) | in the `latest` image | `browser.enable` |

The container's `docker` commands work the same with `podman`.
