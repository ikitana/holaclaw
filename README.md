<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/icon-dark.png">
    <img src="docs/assets/icon-light.png" alt="The HolaClaw mascot" width="128">
  </picture>
</p>

<h1 align="center">HolaClaw</h1>

<p align="center">
  <strong>A small, stable agent harness. One binary, your models, your chats.</strong>
</p>

<p align="center">
  <a href="#quickstart">Quickstart</a> ·
  <a href="docs/getting-started.md">Getting started</a> ·
  <a href="docs/README.md">Documentation</a> ·
  <a href="ARCHITECTURE.md">Architecture</a>
</p>

---

A small, stable agent harness in Go. One binary runs an agent that talks on
Telegram, Slack, Google Chat, WhatsApp or your terminal. It uses tools behind
a policy gate, remembers, runs scheduled tasks and drives a browser. It works
with OpenRouter, OpenAI, a ChatGPT subscription, or any other OpenAI-compatible
endpoint, such as Ollama.

## Quickstart

### On macOS

Download the HolaClaw app from [holaclaw.ai](https://holaclaw.ai/). It runs
HolaClaw in a lightweight virtual machine on your Mac and manages it for you,
from the app or its command line.

### On Linux

On `amd64` or `arm64`:

```
curl -fsSL https://holaclaw.ai/install.sh | bash
```

The script asks three things:

1. **How to run it.** It offers what the machine supports (below).
2. **Whether to set up the browser**, so the agent can open web pages.
3. **Your provider and its API key.** It then runs `holaclaw init`, which
   writes a config for that one provider and model, starts the daemon, and
   prints the command to chat.

Once it is running, go on with [Getting started](docs/getting-started.md):
chat with the agent, change the model, and pair a device.

#### How it runs

| How | Needs | Config in | Chat with |
|---|---|---|---|
| **Container** (recommended) | Docker or Podman | the `holaclaw-config` volume | `docker exec -it holaclaw holaclaw chat` |
| **System service** | systemd, `sudo` | `/etc/holaclaw` | `sudo -u holaclaw holaclaw chat` |
| **As your own user** | systemd, no `sudo` | `~/.config/holaclaw` | `holaclaw chat` |

<details>
<summary>More about each deployment type</summary>

- **The container** (`ghcr.io/ikitana/holaclaw`) is isolated from the host.
  Its config, secrets and data go in the named volumes `holaclaw-config`,
  `holaclaw-secrets` and `holaclaw-data`, and the daemon answers on
  `127.0.0.1:8484`, on this machine only. It runs as you, with no `sudo`,
  under rootless Docker, Podman, or a Docker you may use without `sudo`. With
  Podman, chat with `podman exec`.
- **The system service** is hardened and runs under its own `holaclaw` user.
  It needs `sudo`, as does a Docker your user may not use.
- **As your own user** is a user service that runs as you. The script offers
  it when `sudo` fails ([details](docs/deployments/linux.md#9-as-your-own-user)).

</details>

#### The browser

The browser lets the agent open web pages in Chromium.

- **In the container** it is on by default, and the container runs with
  `--security-opt seccomp=unconfined` for Chromium's sandbox. Saying no runs
  the `slim` image, without Chromium.
- **As a service** it is off by default. Saying yes installs Chromium and Xvfb
  with apt; as your own user, they must be installed already.

#### Options

Choose the provider without the prompt:

```
u=https://holaclaw.ai/install.sh
curl -fsSL $u | bash -s -- --provider openrouter      # asks for the API key; model deepseek/deepseek-v4.1-flash
curl -fsSL $u | bash -s -- --provider openai          # asks for the API key; model gpt-6.1-sol
curl -fsSL $u | bash -s -- --provider openai-codex    # a ChatGPT subscription; model gpt-6.1-sol
curl -fsSL $u | bash -s -- --provider openai-compat \
  --base-url http://localhost:11434/v1 --model gemma4:e2b   # Ollama on this machine
```

- `--model` replaces any provider's default model.
- With a ChatGPT subscription, sign in once the daemon runs; the script
  prints the `holaclaw login codex` command for your install.
- For Ollama from the container, use `http://host.docker.internal:11434/v1`,
  and make Ollama listen beyond loopback (`OLLAMA_HOST=0.0.0.0`).
- `--container`, `--service` or `--user` picks how it runs, and `--browser`
  or `--no-browser` whether the browser is set up.
- With no terminal, as in CI, `--provider` is required, and the script prints
  the `auth set` command that stores the API key.
- To read the script before you run it, download it and run
  `bash install.sh`.

Every way to run it, step by step, with every command the script runs and
where each one keeps its config, secrets and data:
[docs/deployments](docs/deployments/README.md).

## Next steps

- **Chat, change the model, pair a device:** [getting-started.md](docs/getting-started.md)
- **Talk to it from a chat app:** [Telegram](docs/channels/telegram.md),
  [Slack](docs/channels/slack.md),
  [Google Chat](docs/guides/googlechat.md),
  [WhatsApp](docs/guides/whatsapp.md)
- **Invite people and give them roles:** [identity.md](docs/identity.md)
- **Give it tools from an MCP server:** [guides/connectors.md](docs/guides/connectors.md)
- **Let it drive a browser:** [browser.md](docs/browser.md)
- **Teach it procedures:** [skills.md](docs/skills.md)
- **Run it day to day** (config, upgrades, backups, invites): [guides/operator.md](docs/guides/operator.md)
- **Understand what it may do on its own:** [security.md](docs/security.md)

## Reference

- **Every doc:** the [documentation index](docs/README.md).
- **How it fits together:** [ARCHITECTURE.md](ARCHITECTURE.md), a map of the
  project and its code.
- **How it works:** [sessions and messages](docs/sessions.md),
  [models](docs/models.md), [the context window](docs/context.md),
  [memory](docs/memory.md), [tasks](docs/tasks.md),
  [traceability](docs/traceability.md).
- **Generated from the code:** [config keys](docs/reference/config.md),
  [commands](docs/reference/cli.md), [API](docs/reference/api.md),
  [events](docs/reference/events.md),
  [`holaclaw check` rules](docs/reference/check-rules.md).
