# HolaClaw in a container

The image `ghcr.io/ikitana/holaclaw` holds the daemon, the browser sidecar,
Chromium and Xvfb, for `linux/amd64` and `linux/arm64`. It is a supported
way to run HolaClaw, and the recommended one when the machine does anything
else: the agent, its tools and `exec` see only the container, never the host,
and the only programs `exec` can run are the ones in the image. Inside, the
daemon and the browser sidecar run as two users, the same split the systemd
units make.

Every command in this guide works the same with `podman` in place of
`docker`, and with Docker or Podman rootless, as your own user.

Inside the image the binary, the commands and the paths still use the name
holaclaw; see the note in [README.md](README.md), which also lists the other
deployments. Once the container runs and `init` is done, continue with
[getting-started.md](../getting-started.md).

## Tags

Every tag is a multi-arch image, for `linux/amd64` and `linux/arm64`.

| Tag | What it is |
|---|---|
| `latest` | the newest release that is not a pre-release, with the browser |
| `vX.Y.Z` | a release, with the browser |
| `slim` | `latest` without Chromium and Xvfb, and so without the browser |
| `vX.Y.Z-slim` | a release without the browser |

Every image of a release is signed; `cosign verify` checks one, as
[security.md](../security.md#verifying-a-release) shows.

## Run it

The configuration, the secrets and the data live in three named volumes:

```
docker run -d --name holaclaw --restart always \
  -v holaclaw-config:/etc/holaclaw \
  -v holaclaw-secrets:/etc/holaclaw-secrets \
  -v holaclaw-data:/var/lib/holaclaw \
  -p 127.0.0.1:8484:8484 --security-opt seccomp=unconfined \
  --add-host host.docker.internal:host-gateway \
  ghcr.io/ikitana/holaclaw:latest
```

| Volume | In the container | What it holds |
|---|---|---|
| `holaclaw-config` | `/etc/holaclaw` | `config.toml`, the agents, `channels.toml` once you add one |
| `holaclaw-secrets` | `/etc/holaclaw-secrets` | `auth.toml`: API keys and sign-in tokens; the owner's admin token |
| `holaclaw-data` | `/var/lib/holaclaw` | the database, the blobs, the daemon's backups |

Docker or Podman creates the volumes on first start. The container then
starts with a config that has no provider, and waits for `init`. On every
start, the entrypoint gives the three directories to the container's own
users. Use named volumes, not directories of the host: on a host directory
that would change the owner of your files.

`holaclaw` in the container runs as the `holaclaw` user, even under `docker
exec`, which runs as root. Run every command as `docker exec holaclaw
holaclaw <command>`, with `-it` for one that asks or opens a screen, and `-i`
for one that reads stdin.

`--security-opt seccomp=unconfined` is for Chromium's own sandbox, which needs
user namespaces that Docker's default seccomp profile blocks. HolaClaw never
runs Chromium with `--no-sandbox`: without the flag, the sidecar refuses to
start a browser and `holaclaw check` says why. If you do not use the browser,
run the `slim` image instead and leave the flag out: it has no Chromium, and
the container keeps the default seccomp profile. To turn the browser off in
`latest`, pass `-e HOLACLAW_BROWSER_ENABLED=false`.
`--add-host` lets the container reach services on the host, such as Ollama
(below).

**Port 8484.** The daemon listens on `0.0.0.0:8484` inside the container, and
`-p 127.0.0.1:8484:8484` publishes it on the host's loopback only. Clients
that reach the [Control API](../reference/api.md) over TCP need the owner's admin
token. To expose it beyond this machine, change the `-p`, and put TLS in
front of it. The image sets these defaults as environment variables, which
win over `config.toml`; pass `-e` to change one:

| Variable | Config key | `latest` | `slim` |
|---|---|---|---|
| `HOLACLAW_LISTEN` | `server.listen` | `0.0.0.0:8484` | `0.0.0.0:8484` |
| `HOLACLAW_ALLOW_INSECURE_TRANSPORT` | `server.allow_insecure_transport` | `true` | `true` |
| `HOLACLAW_BROWSER_ENABLED` | `browser.enabled` | `true` | not set |

`docker exec holaclaw holaclaw config env` shows each value and where it comes
from.

## Initialise

`init` writes a config for one provider and one model, stores the API key,
creates the owner and, with `--token-file`, writes an admin token for the
owner to the secrets volume. The daemon starts by itself once it has run; no
restart is needed. Pick the provider you have.

### OpenRouter

```
docker exec -it holaclaw holaclaw init --provider openrouter --token-file /etc/holaclaw-secrets/token
docker exec -it holaclaw holaclaw chat
```

`init` asks for the API key, without echo, and shows the model,
`deepseek/deepseek-v4.1-flash`: press Enter to keep it, or type another id.

### OpenAI

```
docker exec -it holaclaw holaclaw init --provider openai --token-file /etc/holaclaw-secrets/token
docker exec -it holaclaw holaclaw chat
```

The same as OpenRouter, with an OpenAI API key and the model `gpt-6.1-sol`.

### A ChatGPT subscription

```
docker exec -it holaclaw holaclaw init --provider openai-codex --token-file /etc/holaclaw-secrets/token
docker exec -it holaclaw holaclaw login codex
docker exec -it holaclaw holaclaw chat
```

`init` asks for nothing secret. `login codex` signs in once the daemon runs,
a few seconds after `init`: it prints a URL and a code, as in
[getting-started.md §2](../getting-started.md#3-sign-in-to-chatgpt).

### Any OpenAI-compatible endpoint

`--provider openai-compat` takes any endpoint that speaks the OpenAI API:
Ollama, llama.cpp's `llama-server`, LM Studio, vLLM or a hosted service.
Where it runs does not matter, on this machine, another one or the cloud, as
long as the container can reach its URL:

```
docker exec -it holaclaw holaclaw init --provider openai-compat \
  --base-url http://ollama.lan:11434/v1 --model gemma4:e2b
docker exec holaclaw holaclaw check
docker exec -it holaclaw holaclaw chat
```

`--base-url` is the endpoint's URL, ending in `/v1`, and `--model` a model id
it serves; `curl <base-url>/models` lists them. In the container, `localhost`
is the container itself: for an endpoint on this machine, use
`http://host.docker.internal:<port>/v1`, which needs the `--add-host` of the
`docker run` above. An endpoint elsewhere needs no `--add-host`.

**The API key.** `init` asks for one: type it, or press Enter if the
endpoint needs none, as Ollama does. If you already know it needs none, run
`init` without `-it`: with no terminal it asks nothing and writes the provider
without a key. A key is never sent over plain http to another host (rule
`NET003`): an endpoint with a key needs an `https://` base URL.

With the key in a file, pipe it in instead, with `-i` and no `-t`:

```
docker exec -i holaclaw holaclaw init --provider openai-compat \
  --base-url https://llm.example.com/v1 --model <id> < key.txt
```

To add a key later, add `auth = "api-key"` to `[providers.openai-compat]`
in `config.toml` ([edit the config by hand](#edit-the-config-by-hand)),
store the key, and restart; without that line the key is never sent, and
`check` warns `SEC007`:

```
docker exec -i holaclaw holaclaw auth set providers.openai-compat api_key < key.txt
docker restart holaclaw
```

**The window.** `init` writes `context = 65536` for the model. That must be
the window the endpoint actually runs ([context.md](../context.md)); with Ollama,
start it with `OLLAMA_CONTEXT_LENGTH=65536` or more. `check`, without
`--offline`, asks the endpoint and reports `MOD002` when its window is
smaller.

**Ollama.** Pull the model (`ollama pull gemma4:e2b`). Ollama listens on
loopback by default, which a container cannot reach, even on the same
machine: make it listen beyond loopback (`OLLAMA_HOST=0.0.0.0`) and firewall
its port, 11434, to the machines that need it, since Ollama has no auth.

### After init

`--token-file` is optional. With it, the owner's admin token is in the
secrets volume; without it, `init` creates no token at all. You need one only
for scripts that reach the daemon over TCP, through the published port; if
nothing does, leave `-p` out of `docker run`, since `docker exec` needs no
port. With `--token-file`:

```
docker exec holaclaw cat /etc/holaclaw-secrets/token
```

**Pairing a device** ([getting-started.md §4](../getting-started.md#5-pair-a-device)).
The apps sign in through the published port with a one-shot code. Inside the container the daemon listens on `0.0.0.0`, which is no address a
device can reach, so the code comes without a link: enter it and the server
address in the app. For a link and a QR code, pass the address on `docker
run`: `-e HOLACLAW_PUBLIC_URL=https://claw.example`.

Without `-t`, `init` asks nothing. It reads the API key from stdin, or, with
nothing on stdin, prints the `auth set` command that stores it later:

```
read -rs KEY && printf %s "$KEY" | docker exec -i holaclaw holaclaw init --provider openrouter --token-file /etc/holaclaw-secrets/token; unset KEY
```

The daemon answers a few seconds after `init`; until then, `status` says it
is not running:

```
docker exec holaclaw holaclaw status
```

With the browser on, `holaclaw check` warns `BRW005` and `BRW006`: the agent
may open any site, and it has no decision model for the browser.
[browser.md §10](../browser.md#configuration) explains both.

## Start at boot

`--restart always` restarts the container when it stops and when the
engine starts. Docker starts at boot as a system service. Podman has no
daemon: its `podman-restart.service` starts these containers at boot.

```
sudo systemctl enable docker                       # Docker
sudo systemctl enable podman-restart.service       # Podman as root
systemctl --user enable podman-restart.service     # Podman rootless, with:
sudo loginctl enable-linger "$USER"                # start your services at boot, without a login
```

Rootless Docker starts as your user's `docker.service`, which also needs
`loginctl enable-linger`.

## Edit the config by hand

To change the model, add a second provider or switch to another one, copy
the files out of the container, edit them, and write them back over the
originals, which keeps their owner (`docker cp` would leave them root's,
which the daemon cannot read). Store any secret with `auth set`, never in a
file you copy in. Then check the config and restart:

```
docker cp holaclaw:/etc/holaclaw/config.toml .
docker cp holaclaw:/etc/holaclaw/agents/main/agent.toml .
$EDITOR config.toml agent.toml
docker exec -i holaclaw sh -c 'cat > /etc/holaclaw/config.toml' < config.toml
docker exec -i holaclaw sh -c 'cat > /etc/holaclaw/agents/main/agent.toml' < agent.toml
read -rs KEY && printf %s "$KEY" | docker exec -i holaclaw holaclaw auth set providers.openai api_key; unset KEY
docker exec holaclaw holaclaw check --offline
docker restart holaclaw
```

The providers and models are the same as on a host: copy a snippet from
[getting-started.md §3](../getting-started.md#4-change-the-model-or-the-provider),
and store the key it needs with `auth set` as above. For a ChatGPT
subscription, sign in after the restart with `docker exec -it holaclaw
holaclaw login codex`.

## Backups

Do not copy the volumes of a running container: the database can be caught
mid-write. Have the daemon take a backup and copy it out. With
`--include-secrets`, the archive holds the config, `auth.toml`, the database
and the blobs: everything needed to start over, and every credential, so keep
it like a key. The owner's admin token file is not in it; copy it out beside
the archive:

```
docker exec holaclaw holaclaw backup create --wait --include-secrets
docker cp holaclaw:/var/lib/holaclaw/backups/<id>.tar.gz .
(umask 077; docker exec holaclaw cat /etc/holaclaw-secrets/token > token)
```

`backup create` prints the `<id>`. The daemon also backs itself up every 24
hours (`backups.every`) and keeps the newest 7 (`backups.keep`), without the
secrets, in the same directory; `backup list` lists them.

**Restore on a new host**, or onto this one: start from empty volumes (`docker
rm -f holaclaw` and `docker volume rm` the three, or other names), put the
archive in the data volume, restore it with no daemon running, put the token
back in the secrets volume, then start the container and remove the archive:

```
docker run --rm -i -v holaclaw-data:/var/lib/holaclaw --entrypoint sh ghcr.io/ikitana/holaclaw:latest \
  -c 'cat > /var/lib/holaclaw/restore.tar.gz' < <id>.tar.gz
docker run --rm -v holaclaw-config:/etc/holaclaw -v holaclaw-secrets:/etc/holaclaw-secrets \
  -v holaclaw-data:/var/lib/holaclaw ghcr.io/ikitana/holaclaw:latest \
  backup restore --yes /var/lib/holaclaw/restore.tar.gz
docker run --rm -i -v holaclaw-secrets:/etc/holaclaw-secrets --entrypoint sh ghcr.io/ikitana/holaclaw:latest \
  -c 'umask 077; cat > /etc/holaclaw-secrets/token' < token
# the `docker run` above, then:
docker exec holaclaw rm /var/lib/holaclaw/restore.tar.gz
```

The owner and the API tokens come with the database, so no `init` is needed,
and `init` refuses to run on a restored installation: the container starts
the daemon on the restored config. On a running installation, `docker exec
holaclaw holaclaw backup restore <id>` rolls the database and the blobs back
to one of its own backups instead. [guides/operator.md](../guides/operator.md#5-backups) covers both commands in full.

## Upgrade

```
docker pull ghcr.io/ikitana/holaclaw:latest
docker rm -f holaclaw
# the same `docker run` as above
```

The volumes keep everything. `init` refuses to run a second time.

## Remove it

```
docker rm -f holaclaw
docker volume rm holaclaw-config holaclaw-secrets holaclaw-data
```

Removing the volumes deletes the config, the secrets and the data; take a
backup first if you may want them back.

## Build the image yourself

From a checkout of the repository, with Nix:

```
nix build .#container        # this machine's architecture; also .#container-amd64, .#container-arm64
nix build .#container-slim   # slim; also .#container-slim-amd64, .#container-slim-arm64
./result | docker load
```

Both architectures build on any Linux machine. The arm64 image does not need
an arm64 builder: its packages come from the Nix binary cache, and the binary
is cross-compiled. The release workflow publishes these images.

Without Nix, the `Dockerfile` builds equivalent images on Debian
(`docker build -t holaclaw .`, and `docker build --target slim -t holaclaw:slim .`).
