# HolaClaw on Linux, with systemd

From a fresh Linux machine to a running daemon. The main path is **Debian
or a Debian derivative, with systemd**: download the binary, initialise it
for your provider, and run it as a hardened system service under its own
user. Other distributions with systemd work the same way, with their own
package manager in place of `apt` and `dpkg`. With no root, run it
[as your own user](#9-as-your-own-user).

The other deployments are in [README.md](README.md). Once the daemon runs,
continue with [getting-started.md](../getting-started.md).

## Contents

1. [Before you start](#1-before-you-start)
2. [Download and verify](#2-download-and-verify)
3. [Install the binary and create the service user](#3-install-the-binary-and-create-the-service-user)
4. [Initialise](#4-initialise)
5. [Start the service](#5-start-the-service)
6. [Edit the config](#6-edit-the-config)
7. [Upgrade](#7-upgrade)
8. [Remove it](#8-remove-it)
9. [As your own user](#9-as-your-own-user)

## 1. Before you start

You need:

- Linux on `amd64` or `arm64`. This guide was tested on Ubuntu 26.04 LTS, a
  Debian derivative.
- A user with `sudo`.
- A model provider: an API key for OpenRouter or OpenAI, a ChatGPT
  subscription, or any OpenAI-compatible endpoint, such as Ollama
  ([README.md §2](README.md#2-choose-a-provider)).

HolaClaw is one static binary with no runtime dependencies. Chromium, Xvfb
and a basic set of fonts are needed only for the browser tool, which is
optional ([browser.md](../browser.md#2-setting-it-up)).

## 2. Download and verify

Download the latest release and check it:

```
arch=$(dpkg --print-architecture)          # amd64 or arm64; elsewhere: uname -m
base=https://github.com/ikitana/holaclaw/releases/latest/download
mkdir -p ~/holaclaw && cd ~/holaclaw
curl -fL --remote-name-all "$base/holaclaw-linux-$arch" "$base/holaclaw.service" "$base/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS
```

Each file must print `OK`. Each release has the binary for both
architectures, the systemd units (`holaclaw.service` and
`holaclaw-browser.service`) and `SHA256SUMS`. `wget` works as well as `curl`.

Every file of a release is signed. To check that one comes from HolaClaw's
release workflow, verify it, or `SHA256SUMS`, with `cosign`, as
[security.md](../security.md#verifying-a-release) shows.

## 3. Install the binary and create the service user

```
sudo install -m 755 "holaclaw-linux-$arch" /usr/local/bin/holaclaw
holaclaw version

sudo useradd --system --home-dir /var/lib/holaclaw --shell /usr/sbin/nologin holaclaw
sudo install -d -o holaclaw -g holaclaw -m 700 /etc/holaclaw
```

The daemon runs as `holaclaw`, a system user with no login shell. Its
configuration directory is `/etc/holaclaw`, which only that user may read.

## 4. Initialise

Pick the provider you have. What each one asks for is in
[README.md §2](README.md#2-choose-a-provider).

### OpenRouter

```
sudo -u holaclaw holaclaw init --provider openrouter --token-file /etc/holaclaw/token
```

### OpenAI

```
sudo -u holaclaw holaclaw init --provider openai --token-file /etc/holaclaw/token
```

### A ChatGPT subscription

```
sudo -u holaclaw holaclaw init --provider openai-codex --token-file /etc/holaclaw/token
```

You sign in once the service is running.

### Ollama, or another OpenAI-compatible endpoint

```
sudo -u holaclaw holaclaw init --provider openai-compat --base-url http://localhost:11434/v1 --model gemma4:e2b --token-file /etc/holaclaw/token
```

This is Ollama on the same machine: install it and pull the model
(`ollama pull gemma4:e2b`). Any other endpoint works the same way, with its
own `--base-url` and `--model`.

### What init writes

`init` writes a complete, self-contained installation into `/etc/holaclaw`:

| Path | What it is |
|---|---|
| `config.toml` | the provider, the model, the server; [every key](../reference/config.md) |
| `agents/main/` | the agent: `agent.toml` (its tools and models) and `prompt.md` |
| `data/` | the database and the blobs |
| `secrets/` | `auth.toml`: API keys and sign-in tokens |
| `token` | the owner's admin token, mode 0600 |

You do not need the token on this machine. The CLI talks to the daemon over
its control socket, and file permissions decide who may use it. The token is
for scripts that reach the daemon's Control API over TCP
([reference/api.md](../reference/api.md)).

Every `holaclaw` command reads `/etc/holaclaw` by default. Run each one as the
`holaclaw` user (`sudo -u holaclaw holaclaw …`). Your own user cannot read that
directory or reach the daemon's socket. If typing `sudo -u holaclaw` gets
tedious, add an alias to your shell: `alias hc='sudo -u holaclaw holaclaw'`.

**From a script.** Without a terminal on stdin, `init` asks nothing: pass
`--provider`, and for `openai-compat` also `--base-url` and `--model`. It
reads the API key from stdin:

```
read -rs KEY && printf %s "$KEY" | sudo -u holaclaw holaclaw init --provider openrouter --token-file /etc/holaclaw/token; unset KEY
```

## 5. Start the service

Install the unit you downloaded and start it:

```
sudo install -m 644 holaclaw.service /etc/systemd/system/holaclaw.service
sudo systemctl daemon-reload
sudo systemctl enable --now holaclaw
systemctl status holaclaw
```

The unit runs `holaclaw run` as the `holaclaw` user, behind systemd's
sandboxing ([security.md §9](../security.md#9-deployment)). It restarts the
daemon on failure and starts it at boot. The daemon listens on
`/run/holaclaw/holaclaw.sock` a few seconds after it starts; until then,
`status` says it is not running:

```
sudo -u holaclaw holaclaw status
```

The daemon backs itself up every 24 hours, under `/etc/holaclaw/data/backups/`.
To take one now, run `sudo -u holaclaw holaclaw backup create --wait`
([guides/operator.md](../guides/operator.md#5-backups)).

HolaClaw is running. Continue with
[getting-started.md](../getting-started.md): chat, and, with a ChatGPT
subscription, sign in first.

## 6. Edit the config

Edit the files with `sudoedit`, which keeps their owner and mode. What to
write in them, for a new model or provider, is in
[getting-started.md §3](../getting-started.md#4-change-the-model-or-the-provider).

```
sudoedit /etc/holaclaw/config.toml
sudoedit /etc/holaclaw/agents/main/agent.toml
```

Store a secret with `auth set`, which reads it from stdin, so it never lands
in your shell history:

```
read -rs KEY && printf %s "$KEY" | sudo -u holaclaw holaclaw auth set providers.openai api_key; unset KEY
```

Check the config, and restart the daemon, which reads it only at start:

```
sudo -u holaclaw holaclaw check --offline
sudo systemctl restart holaclaw
```

## 7. Upgrade

Download the new release as in section 2, then:

```
sudo install -m 755 "holaclaw-linux-$arch" /usr/local/bin/holaclaw
sudo install -m 644 holaclaw.service /etc/systemd/system/holaclaw.service
sudo systemctl daemon-reload && sudo systemctl restart holaclaw
sudo -u holaclaw holaclaw config migrate     # says whether the config format changed; --write applies the diff
```

## 8. Remove it

```
sudo systemctl disable --now holaclaw
sudo rm /etc/systemd/system/holaclaw.service /usr/local/bin/holaclaw
sudo systemctl daemon-reload
sudo userdel holaclaw
sudo rm -rf /etc/holaclaw /var/lib/holaclaw
```

The last line deletes the config, the secrets and the data; take a backup
first if you may want them back. With the browser set up, remove
`holaclaw-browser.service` and the `holaclaw-browser` user the same way.

## 9. As your own user

With no `sudo`, HolaClaw can run as you, from your home. The installer of the
[README](../../README.md#quickstart) offers it as "As your own user", or does it
with `--user`:

```
curl -fsSL https://holaclaw.ai/install.sh | bash -s -- --user --provider openrouter
```

Any provider works, with its flags; without `--provider`, `init` asks for
one. It installs the binary to `~/.local/bin/holaclaw`, runs
`holaclaw init` into `~/.config/holaclaw`, which asks for the API key, and
starts the daemon as a systemd user service, `holaclaw`, from
`~/.config/systemd/user/holaclaw.service`. It starts at boot
once you linger, which on Debian only root may allow:
`sudo loginctl enable-linger <you>`. The container is still the better
choice: here the agent, and any command its `exec` tool runs, run as you,
with your home in reach. With `--browser`, the sidecar runs as you too, under
a second user service, `holaclaw-browser` (see
[browser.md](../browser.md#2-setting-it-up)); install Chromium, Xvfb and the
fonts first.
To edit the config, edit the files in `~/.config/holaclaw` directly, then
`systemctl --user restart holaclaw`. To upgrade, install the new binary over
`~/.local/bin/holaclaw` and restart the same way.

For a quick try with no service, run the daemon in a terminal. Put the
binary on your `PATH`. Debian adds `~/.local/bin` to it at login only once
the directory exists, so add it by hand the first time:

```
install -Dm755 "holaclaw-linux-$arch" ~/.local/bin/holaclaw
export PATH="$HOME/.local/bin:$PATH"
```

Then:

```
holaclaw init --provider openrouter --token-file ~/.config/holaclaw/token
```

Any provider of section 4 works the same way, with its own flags.

As a user other than root, on a machine with no `/etc/holaclaw`, the config
directory defaults to `$XDG_CONFIG_HOME/holaclaw` (`~/.config/holaclaw`), and
the control socket to `$XDG_RUNTIME_DIR/holaclaw.sock`. With a system install
on the same machine, the defaults are its own: set
`HOLACLAW_CONFIG=~/.config/holaclaw` in every terminal, and the socket in the
`[server]` table of its `config.toml`, written out, as TOML does not expand
variables (`socket = "/run/user/1000/holaclaw.sock"`).

Drop `sudo -u holaclaw` from every command of the guides, and edit the files
directly. Run the daemon in one terminal and chat from another:

```
holaclaw check --offline
holaclaw run
holaclaw chat                               # in a second terminal
```

`holaclaw chat` also works with no daemon running, but the ChatGPT sign-in
needs the daemon.
