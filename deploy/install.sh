#!/usr/bin/env bash
# Installs the latest HolaClaw release. README.md's quickstart runs it:
#   curl -fsSL https://holaclaw.ai/install.sh | bash
# With Docker or Podman present it offers the container of docs/deployments/container.md;
# otherwise, or when asked, the hardened systemd service of
# docs/deployments/linux.md, or user services as your own user, with no sudo
# (section 9 of the guide). It always asks before installing, and whether to
# set up the browser, then runs `holaclaw init`, which asks for the provider
# and its API key. To choose without a prompt: `... | bash -s --
# --container` (or `--service`, or `--user`), `--browser` or `--no-browser`,
# and `--provider P`, with `--model ID` and `--base-url URL` passed to init.
set -euo pipefail

image=ghcr.io/ikitana/holaclaw:latest
base=https://github.com/ikitana/holaclaw/releases/latest/download
docs=https://github.com/ikitana/holaclaw/blob/main/docs
tmp=""

say() { printf '%s\n' "$*"; }
die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# as_root runs a command as root, through sudo unless the script is root.
as_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  else
    sudo "$@"
  fi
}

# as_holaclaw runs a command as the holaclaw user.
as_holaclaw() {
  if [ "$(id -u)" -eq 0 ]; then
    runuser -u holaclaw -- "$@"
  else
    sudo -u holaclaw "$@"
  fi
}

# need_root fails unless the script is root or the person may sudo, asking
# for the password now, before anything changes. On a terminal it offers to
# install as the person's own user instead, and then does that and exits.
need_root() {
  local answer
  [ "$(id -u)" -eq 0 ] && return 0
  if command -v sudo >/dev/null 2>&1; then
    sudo -n true 2>/dev/null && return 0
    have_tty && sudo -v && return 0
  fi
  if ! have_tty || ! has_systemd; then
    die "this needs root or sudo; to install HolaClaw as your own user instead, pass --user"
  fi
  answer=$(ask "This needs root or sudo. Install as your own user instead, with no sudo? [Y/n]")
  case "${answer:-y}" in
    y | Y | yes) ;;
    *) die "this needs root or sudo" ;;
  esac
  method=user
  browser=$browser_arg
  choose_browser
  install_user
  exit 0
}

# ask reads an answer from the terminal: under `curl | bash`, stdin is the
# script itself.
ask() {
  local answer=""
  printf '%s ' "$1" >/dev/tty
  read -r answer </dev/tty || true
  printf '%s' "$answer"
}

have_tty() { (exec </dev/tty) 2>/dev/null; }

has_systemd() { [ -d /run/systemd/system ]; }

# detect_engine sets engine to podman or docker, whichever is installed
# (podman first: podman-docker installs a `docker` that is podman), sets
# engine_sudo when Docker needs root, and rootless when the engine runs as
# this user. Podman never runs through sudo: as a user it is rootless.
engine=""
engine_sudo=""
rootless=""
detect_engine() {
  if command -v podman >/dev/null 2>&1; then
    engine=podman
  elif command -v docker >/dev/null 2>&1; then
    engine=docker
  else
    return 0
  fi
  [ "$(id -u)" -ne 0 ] || return 0
  if [ "$engine" = podman ]; then
    rootless=1
  elif ! docker info >/dev/null 2>&1; then
    engine_sudo=sudo
  elif docker info --format '{{.SecurityOptions}}' 2>/dev/null | grep -q rootless; then
    rootless=1
  fi
}

ctr() {
  if [ -n "$engine_sudo" ]; then
    as_root "$engine" "$@"
  else
    "$engine" "$@"
  fi
}

# choose_method sets method to container, service or user: the one the flags
# name, or the one the person picks, with the container offered first when an
# engine is installed. Root is offered no user choice: it would be root's.
choose_method() {
  local answer i choices=()
  if [ -n "$method" ]; then
    if [ "$method" = container ] && [ -z "$engine" ]; then
      die "--container needs Docker or Podman, and neither is installed"
    fi
    return 0
  fi
  have_tty || die "no terminal to ask on; pass --container, --service or --user (\`curl ... | bash -s -- --service\`)"
  [ -z "$engine" ] || choices+=(container)
  if has_systemd; then
    choices+=(service)
    [ "$(id -u)" -eq 0 ] || choices+=(user)
  fi
  [ "${#choices[@]}" -gt 0 ] || die "found neither Docker, Podman nor systemd; see $docs/deployments/README.md for the other ways to run HolaClaw"
  if [ "${choices[*]}" = service ]; then
    say "Docker and Podman are not installed. HolaClaw will run as a systemd"
    say "service on this machine, under its own user."
    answer=$(ask "Continue? [Y/n]")
    case "${answer:-y}" in
      y | Y | yes) method=service ;;
      *) die "cancelled" ;;
    esac
    return 0
  fi
  if [ -n "$engine" ]; then
    say "Found $engine. How do you want to run HolaClaw?"
  else
    say "Docker and Podman are not installed. How do you want to run HolaClaw?"
  fi
  for i in "${!choices[@]}"; do
    case "${choices[$i]}" in
      container) say "  $((i + 1))) In a container, with $engine: isolated from this machine (recommended)" ;;
      service) say "  $((i + 1))) As a systemd service on this machine, under its own user, with sudo" ;;
      user) say "  $((i + 1))) As your own user, with no sudo: the agent runs as you, with your home in reach" ;;
    esac
  done
  answer=$(ask "Choice [1]:")
  case "${answer:-1}" in
    [1-9]) [ "${answer:-1}" -gt "${#choices[@]}" ] || method=${choices[$((${answer:-1} - 1))]} ;;
  esac
  [ -n "$method" ] || die "not a choice: $answer"
}

# need_provider fails, before anything changes, when init could not ask for
# what the flags leave out.
need_provider() {
  have_tty && return 0
  [ -n "$provider" ] || die "no terminal for init to ask on; pass --provider openai, openai-codex, openrouter or openai-compat"
  if [ "$provider" = openai-compat ] && { [ -z "$model" ] || [ -z "$base_url" ]; }; then
    die "--provider openai-compat needs --base-url and --model"
  fi
}

# run_init TOKEN CMD... runs CMD init, CMD being a holaclaw of the install,
# with the owner's admin token to the file TOKEN. On a terminal init asks for
# what the flags leave out, and for the API key; without one it gets an empty
# stdin, as under `curl | bash` stdin is the script, and its advice is
# shown only on failure: the summary gives it for this install.
run_init() {
  local token=$1 err args=()
  shift
  [ -z "$provider" ] || args+=(--provider "$provider")
  [ -z "$model" ] || args+=(--model "$model")
  [ -z "$base_url" ] || args+=(--base-url "$base_url")
  if have_tty; then
    "$@" init "${args[@]}" --token-file "$token" </dev/tty || die "init failed"
  elif ! err=$("$@" init "${args[@]}" --token-file "$token" 2>&1 </dev/null >/dev/null); then
    die "init failed: $err"
  fi
}

# check_config CMD... runs CMD check --offline. Warnings exit 1, such as the
# browser's BRW005 and BRW006; an API key init could not ask for is an error,
# SEC002, until it is stored.
check_config() {
  local rc=0
  "$@" check --offline || rc=$?
  [ "$rc" -le 1 ] || key_later || die "the config does not pass \`holaclaw check\`"
}

# key_later is true when init had no terminal to ask for the API key on.
key_later() {
  ! have_tty && { [ "$provider" = openai ] || [ "$provider" = openrouter ]; }
}

# next_steps CLI CLI_TTY CONFIG says what is left: the API key init could not
# ask for, the ChatGPT sign-in, which needs the daemon, the chat, and how to
# pair a device with the apps, which init's output only says on failure. CLI is
# the install's holaclaw command, CLI_TTY the same on a terminal, and CONFIG
# its config.toml.
next_steps() {
  local name=$provider
  [ "$name" != openai-codex ] || name=codex
  if key_later; then
    say "Store the API key, which init had no terminal to ask for:"
    say "  read -rs KEY && printf %s \"\$KEY\" | $1 auth set providers.$name api_key; unset KEY"
    say ""
  fi
  if [ "$provider" = openai-codex ] || { [ -z "$provider" ] && grep -q '^type *= *"codex"' <<<"$3"; }; then
    say "Sign in with your ChatGPT account:  $2 login codex"
    say ""
  fi
  say "Then chat:  $2 chat"
  say ""
  say "To sign in to the apps, pair each device with a one-shot code, now or later."
  say "Take the owner's id from \`$1 identity list\`, then:"
  say "  $1 invite create --identity <owner id> --channel holaclaw"
}

# choose_browser sets browser to yes or no: the one the flags name, or the
# one the person picks. The default is the container's yes, since the image
# holds Chromium, and the service's no, since it installs Chromium here.
choose_browser() {
  local answer def=n hint="[y/N]"
  [ -z "$browser" ] || return 0
  if [ "$method" = container ]; then
    def=y hint="[Y/n]"
  fi
  if ! have_tty; then
    browser=$def
    return 0
  fi
  say "The browser lets the agent open web pages, in Chromium."
  case "$method" in
    container)
      say "Its sandbox needs the container to run with --security-opt seccomp=unconfined."
      say "Without it, the container runs the slim image, which has no Chromium."
      ;;
    service) say "It installs Chromium and Xvfb, which run as the user holaclaw-browser." ;;
    user)
      say "Chromium and Xvfb must be installed already. They run as you, so a page"
      say "that breaks out of Chromium's sandbox reaches your secrets: use it to"
      say "try the browser out."
      ;;
  esac
  answer=$(ask "Set up the browser? $hint")
  case "${answer:-$def}" in
    y | Y | yes) browser=y ;;
    n | N | no) browser=n ;;
    *) die "not a choice: $answer" ;;
  esac
}

# install_container follows docs/deployments/container.md: run it, start at boot, and
# init inside it, after which the daemon starts by itself.
install_container() {
  local show="${engine_sudo:+sudo }$engine" i img=$image unit=podman-restart.service opts=() it=()
  [ -z "$engine_sudo" ] || need_root
  if ctr container inspect holaclaw >/dev/null 2>&1; then
    die "a container named holaclaw exists already; to upgrade it, see $docs/deployments/container.md#upgrade"
  fi
  for i in config secrets data; do
    if ctr volume inspect "holaclaw-$i" >/dev/null 2>&1; then
      die "the volume holaclaw-$i of an earlier installation exists; to reuse it, see $docs/deployments/container.md#upgrade, and to start over, $docs/deployments/container.md#remove-it"
    fi
  done

  # Chromium's sandbox needs user namespaces, which the engine's default
  # seccomp profile blocks; the slim image has no Chromium. host.docker.internal
  # reaches a model server on the host, such as Ollama or llama-server.
  if [ "$browser" = y ]; then
    opts=(--security-opt seccomp=unconfined)
  else
    img=${image%:*}:slim
  fi
  ctr pull "$img"
  ctr run -d --name holaclaw --restart always \
    -v holaclaw-config:/etc/holaclaw \
    -v holaclaw-secrets:/etc/holaclaw-secrets \
    -v holaclaw-data:/var/lib/holaclaw \
    -p 127.0.0.1:8484:8484 \
    "${opts[@]}" \
    --add-host host.docker.internal:host-gateway \
    "$img" >/dev/null

  # Start at boot: rootless, the user's podman-restart.service or docker.service,
  # and lingering, which starts the user's services without a login. A user
  # may linger themselves only through polkit, which Debian does not install.
  if [ -n "$rootless" ]; then
    [ "$engine" = podman ] || unit=docker.service
    if ! systemctl --user enable --quiet "$unit" 2>/dev/null; then
      say "warning: the container will not start at boot until you run \`systemctl --user enable $unit\`" >&2
    fi
    if ! loginctl --no-ask-password enable-linger "$(id -un)" >/dev/null 2>&1; then
      say "warning: the container will not start at boot until root runs \`loginctl enable-linger $(id -un)\`" >&2
    fi
  elif [ "$engine" = podman ] && systemctl cat podman-restart.service >/dev/null 2>&1; then
    as_root systemctl enable --quiet podman-restart.service
  fi

  # The container waits for init before it starts the daemon.
  for i in $(seq 30); do
    ctr exec holaclaw holaclaw version >/dev/null 2>&1 && break
    [ "$i" -eq 30 ] && die "the container did not start; see \`$show logs holaclaw\`"
    sleep 1
  done
  have_tty && it=(-it)
  run_init /etc/holaclaw-secrets/token ctr exec "${it[@]}" holaclaw holaclaw

  say ""
  say "HolaClaw is running in the container holaclaw. Its config, secrets and"
  say "data are in the volumes holaclaw-config, holaclaw-secrets and"
  say "holaclaw-data. The daemon answers on 127.0.0.1:8484, on this machine"
  say "only, with the owner's admin token:"
  say "  $show exec holaclaw cat /etc/holaclaw-secrets/token"
  say ""
  if [ "$browser" = y ]; then
    say "The browser is on. To limit the sites it may open, see"
    say "$docs/browser.md#10-configuration"
    say ""
  fi
  next_steps "$show exec -i holaclaw holaclaw" "$show exec -it holaclaw holaclaw" \
    "$(ctr exec holaclaw cat /etc/holaclaw/config.toml 2>/dev/null || true)"
}

# release_arch sets arch to the release's name for this machine.
release_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "no release for $(uname -m); releases are for amd64 and arm64" ;;
  esac
}

# download FILE... fetches files of the release into a new $tmp and checks
# them against its SHA256SUMS.
download() {
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  (
    cd "$tmp"
    curl -fsSL --remote-name-all "${@/#/$base/}" "$base/SHA256SUMS"
    sha256sum --check --ignore-missing --quiet SHA256SUMS
  )
}

# install_service follows docs/deployments/linux.md, sections 2 to 5.
install_service() {
  local arch files
  need_root
  has_systemd || die "systemd is not running on this machine"
  release_arch
  if as_root test -e /etc/holaclaw/config.toml; then
    die "HolaClaw is installed already in /etc/holaclaw; to upgrade it, see $docs/deployments/linux.md#7-upgrade"
  fi
  if [ "$browser" = y ] && ! command -v apt-get >/dev/null 2>&1 && ! has_chromium; then
    die "the browser needs Chromium and Xvfb, and this script installs them only with apt; install them, or see $docs/browser.md#setting-it-up"
  fi

  files=("holaclaw-linux-$arch" holaclaw.service)
  [ "$browser" = n ] || files+=(holaclaw-browser.service)
  download "${files[@]}"

  as_root install -m 755 "$tmp/holaclaw-linux-$arch" /usr/local/bin/holaclaw
  if ! id holaclaw >/dev/null 2>&1; then
    as_root useradd --system --home-dir /var/lib/holaclaw --shell /usr/sbin/nologin holaclaw
  fi
  as_root install -d -o holaclaw -g holaclaw -m 700 /etc/holaclaw
  run_init /etc/holaclaw/token as_holaclaw holaclaw
  [ "$browser" = n ] || install_browser
  check_config as_holaclaw holaclaw
  as_root install -m 644 "$tmp/holaclaw.service" /etc/systemd/system/holaclaw.service
  as_root systemctl daemon-reload
  as_root systemctl enable --now holaclaw

  say ""
  say "HolaClaw is running as the systemd service holaclaw. Its config is in"
  say "/etc/holaclaw, and the owner's admin token is in /etc/holaclaw/token."
  say ""
  if [ "$browser" = y ]; then
    say "The browser runs as the systemd service holaclaw-browser. To limit the"
    say "sites it may open, see $docs/browser.md#10-configuration"
    say ""
  fi
  next_steps "sudo -u holaclaw holaclaw" "sudo -u holaclaw holaclaw" "$(as_root cat /etc/holaclaw/config.toml)"
}

has_chromium() {
  command -v Xvfb >/dev/null 2>&1 &&
    { command -v chromium >/dev/null 2>&1 || command -v chromium-browser >/dev/null 2>&1; }
}

# install_browser follows docs/browser.md, "Setting it up", for a systemd
# service: the sidecar's user, unit and packages, the [browser] table, with
# the default socket, and the config readable by the sidecar's group.
install_browser() {
  local c=/etc/holaclaw
  if ! has_chromium; then
    as_root apt-get update -qq
    as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq chromium xvfb >/dev/null
  fi
  if ! id holaclaw-browser >/dev/null 2>&1; then
    as_root useradd --system --home-dir /var/lib/holaclaw-browser --shell /usr/sbin/nologin holaclaw-browser
  fi
  as_root usermod -aG holaclaw-browser holaclaw
  printf '\n[browser]\n' | as_holaclaw tee -a $c/config.toml >/dev/null
  as_root chgrp holaclaw-browser $c
  as_root chmod 750 $c
  as_root chgrp -R holaclaw-browser $c/config.toml $c/agents
  as_root chmod -R g+rX $c/config.toml $c/agents
  as_root install -m 644 "$tmp/holaclaw-browser.service" /etc/systemd/system/holaclaw-browser.service
  as_root systemctl daemon-reload
  as_root systemctl enable --now --quiet holaclaw-browser
}

# install_user follows docs/deployments/linux.md, section 9, with systemd user
# units in place of its terminals: everything in the person's home, nothing
# through sudo. The units name the config directory, so they do not rest on
# the CLI's defaults; the socket is written out, as TOML does not expand
# variables.
install_user() {
  local arch bin="$HOME/.local/bin" conf="${XDG_CONFIG_HOME:-$HOME/.config}" cfg units run chat=holaclaw
  cfg=$conf/holaclaw
  units=$conf/systemd/user
  [ "$(id -u)" -ne 0 ] || die "--user is for a user other than root; as root, pass --service or --container"
  has_systemd || die "systemd is not running on this machine"
  run=${XDG_RUNTIME_DIR:-}
  if [ -z "$run" ] || ! systemctl --user show-environment >/dev/null 2>&1; then
    die "no systemd user manager for $(id -un); run this in a login of your own, such as over SSH, not under su or sudo"
  fi
  release_arch
  if [ -e "$cfg" ]; then
    die "HolaClaw is installed already in $cfg; to upgrade it, see $docs/deployments/linux.md#9-as-your-own-user"
  fi
  if [ "$browser" = y ] && ! has_chromium; then
    die "the browser needs Chromium and Xvfb, which this script installs only with sudo; install them, or see $docs/browser.md#setting-it-up"
  fi

  download "holaclaw-linux-$arch"
  install -Dm755 "$tmp/holaclaw-linux-$arch" "$bin/holaclaw"
  run_init "$cfg/token" env HOLACLAW_CONFIG="$cfg" "$bin/holaclaw"
  sed -i "/^\[server\]/a socket = \"$run/holaclaw.sock\"" "$cfg/config.toml"
  if [ "$browser" = y ]; then
    printf '\n[browser]\nsocket = "%s/holaclaw-browser.sock"\n' "$run" >>"$cfg/config.toml"
  fi
  check_config env HOLACLAW_CONFIG="$cfg" "$bin/holaclaw"

  mkdir -p "$units"
  cat >"$units/holaclaw.service" <<UNIT
# HolaClaw as your own user, written by install.sh. The agent, and any
# command its exec tool runs, runs as you.
[Unit]
Description=holaclaw agent harness

[Service]
Environment=HOLACLAW_CONFIG=$cfg
ExecStart=$bin/holaclaw run
Restart=on-failure
RestartSec=3
# Input interrupted by a stop is re-queued and replayed on start.
TimeoutStopSec=20
UMask=0077

[Install]
WantedBy=default.target
UNIT
  if [ "$browser" = y ]; then
    cat >"$units/holaclaw-browser.service" <<UNIT
# The HolaClaw browser sidecar as your own user, written by install.sh.
[Unit]
Description=holaclaw browser sidecar (Chromium)
Before=holaclaw.service

[Service]
Environment=HOLACLAW_CONFIG=$cfg
ExecStart=$bin/holaclaw browser
Restart=on-failure
RestartSec=3
TimeoutStopSec=20
# Profiles and the download quarantine, in ~/.local/state/holaclaw-browser.
StateDirectory=holaclaw-browser
UMask=0077

[Install]
WantedBy=default.target
UNIT
  fi
  systemctl --user daemon-reload
  [ "$browser" = n ] || systemctl --user enable --now --quiet holaclaw-browser
  systemctl --user enable --now --quiet holaclaw

  # Start at boot: lingering starts the user's services without a login. A
  # user may linger themselves only through polkit, which Debian does not
  # install.
  if ! loginctl --no-ask-password enable-linger "$(id -un)" >/dev/null 2>&1; then
    say "warning: HolaClaw will not start at boot until root runs \`loginctl enable-linger $(id -un)\`" >&2
  fi
  case ":$PATH:" in
    *":$bin:"*) ;;
    *) say "warning: $bin is not on your PATH; Debian adds it at your next login, or run \`export PATH=\"\$HOME/.local/bin:\$PATH\"\`" >&2 ;;
  esac
  # With a system install in /etc/holaclaw, that is the CLI's default.
  [ ! -e /etc/holaclaw ] || chat="HOLACLAW_CONFIG=$cfg holaclaw"

  say ""
  say "HolaClaw is running as your systemd user service holaclaw. Its config is"
  say "in $cfg, and the owner's admin token is in $cfg/token."
  say ""
  if [ "$browser" = y ]; then
    say "The browser runs as your user service holaclaw-browser. To limit the"
    say "sites it may open, see $docs/browser.md#10-configuration"
    say ""
  fi
  next_steps "$chat" "$chat" "$(cat "$cfg/config.toml")"
}

main() {
  method=""
  browser=""
  provider=""
  model=""
  base_url=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --container) method=container ;;
      --service) method=service ;;
      --user) method=user ;;
      --browser) browser=y ;;
      --no-browser) browser=n ;;
      --provider | --model | --base-url)
        [ $# -ge 2 ] || die "$1 needs a value"
        case "$1" in
          --provider) provider=$2 ;;
          --model) model=$2 ;;
          --base-url) base_url=$2 ;;
        esac
        shift
        ;;
      -h | --help)
        say "usage: install.sh [--container | --service | --user] [--browser | --no-browser]"
        say "                  [--provider openai|openai-codex|openrouter|openai-compat] [--model ID] [--base-url URL]"
        return 0
        ;;
      *) die "unknown argument: $1" ;;
    esac
    shift
  done
  [ "$(uname -s)" = Linux ] || die "HolaClaw runs on Linux only"
  browser_arg=$browser

  detect_engine
  choose_method
  choose_browser
  need_provider
  case "$method" in
    container) install_container ;;
    service) install_service ;;
    user) install_user ;;
  esac
}

# Everything is in functions, so a download cut short runs nothing.
main "$@"
