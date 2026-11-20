# Getting started with HolaClaw

HolaClaw is an agent harness in a single binary. A daemon runs your agent,
which you talk to from your terminal, the HolaClaw apps, Telegram, Slack,
Google Chat or WhatsApp. It uses tools behind a policy gate, remembers what
you tell it, runs scheduled tasks and drives a browser. It works with
OpenRouter, OpenAI, a ChatGPT subscription, or any other OpenAI-compatible
endpoint, such as Ollama.

This guide takes you from installing HolaClaw to a chat with your agent, and
what to do next.

## Contents

1. [Install it](#1-install-it)
2. [Chat](#2-chat)
3. [Sign in to ChatGPT](#3-sign-in-to-chatgpt)
4. [Change the model or the provider](#4-change-the-model-or-the-provider)
5. [Pair a device](#5-pair-a-device)
6. [Next steps](#6-next-steps)

## 1. Install it

On Linux (`amd64` or `arm64`), the install script does it all:

```
curl -fsSL https://holaclaw.ai/install.sh | bash
```

It asks how to run HolaClaw, in a container (recommended) or as a systemd
service, and then for the provider and its API key. When it finishes, the
daemon is running and it prints the command to chat.

To do the same by hand, or on NixOS, follow the guide of a deployment:

- [In a container](deployments/container.md), with Docker or Podman,
  isolated from the host.
- [As a Linux system service](deployments/linux.md), under its own `holaclaw`
  user, or [as your own user](deployments/linux.md#9-as-your-own-user),
  without `sudo`.
- [On NixOS](deployments/nix.md), as a NixOS module.

[Deployments](deployments/README.md) compares them. Each one ends with
`holaclaw init` done and the daemon running. The rest of this guide writes
commands as `holaclaw …`; run them
[the way your deployment does](deployments/README.md#run-a-command).

## 2. Chat

```
holaclaw chat
```

It attaches to the running daemon. At a terminal it opens a full-screen UI,
with the reply on the left and the trace of the run on the right. `ctrl+p`
lists the slash commands and `F1` lists every key. To ask one question and
print the answer:

```
$ holaclaw chat -m "Reply with exactly: hola"
·   0.6s model step 1 → default (deepseek/deepseek-v4.1-flash)
hola
```

## 3. Sign in to ChatGPT

Only with a ChatGPT subscription (`--provider openai-codex`). The sign-in
runs inside the daemon, which keeps the tokens and refreshes them:

```
holaclaw login codex
```

It prints a URL and a code:

```
Open https://auth.openai.com/codex/device and enter the code: XXXX-XXXXX
```

Your ChatGPT account must allow this kind of sign-in: enable **device code
sign-in** in ChatGPT's Security Settings first. With it off, OpenAI's page
refuses the code and tells you to run `codex login` again: enable the setting
and run `holaclaw login codex` again instead.

Open the URL on any device, sign in with your ChatGPT account, enter the code
and approve. The command returns `codex: logged in`. You do this once: the
daemon refreshes the tokens from then on. If they are ever revoked,
`holaclaw login codex` signs in again.

## 4. Change the model or the provider

`init` writes one provider and one model. To change the model, add another
provider, or switch to one, [edit the config](deployments/README.md#edit-the-config).
A **provider** is an endpoint, a **model** is an id on that endpoint, and the
agent's `chat` role names the model to use:

1. Add the provider's snippet to `config.toml`.
2. Point the agent at the model: in `agents/main/agent.toml`, the `chat` line
   under `[models]` says `"default"`, the model `init` wrote. Change it to the
   name of the `[models.<name>]` table you added:

   ```toml
   [models]
   chat = "chatgpt"
   ```

3. Store any secret it needs with `holaclaw auth set`. Secrets never go in
   `config.toml`: they live in `auth.toml`, in the
   [secrets directory](deployments/README.md#secrets-directory).
4. Check the config, and [restart](deployments/README.md#restart) the daemon,
   which reads it only at start.

### A ChatGPT subscription

```toml
[providers.codex]
type = "codex"
auth = "subscription"

[models.chatgpt]
provider = "codex"
id       = "gpt-6.1-sol"
context  = 128000
tools    = true
```

`context` is an estimate, because OpenAI does not publish the window sizes.
Restart, then sign in with `holaclaw login codex` (section 3).

### An OpenAI-compatible endpoint with an API key

This covers OpenAI's API, OpenRouter, and any other endpoint that speaks the
OpenAI API:

```toml
[providers.openai]
type     = "openai-compatible"
base_url = "https://api.openai.com/v1"     # OpenRouter: https://openrouter.ai/api/v1
auth     = "api-key"

[models.chatgpt]
provider = "openai"
id       = "<a model id of that endpoint>"
context  = 128000
tools    = true
```

Store the key. The command reads it from stdin, so the key never lands in
your shell history:

```
read -rs KEY && printf %s "$KEY" | holaclaw auth set providers.openai api_key; unset KEY
```

`holaclaw check` reports `SEC002` until the key is stored. For an endpoint
with no key, leave out `auth`; without it, a stored key is never sent, and
`check` warns `SEC007`.

### Check the config

```
holaclaw check --offline
```

`ok: no findings` means the config loads. Any finding names a rule; every
rule is listed in [reference/check-rules.md](reference/check-rules.md).
Without `--offline`, `check` also contacts the model endpoints.

## 5. Pair a device

The apps sign in with a one-shot code
([identity.md §3](identity.md#3-apps-devices-and-tokens)). `init` prints the
command with the owner's id; `holaclaw identity list` shows it too:

```
holaclaw invite create --identity <owner id> --channel holaclaw
```

It prints the code and, when the daemon knows the address devices reach it
at, a `holaclaw://pair?server=…&code=…` link and a QR code of it. That
address is `server.public_url`, or `$HOLACLAW_PUBLIC_URL`, else
`server.listen` when it names a host other devices reach: a loopback or
wildcard listen gives no link. Set `public_url` when the daemon listens on
loopback, or sits behind a port mapping or a TLS proxy. Without a link, enter
the code and the server address in the app.

## 6. Next steps

- Talk to the agent from [Telegram](channels/telegram.md),
  [Slack](channels/slack.md),
  [Google Chat](guides/googlechat.md) or [WhatsApp](guides/whatsapp.md).
- Invite other people and give them roles: [identity.md](identity.md).
- Give the agent the tools of a remote MCP server:
  [guides/connectors.md](guides/connectors.md).
- Let the agent drive a browser: [browser.md](browser.md#2-setting-it-up).
- Run it day to day: logs, config changes, upgrades, backups and common
  issues are in the [operator manual](guides/operator.md).
- Every config key: [reference/config.md](reference/config.md). Every
  command: [reference/cli.md](reference/cli.md).
