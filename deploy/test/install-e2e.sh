#!/usr/bin/env bash
# End-to-end tests of the installation: each scenario boots a fresh Debian
# machine (a privileged container with systemd, and Docker or Podman inside)
# and, as some user, runs deploy/install.sh the way the quickstart does, or
# the step-by-step guides' own code blocks, taken from docs/ as they are
# written. Then it checks what was installed. The release is served
# locally: the images from `nix build .#container` and `.#container-slim`
# through a registry, the binary and units through an HTTP server, and
# install.sh and the guides pointed at both. Nothing reaches a model: the
# API key is a placeholder, and `chat` and `login codex` are left out.
#
#   nix develop -c just install-e2e                       # every scenario
#   nix develop -c just install-e2e docker-root service-sudo
#   KEEP=1 ...            keep the machines: docker exec -it hc-e2e-<scenario> bash
#   JOBS=1 ...            one scenario at a time (default 4)
#   HOLACLAW_IMAGE=<ref>  test an image already in Docker instead of building one
#   HOLACLAW_SLIM_IMAGE=<ref>  the same, for the slim image
#
# Run it in the dev shell, which has the tools. It needs a rootful Docker
# daemon, at E2E_DOCKER_HOST (default the system's socket): the machines run
# --privileged, and rootless Podman inside them needs subordinate ids a
# rootless Docker does not have.
# Users on every machine: alice (sudo without a password, and in the docker
# group where there is one), bob (docker group, no sudo), carol (sudo with
# the password "secret") and dave (nothing).
# The steps in single quotes run on the machines, where they expand.
# shellcheck disable=SC2016
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
here="$root/deploy/test"
label=holaclaw-e2e
net=hc-e2e
jobs=${JOBS:-4}
export DOCKER_HOST=${E2E_DOCKER_HOST:-unix:///var/run/docker.sock}

scenarios=(
  docker-root docker-group docker-sudo docker-unprivileged docker-rootless
  docker-service docker-nosystemd
  podman-root podman-rootless podman-rootless-polkit podman-rootless-sudoer
  service-root service-sudo service-noapt service-cancel service-nosudo
  user user-fallback user-browser
  nothing
  guide-service guide-user guide-user-service
  guide-container-root guide-container-user guide-container-dind guide-container-docker-rootless
  guide-container-podman-root guide-container-podman-rootless guide-container-build
  guide-browser-service guide-browser-user
)

# --- Harness ------------------------------------------------------------------

work=""
fails=0

cleanup() {
  [ -n "$work" ] && rm -rf "$work"
  [ -n "${KEEP:-}" ] && return 0
  docker ps -aq --filter "label=$label" | xargs -r docker rm -f >/dev/null
  docker network rm "$net" >/dev/null 2>&1 || true
}

fail() {
  printf 'FAIL: %s\n' "$*"
  fails=$((fails + 1))
}
pass() { printf 'ok: %s\n' "$*"; }

# x runs a shell command on the machine as root.
x() { docker exec "$m" bash -c "$1"; }

# check DESC CMD passes when CMD succeeds on the machine.
check() {
  if x "$2" >/dev/null 2>&1; then pass "$1"; else fail "$1"; fi
}

# wait_for DESC CMD retries CMD for two minutes.
wait_for() {
  local i
  for i in $(seq 60); do
    if x "$2" >/dev/null 2>&1; then
      pass "$1"
      return 0
    fi
    sleep 2
  done
  fail "$1"
}

wait_systemd() {
  local i s=""
  for i in $(seq 60); do
    s=$(docker exec "$m" systemctl is-system-running 2>/dev/null || true)
    case "$s" in running | degraded) return 0 ;; esac
    sleep 1
  done
  fail "systemd did not boot: $s"
  return 1
}

# machine TARGET [nosystemd] boots this scenario's machine from a target of
# deploy/test/machines.Dockerfile and adds the users.
machine() {
  m="hc-e2e-$scenario"
  docker rm -f "$m" >/dev/null 2>&1 || true
  local args=(-d --name "$m" --hostname "$m" --label "$label" --network "$net"
    --privileged --cgroupns=private -v /var/lib/docker -v /var/lib/containers -v /home)
  if [ "${2:-}" = nosystemd ]; then
    docker run "${args[@]}" "hc-e2e-$1" sleep infinity >/dev/null
  else
    docker run "${args[@]}" --tmpfs /run:exec --tmpfs /run/lock "hc-e2e-$1" >/dev/null
    wait_systemd
  fi
  x '
    useradd -m -s /bin/bash -G sudo alice
    useradd -m -s /bin/bash bob
    useradd -m -s /bin/bash -G sudo carol
    useradd -m -s /bin/bash dave
    echo carol:secret | chpasswd
    echo "alice ALL=(ALL) NOPASSWD: ALL" > /tmp/alice
    install -m 440 /tmp/alice /etc/sudoers.d/alice
    if getent group docker >/dev/null; then usermod -aG docker alice; usermod -aG docker bob; fi'
}

reboot() {
  docker restart "$m" >/dev/null
  wait_systemd
}

# inst USER ARGS... runs the quickstart's one-liner as USER, with no terminal;
# inst_tty USER PROMPT ANSWER ... -- ARGS... on one, answering each prompt.
# Both leave the output in $out and the status in $rc.
inst() {
  local user=$1
  shift
  run_inst "$user" "$*"
}
inst_tty() {
  local user=$1 pairs=()
  shift
  while [ "$1" != -- ]; do
    pairs+=("$1")
    shift
  done
  shift
  # runuser -c detaches from the terminal unless it opens one of its own.
  run_inst "$user" "$*" drive "${pairs[@]}" -- runuser --pty
}
run_inst() {
  local user=$1 flags=$2
  shift 2
  [ $# -gt 0 ] || set -- runuser
  printf '$ %s: curl ... | bash -s -- %s\n' "$user" "$flags"
  rc=0
  # A login, as over SSH: a logind session, and the umask of one.
  out=$(docker exec "$m" "$@" -l "$user" -c \
    "umask 022; curl -fsSL http://assets/install.sh | bash -s -- $flags" 2>&1) || rc=$?
  printf '%s\n[exit %s]\n' "$out" "$rc" | sed 's/^/  | /'
}

expect_rc() {
  if [ "$rc" -eq "$1" ]; then pass "exit $1"; else fail "exit $rc, want $1"; fi
}
expect_out() {
  if grep -qF -- "$1" <<<"$out"; then pass "says \"$1\""; else fail "does not say \"$1\""; fi
}
expect_no_out() {
  if grep -qF -- "$1" <<<"$out"; then fail "says \"$1\""; else pass "does not say \"$1\""; fi
}

# as USER CMD runs CMD on the machine in a login of USER.
as() { printf 'runuser -l %s -c %q' "$1" "$2"; }

# ctr_as ENGINE USER is the engine's command, run as USER for a rootless
# engine of theirs (with no user, root's).
ctr_as() {
  if [ -z "${2:-}" ]; then
    printf '%s' "$1"
  else
    printf 'runuser -u %s -- env XDG_RUNTIME_DIR=/run/user/$(id -u %s) DOCKER_HOST=unix:///run/user/$(id -u %s)/docker.sock %s' \
      "$2" "$2" "$2" "$1"
  fi
}

# container_ok ENGINE [ROOTLESS-USER] checks a container as
# docs/deployments/container.md runs it, after init: the owner's token in the secrets
# volume, and the daemon on the host's loopback, port 8484.
container_ok() {
  local ctr
  ctr=$(ctr_as "$1" "${2:-}")
  wait_for "the container runs" "$ctr inspect -f '{{.State.Running}}' holaclaw | grep -qx true"
  check "restart policy always" "$ctr inspect -f '{{.HostConfig.RestartPolicy.Name}}' holaclaw | grep -qx always"
  check "the state is in the three volumes" "$ctr volume inspect holaclaw-config holaclaw-secrets holaclaw-data"
  wait_for "the daemon answers" "$ctr exec holaclaw holaclaw status"
  check "init wrote a provider" "$ctr exec holaclaw cat /etc/holaclaw/config.toml | grep -q '^\[providers\.'"
  check "the token is in the secrets volume, one line" \
    "[ \"\$($ctr exec holaclaw cat /etc/holaclaw-secrets/token | wc -l)\" = 1 ]"
  wait_for "the API answers on 127.0.0.1:8484 with the token" \
    "curl -fsS -o /dev/null -H \"Authorization: Bearer \$($ctr exec holaclaw cat /etc/holaclaw-secrets/token)\" http://127.0.0.1:8484/v1/agents"
}

# container_browser ENGINE on|off [ROOTLESS-USER] checks the browser of the
# container: on, with the seccomp flag its sandbox needs and the sidecar's
# socket; off, the slim image with neither, and no browser finding.
container_browser() {
  local ctr
  ctr=$(ctr_as "$1" "${3:-}")
  if [ "$2" = on ]; then
    check "seccomp is unconfined" "$ctr inspect -f '{{.HostConfig.SecurityOpt}}' holaclaw | grep -q seccomp=unconfined"
    wait_for "the sidecar's socket is there" "$ctr exec holaclaw test -S /run/holaclaw-browser/browser.sock"
    check "the browser starts" "! $ctr exec holaclaw holaclaw check --offline 2>&1 | grep -E '^BRW00[1-4]'"
  else
    check "the slim image" "$ctr inspect -f '{{.Config.Image}}' holaclaw | grep -q ':slim\$'"
    check "the default seccomp profile" "! $ctr inspect -f '{{.HostConfig.SecurityOpt}}' holaclaw | grep -q seccomp"
    check "no sidecar" "! $ctr exec holaclaw test -e /run/holaclaw-browser/browser.sock"
    check "no browser finding" "! $ctr exec holaclaw holaclaw check --offline 2>&1 | grep -E '^BRW'"
  fi
}

# rootless_docker USER gives USER a rootless Docker, which their logins use,
# and waits until it answers.
rootless_docker() {
  docker exec -i "$m" runuser -l "$1" -c 'bash -s' <<'EOF' >/dev/null
set -e
umask 022
mkdir -p ~/.config/docker
echo '{"insecure-registries":["registry:5000"]}' >~/.config/docker/daemon.json
PATH=/usr/share/docker.io/contrib:$PATH dockerd-rootless-setuptool.sh install --skip-iptables
echo "export DOCKER_HOST=unix:///run/user/$(id -u)/docker.sock" >>~/.profile
EOF
  wait_for "$1's rootless Docker answers" "$(as "$1" 'docker info')"
}

# service_ok [warnings] checks the service; with warnings, check may warn
# (exit 1) but not fail.
service_ok() {
  local ok=""
  [ -z "${1:-}" ] || ok=' || [ $? -eq 1 ]'
  wait_for "holaclaw.service is active" "systemctl is-active holaclaw"
  check "holaclaw.service is enabled" "systemctl is-enabled holaclaw"
  check "the token is holaclaw's, mode 600" "[ \"\$(stat -c '%U %a' /etc/holaclaw/token)\" = 'holaclaw 600' ]"
  check "holaclaw check passes" "runuser -u holaclaw -- env HOLACLAW_CONFIG=/etc/holaclaw holaclaw check --offline$ok"
}

# service_browser checks the sidecar of a service installed with the browser.
service_browser() {
  wait_for "holaclaw-browser.service is active" "systemctl is-active holaclaw-browser"
  check "holaclaw-browser.service is enabled" "systemctl is-enabled holaclaw-browser"
  check "the sidecar's socket is there" "[ -S /run/holaclaw-browser/browser.sock ]"
  check "the browser starts" "! runuser -u holaclaw -- env HOLACLAW_CONFIG=/etc/holaclaw holaclaw check --offline 2>&1 | grep -E '^BRW00[1-4]'"
  # The fonts of browser.md: none of these falls back to DejaVu.
  check "Arial is Liberation Sans" "runuser -u holaclaw-browser -- fc-match Arial family | grep -q '^Liberation Sans'"
  check "CJK is Noto Sans CJK" "runuser -u holaclaw-browser -- fc-match :lang=ja family | grep -q '^Noto Sans CJK'"
  check "emoji is Noto Color Emoji" "runuser -u holaclaw-browser -- fc-match emoji family | grep -q '^Noto Color Emoji'"
  check "the secrets and the data stay the daemon's" \
    "[ \"\$(stat -c %a /etc/holaclaw/data)\" = 700 ] && [ \"\$(stat -c %a /etc/holaclaw/token)\" = 600 ]"
}

# user_ok USER checks the user services of an install as USER, and that the
# CLI finds the daemon in a login with no setup.
user_ok() {
  local h
  h=$(x "getent passwd $1 | cut -d: -f6")
  wait_for "$1's holaclaw.service is active" "$(as "$1" 'systemctl --user is-active holaclaw')"
  check "$1's holaclaw.service is enabled" "$(as "$1" 'systemctl --user is-enabled holaclaw')"
  check "the token is $1's, mode 600" "[ \"\$(stat -c '%U %a' $h/.config/holaclaw/token)\" = '$1 600' ]"
  check "the socket is written out" "grep -qx \"socket = \\\"/run/user/\$(id -u $1)/holaclaw.sock\\\"\" $h/.config/holaclaw/config.toml"
  wait_for "holaclaw status, with no setup" "$(as "$1" 'holaclaw status')"
}

# --- Following the guides -----------------------------------------------------

# doc FILE HEADING N prints the Nth fenced block under HEADING in docs/FILE,
# pointed at the local release.
doc() {
  awk -v h="$2" -v n="$3" '
    $0 == h { s = 1; lvl = index(h, " "); next }
    s && !f && /^#/ && index($0, " ") <= lvl { exit }
    s && /^```/ { if (f) { f = 0; if (++k == n) exit } else f = 1; next }
    s && f && k + 1 == n { print }
  ' "$root/docs/$1" |
    sed -e 's|https://github.com/ikitana/holaclaw/releases/latest/download|http://assets|g' \
      -e 's|ghcr.io/ikitana/holaclaw:latest|registry:5000/holaclaw:latest|g'
}

# add FILE HEADING N [SED] adds a block of a guide to the steps, through the
# sed script SED when given (to leave out what needs a model or a person);
# inline FILE TEXT adds a command the guide gives in its prose; ours CMD adds
# a step of the test's own, such as moving to a new host.
steps=""
add() {
  local block
  block=$(doc "$1" "$2" "$3")
  if [ -z "$block" ]; then
    fail "docs/$1 has no block $3 under \"$2\""
    return 1
  fi
  [ -z "${4:-}" ] || block=$(sed -E -e "$4" <<<"$block")
  [ -z "$as_engine" ] || block=$(sed -E -e "$as_engine" <<<"$block")
  steps+="# docs/$1, $2, block $3${4:+, through sed '$4'}"$'\n'"$block"$'\n'
}
inline() {
  if ! grep -qF -- "$2" "$root/docs/$1"; then
    fail "docs/$1 no longer says: $2"
    return 1
  fi
  steps+="# docs/$1: $2"$'\n'"$2"$'\n'
}
ours() {
  local step=$1
  [ -z "$as_engine" ] || step=$(sed -E -e "$as_engine" <<<"$step")
  steps+="$step  # the test's own step"$'\n'
}

# follow USER [INPUT] runs the steps in one login of USER, as pasted into a
# terminal, stopping at the first that fails, with INPUT on stdin. The
# editor the guides open makes the edit section 3 of getting-started.md
# describes (e2e-edit, from guide_machine).
follow() {
  local user=$1 input=${2:-}
  printf '$ %s follows the guides:\n' "$user"
  # shellcheck disable=SC2001
  sed 's/^/  > /' <<<"$steps"
  docker exec -i "$m" sh -c 'cat >/tmp/steps.sh && chmod 644 /tmp/steps.sh' <<<"$steps"
  rc=0
  out=$(printf '%s' "$input" | docker exec -i "$m" runuser -l "$user" -c \
    "umask 022; export EDITOR=e2e-edit SUDO_EDITOR=e2e-edit; bash -ex -o pipefail /tmp/steps.sh" 2>&1) || rc=$?
  printf '%s\n[exit %s]\n' "$out" "$rc" | sed 's/^/  | /'
  steps=""
}

# guide_machine TARGET [nosystemd] is machine, plus e2e-edit: it adds the
# API-key provider of getting-started.md, section 3, to a config.toml and
# points an agent.toml at it, as a person would in their editor.
guide_machine() {
  local snippet
  machine "$@"
  snippet=$(doc getting-started.md "### An OpenAI-compatible endpoint with an API key" 1 |
    sed 's/<a model id of that endpoint>/e2e-model/')
  [ -n "$snippet" ] || fail "getting-started.md has no API-key provider snippet"
  docker exec -i "$m" sh -c 'mkdir -p /etc/e2e && cat >/etc/e2e/provider.toml' <<<"$snippet"
  x 'cat >/usr/local/bin/e2e-edit <<"EOF"
#!/bin/sh
for f; do
  case "$(basename "$f")" in
    config*) printf "\n" >>"$f"; cat /etc/e2e/provider.toml >>"$f" ;;
    agent*) sed -i "s/^chat *= *\"default\"/chat = \"chatgpt\"/" "$f" ;;
  esac
  echo "# edited by the e2e test" >>"$f"
done
EOF
chmod 755 /usr/local/bin/e2e-edit'
}

# container_guide ENGINE USER [rootless] follows docs/deployments/container.md as USER,
# with ENGINE in place of docker, in the directory a login starts in: run,
# init, edit the config, back up, restore on a new host, upgrade, remove;
# and start at boot, unless there is no systemd. `chat` and `login codex`
# need a model and a person, so they are left out. Ollama's init needs no
# key, so the one on stdin goes to the edit's `auth set`.
container_guide() {
  local engine=$1 user=$2 rootless=${3:-} c=deployments/container.md boot=""
  [ "$engine" = docker ] || as_engine='s/(^|[^.[:alnum:]-])docker([[:space:]])/\1podman\2/g'
  add $c "## Run it" 1
  ours "$up"
  # The test reads the token from the secrets volume, and has no endpoint
  # for the online check.
  add $c "### Any OpenAI-compatible endpoint" 1 's/exec -it /exec /; /holaclaw chat/d; /holaclaw check$/d; s|--model gemma4:e2b$|& --token-file /etc/holaclaw-secrets/token|'
  ours "$ready"
  add $c "### After init" 3
  # The image turns the browser on, which warns BRW005 and BRW006, as the
  # guide says: check exits 1 on warnings, 2 on errors.
  add $c "## Edit the config by hand" 1 's/check --offline$/check --offline || [ $? -eq 1 ]/'
  ours '! docker exec holaclaw holaclaw check --offline 2>&1 | grep -E "^BRW00[1-4]"'
  ours "$ready"
  add $c "## Backups" 1 '2,$d; s/$/ | tee created/'
  ours 'id=$(awk '"'"'$1 == "id" {print $2}'"'"' created)'
  add $c "## Backups" 1 '1d; s/<id>/$id/g'
  # A new host: empty volumes, and the archive.
  ours 'docker rm -f holaclaw && docker volume rm holaclaw-config holaclaw-secrets holaclaw-data'
  add $c "## Backups" 2 '/^# /,$d; s/<id>/$id/g'
  add $c "## Run it" 1
  add $c "## Backups" 2 '1,/^# /d'
  ours "$ready"
  ours '! docker exec holaclaw holaclaw init --provider openrouter --token-file /tmp/token'
  # The image has no grep: read the files out.
  ours 'docker exec holaclaw cat /etc/holaclaw/config.toml | grep -q "edited by the e2e test"'
  ours 'docker exec holaclaw cat /etc/holaclaw-secrets/auth.toml | grep -q api_key'
  add $c "## Upgrade" 1
  add $c "## Run it" 1
  ours "$ready"
  case "$engine/$user/$rootless" in
    docker/*/rootless) boot='/enable-linger/!d'; ours 'systemctl --user enable docker' ;;
    docker/*) boot='/# Docker$/!d' ;;
    podman/root/) boot='/Podman as root/!d' ;;
    podman/*) boot='/Podman rootless|enable-linger/!d' ;;
  esac
  [ -z "$no_boot" ] && add $c "## Start at boot" 1 "$boot"
  follow "$user" $'sk-e2e\n'
  expect_rc 0
  container_ok "$engine" "${rootless:+$user}"
  if [ -z "$no_boot" ]; then
    reboot
    container_ok "$engine" "${rootless:+$user}"
  fi
  add $c "## Remove it" 1
  ours '! docker volume inspect holaclaw-data'
  follow "$user"
  expect_rc 0
  as_engine=""
}
as_engine=""
no_boot=""
# A person runs `init` once the container is up, and `status` once the
# daemon has had a few seconds to start.
up='for i in $(seq 30); do docker exec holaclaw holaclaw version >/dev/null 2>&1 && break; sleep 1; done'
ready='for i in $(seq 30); do docker exec holaclaw holaclaw status >/dev/null 2>&1 && break; sleep 1; done'
# init's own questions on a terminal, after install.sh's, for --provider
# openrouter: the model, kept, then the API key.
init_tty=("deepseek/deepseek-v4.1-flash" "" "API key" sk-e2e)

# --- Scenarios ----------------------------------------------------------------

# The quickstart as root, taking the defaults, init's included. Without a
# terminal, init needs its flags.
sc_docker_root() {
  machine docker
  inst_tty root "Choice [1]:" "" "Set up the browser? [Y/n]" "" "${init_tty[@]}" -- --provider openrouter
  expect_rc 0
  expect_out "2) As a systemd service"
  expect_out "Then chat:  docker exec -it holaclaw holaclaw chat"
  expect_no_out "Store the API key"
  expect_no_out "Next, pick a model"
  container_ok docker
  container_browser docker on
  check "host.docker.internal resolves" "docker inspect -f '{{.HostConfig.ExtraHosts}}' holaclaw | grep -q host.docker.internal:host-gateway"
  check "the API key is stored" "docker exec holaclaw cat /etc/holaclaw-secrets/auth.toml | grep -q api_key"

  inst root --container --provider openrouter
  expect_rc 1
  expect_out "a container named holaclaw exists already"
  inst root
  expect_rc 1
  expect_out "no terminal to ask on"
  inst root --container
  expect_rc 1
  expect_out "pass --provider"
  inst root --container --provider openai-compat --model m
  expect_rc 1
  expect_out "--provider openai-compat needs --base-url and --model"
  inst root --container --provider
  expect_rc 1
  expect_out "--provider needs a value"
  inst root --bogus
  expect_rc 1
  expect_out "unknown argument: --bogus"

  reboot
  container_ok docker
  container_browser docker on

  x 'docker rm -f holaclaw'
  inst root --container --provider openrouter
  expect_rc 1
  expect_out "the volume holaclaw-config of an earlier installation exists"
}

# In the docker group, with no sudo: nothing needs root. Without a terminal,
# the container has the browser, unless told otherwise, and the API key is
# left for the command the installer prints.
sc_docker_group() {
  machine docker
  inst bob --container --provider openrouter
  expect_rc 0
  expect_out "Then chat:  docker exec"
  expect_out "| docker exec -i holaclaw holaclaw auth set providers.openrouter api_key"
  container_ok docker
  container_browser docker on
  check "the key it says to store is missing" "docker exec holaclaw holaclaw check --offline 2>&1 | grep -q '^SEC002'"
  x 'docker rm -f holaclaw && docker volume rm holaclaw-config holaclaw-secrets holaclaw-data'
  inst bob --container --no-browser --provider openrouter
  expect_rc 0
  expect_no_out "The browser is on"
  container_ok docker
  container_browser docker off
}

# Not in the docker group: Docker through sudo, which asks for the password
# once. No browser, so the slim image and no seccomp flag.
sc_docker_sudo() {
  machine docker
  inst_tty carol "Choice [1]:" "" "Set up the browser? [Y/n]" n "password for carol:" secret "${init_tty[@]}" -- --provider openrouter
  expect_rc 0
  expect_out "Then chat:  sudo docker exec"
  container_ok docker
  container_browser docker off
  reboot
  container_ok docker
  container_browser docker off
}

# Neither the docker group nor sudo: refused before anything runs, with no
# terminal to offer the user services on.
sc_docker_unprivileged() {
  machine docker
  inst dave --container --provider openrouter
  expect_rc 1
  expect_out "this needs root or sudo; to install HolaClaw as your own user instead, pass --user"
  check "no container" "! docker container inspect holaclaw"
}

# Rootless Docker: the user's docker.service and lingering start it at boot.
sc_docker_rootless() {
  machine docker-rootless
  rootless_docker bob
  inst_tty bob "Choice [1]:" "" "Set up the browser? [Y/n]" "" "${init_tty[@]}" -- --provider openrouter
  expect_rc 0
  expect_out "until root runs \`loginctl enable-linger bob\`"
  expect_no_out "systemctl --user enable"
  container_ok docker bob
  check "root makes bob linger, as told" "loginctl enable-linger bob"
  reboot
  container_ok docker bob
}

# Docker installed, the service chosen instead.
sc_docker_service() {
  machine docker
  inst_tty root "Choice [1]:" 2 "Set up the browser? [y/N]" "" "${init_tty[@]}" -- --provider openrouter
  expect_rc 0
  expect_out "HolaClaw is running as the systemd service holaclaw"
  service_ok
  check "no container" "! docker container inspect holaclaw"
  check "no sidecar" "! systemctl cat holaclaw-browser"
}

# Docker without systemd, as in a dind container: no service on offer.
sc_docker_nosystemd() {
  machine docker nosystemd
  x 'dockerd >/var/log/dockerd.log 2>&1 &'
  wait_for "dockerd starts" "docker info"
  inst_tty root "Choice [1]:" 2 --
  expect_rc 1
  expect_no_out "2) As a systemd service"
  expect_out "not a choice: 2"
  inst root --user --provider openrouter
  expect_rc 1
  expect_out "--user is for a user other than root"
  inst root --service --provider openrouter
  expect_rc 1
  expect_out "systemd is not running"
  inst root --container --provider openrouter
  expect_rc 0
  container_ok docker
}

# Podman as root: the system's podman-restart.service.
sc_podman_root() {
  machine podman
  inst root --container --no-browser --provider openrouter
  expect_rc 0
  expect_out "Then chat:  podman exec"
  container_ok podman
  container_browser podman off
  check "podman-restart.service is enabled" "systemctl is-enabled podman-restart.service"
  reboot
  container_ok podman
}

# Podman as a user without sudo: rootless, started at boot by the user's
# podman-restart.service and lingering. Without polkit, as on a plain
# Debian, only root may make bob linger, and the installer says so.
sc_podman_rootless() {
  machine podman
  inst_tty bob "Choice [1]:" "" "Set up the browser? [Y/n]" "" "${init_tty[@]}" -- --provider openrouter
  expect_rc 0
  expect_out "until root runs \`loginctl enable-linger bob\`"
  expect_no_out "systemctl --user enable"
  container_ok podman bob
  container_browser podman on bob
  check "no root container" "! podman container inspect holaclaw"
  check "bob's podman-restart.service is enabled" "$(as bob 'systemctl --user is-enabled podman-restart.service')"
  check "root makes bob linger, as told" "loginctl enable-linger bob"
  reboot
  container_ok podman bob
}

# With polkit, bob lingers by himself.
sc_podman_rootless_polkit() {
  machine podman-polkit
  inst bob --container --provider openrouter
  expect_rc 0
  expect_no_out "warning:"
  check "bob lingers" "loginctl show-user bob -p Linger | grep -qx Linger=yes"
  reboot
  container_ok podman bob
}

# Rootless even for a user who could sudo: with no terminal, any sudo fails.
# A ChatGPT subscription: no key, and the sign-in once the daemon runs.
sc_podman_rootless_sudoer() {
  machine podman
  inst carol --container --provider openai-codex
  expect_rc 0
  expect_out "Sign in with your ChatGPT account:  podman exec -it holaclaw holaclaw login codex"
  expect_no_out "Store the API key"
  container_ok podman carol
  check "the codex provider" "$(ctr_as podman carol) exec holaclaw cat /etc/holaclaw/config.toml | grep -qx '\[providers.codex\]'"
}

# No engine: the service, as root, after a yes.
sc_service_root() {
  machine base
  inst_tty root "Continue? [Y/n]" "" "Set up the browser? [y/N]" "" "${init_tty[@]}" -- --provider openrouter
  expect_rc 0
  expect_out "Then chat:  sudo -u holaclaw holaclaw chat"
  service_ok
  inst root --service --provider openrouter
  expect_rc 1
  expect_out "HolaClaw is installed already"
  reboot
  service_ok
}

# With the browser: its packages, user and unit, which come back at boot.
# Not root, so the user services are on offer too, after the service.
sc_service_sudo() {
  machine base
  inst_tty carol "Choice [1]:" "" "Set up the browser? [y/N]" y "password for carol:" secret "${init_tty[@]}" -- --provider openrouter
  expect_out "2) As your own user, with no sudo"
  expect_rc 0
  expect_out "The browser runs as the systemd service holaclaw-browser"
  service_ok warnings
  service_browser
  reboot
  service_ok warnings
  service_browser
}

# Asked for the browser with neither apt nor Chromium: refused first.
sc_service_noapt() {
  machine base
  x 'mv /usr/bin/apt-get /usr/bin/apt-get.off'
  inst root --service --browser --provider openrouter
  expect_rc 1
  expect_out "the browser needs Chromium and Xvfb"
  check "nothing installed" "[ ! -e /usr/local/bin/holaclaw ] && ! id holaclaw"
}

sc_service_cancel() {
  machine base
  inst_tty root "Continue? [Y/n]" n --
  expect_rc 1
  expect_out "cancelled"
  check "nothing installed" "[ ! -e /usr/local/bin/holaclaw ] && ! id holaclaw"
}

sc_service_nosudo() {
  machine base
  inst dave --service --provider openrouter
  expect_rc 1
  expect_out "this needs root or sudo"
  check "nothing installed" "[ ! -e /usr/local/bin/holaclaw ]"
}

# No sudo and no engine: the user services, picked at the prompt, which come
# back at boot once root makes dave linger. Refused as root, over an earlier
# installation, and with the browser but no Chromium, before anything changes.
sc_user() {
  machine base
  inst root --user --provider openrouter
  expect_rc 1
  expect_out "--user is for a user other than root"
  inst dave --user --browser --provider openrouter
  expect_rc 1
  expect_out "the browser needs Chromium and Xvfb"
  check "nothing installed" "[ ! -e /home/dave/.local/bin/holaclaw ] && [ ! -e /home/dave/.config/holaclaw ]"
  inst_tty dave "Choice [1]:" 2 "Set up the browser? [y/N]" "" "${init_tty[@]}" -- --provider openrouter
  expect_rc 0
  expect_out "As your own user, with no sudo: the agent runs as you"
  expect_out "until root runs \`loginctl enable-linger dave\`"
  expect_out "Then chat:  holaclaw chat"
  user_ok dave
  check "no sidecar" "[ ! -e /home/dave/.config/systemd/user/holaclaw-browser.service ]"
  check "holaclaw check passes" "$(as dave 'holaclaw check --offline')"
  x 'sha256sum /home/dave/.config/holaclaw/config.toml >/tmp/config.sum'
  inst dave --user --provider openrouter
  expect_rc 1
  expect_out "HolaClaw is installed already in /home/dave/.config/holaclaw"
  check "the config is as it was" "sha256sum -c /tmp/config.sum"
  check "root makes dave linger, as told" "loginctl enable-linger dave"
  reboot
  user_ok dave
}

# A Docker that dave may not use, on a machine with no sudo at all: the
# container is chosen, and the user services are offered in its place.
sc_user_fallback() {
  machine docker
  x 'mv /usr/bin/sudo /usr/bin/sudo.off'
  inst_tty dave "Choice [1]:" "" "Set up the browser? [Y/n]" n \
    "Install as your own user instead, with no sudo? [Y/n]" "" "Set up the browser? [y/N]" "" \
    "${init_tty[@]}" -- --provider openrouter
  expect_rc 0
  expect_out "HolaClaw is running as your systemd user service holaclaw"
  user_ok dave
  check "no container" "! docker container inspect holaclaw"
}

# The browser as dave's second user service, with Chromium and Xvfb from root.
sc_user_browser() {
  machine base
  x 'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq chromium xvfb >/dev/null'
  inst dave --user --browser --provider openrouter
  expect_rc 0
  expect_out "The browser runs as your user service holaclaw-browser"
  user_ok dave
  wait_for "dave's holaclaw-browser.service is active" "$(as dave 'systemctl --user is-active holaclaw-browser')"
  check "the sidecar's socket is there" "[ -S /run/user/\$(id -u dave)/holaclaw-browser.sock ]"
  check "the browser starts" "! $(as dave 'holaclaw check --offline 2>&1') | grep -E '^BRW00[1-4]'"
}

# Neither an engine nor systemd.
sc_nothing() {
  machine base nosystemd
  inst_tty root --
  expect_rc 1
  expect_out "found neither Docker, Podman nor systemd"
  inst root --container
  expect_rc 1
  expect_out "--container needs Docker or Podman"
  inst root --service --provider openrouter
  expect_rc 1
  expect_out "systemd is not running"
}

# deployments/linux.md, sections 2 to 7, as a user with sudo: the service,
# init for Ollama, an API-key model added by hand, a backup and an upgrade.
# Ollama needs no key: init gets none, and the one on stdin goes to the
# hand edit's `auth set`. Chatting needs a model.
sc_guide_service() {
  local g=deployments/linux.md
  local ready='for i in $(seq 30); do sudo -u holaclaw holaclaw status >/dev/null 2>&1 && break; sleep 1; done'
  guide_machine base
  add $g "## 2. Download and verify" 1
  add $g "## 3. Install the binary and create the service user" 1
  add $g "### Ollama, or another OpenAI-compatible endpoint" 1 's|$| </dev/null|'
  add $g "## 5. Start the service" 1
  ours "$ready"
  add $g "## 5. Start the service" 2
  add $g "## 6. Edit the config" 1
  add $g "## 6. Edit the config" 2
  add $g "## 6. Edit the config" 3
  ours "$ready"
  inline $g "sudo -u holaclaw holaclaw backup create --wait"
  add $g "## 7. Upgrade" 1
  follow alice $'sk-e2e\n'
  expect_rc 0
  expect_out "ok: no findings"
  service_ok
  check "the edit is in the config, still holaclaw's" \
    "grep -q 'edited by the e2e test' /etc/holaclaw/config.toml && [ \"\$(stat -c %U /etc/holaclaw/config.toml)\" = holaclaw ]"
  check "the agent uses the new model" "grep -qx 'chat = \"chatgpt\"' /etc/holaclaw/agents/main/agent.toml"
  check "the key is stored" "grep -q api_key /etc/holaclaw/secrets/auth.toml"
  check "a backup exists" "ls /etc/holaclaw/data/backups/*.tar.gz"
}

# deployments/linux.md, section 9: as a user with no root, the daemon in the
# background in place of a second terminal, on the CLI's own defaults, and
# the API key piped to init.
sc_guide_user() {
  local g=deployments/linux.md s9="## 9. As your own user"
  guide_machine base
  add $g "## 2. Download and verify" 1
  add $g "$s9" 2
  add $g "$s9" 3
  add $g "$s9" 4 's/^holaclaw run$/holaclaw run \&/; /holaclaw chat/d'
  ours 'for i in $(seq 30); do holaclaw status && break; sleep 1; done'
  ours 'kill %1'
  follow bob $'sk-e2e\n'
  expect_rc 0
  expect_out "ok: no findings"
  expect_out "sessions active"
}

# deployments/linux.md, section 9: the installer's user services, with no
# terminal, so the key is left for later.
sc_guide_user_service() {
  guide_machine base
  add deployments/linux.md "## 9. As your own user" 1
  follow dave
  expect_rc 0
  user_ok dave
}

sc_guide_container_root() {
  guide_machine docker
  container_guide docker root
}

# In the docker group, with sudo for the boot step.
sc_guide_container_user() {
  guide_machine docker
  container_guide docker alice
}

# Docker without systemd, as in a dind container.
sc_guide_container_dind() {
  guide_machine docker nosystemd
  x 'dockerd >/var/log/dockerd.log 2>&1 &'
  wait_for "dockerd starts" "docker info"
  no_boot=1
  container_guide docker root
}

sc_guide_container_docker_rootless() {
  guide_machine docker-rootless
  rootless_docker alice
  container_guide docker alice rootless
}

sc_guide_container_podman_root() {
  guide_machine podman
  container_guide podman root
}

sc_guide_container_podman_rootless() {
  guide_machine podman
  container_guide podman alice rootless
}

# The image built from the Dockerfile, run as the guide says.
sc_guide_container_build() {
  local c=deployments/container.md
  guide_machine docker
  git -C "$root" ls-files -co --exclude-standard -z |
    tar -C "$root" --null -T - -cf - | docker exec -i "$m" sh -c 'mkdir -p /src && tar -C /src -xf -'
  ours 'cd /src'
  inline $c 'docker build -t holaclaw .'
  ours 'cd'
  add $c "## Run it" 1 's|registry:5000/holaclaw:latest|holaclaw|'
  ours "$up"
  add $c "### OpenRouter" 1 's/exec -it /exec -i /; /holaclaw chat/d'
  follow root $'sk-e2e\n'
  expect_rc 0
  container_ok docker
  check "the API key is stored" "docker exec holaclaw cat /etc/holaclaw-secrets/auth.toml | grep -q api_key"
}

# browser.md, on a system install made with deployments/linux.md: the
# sidecar's unit and user, then the [browser] table, as the guide says to add
# it. apt's question is answered with -y.
sc_guide_browser_service() {
  local g=deployments/linux.md b=browser.md
  local ready='for i in $(seq 30); do sudo -u holaclaw holaclaw status >/dev/null 2>&1 && break; sleep 1; done'
  guide_machine base
  add $g "## 2. Download and verify" 1
  add $g "## 3. Install the binary and create the service user" 1
  add $g "### OpenRouter" 1
  add $g "## 5. Start the service" 1
  ours "$ready"
  ours 'printf "\n[browser]\n" | sudo tee -a /etc/holaclaw/config.toml >/dev/null'
  ours 'sudo apt-get update -qq'
  add $b "### Setting it up" 1 's/apt install /apt install -y /'
  ours "$ready"
  ours '! sudo -u holaclaw holaclaw check --offline 2>&1 | grep -E "^BRW00[1-4]"'
  follow alice $'sk-e2e\n'
  expect_rc 0
  service_ok warnings
  wait_for "holaclaw-browser.service is active" "systemctl is-active holaclaw-browser"
  check "the sidecar's socket is there" "[ -S /run/holaclaw-browser/browser.sock ]"
  check "the secrets and the data stay the daemon's" \
    "[ \"\$(stat -c %a /etc/holaclaw/data)\" = 700 ] && [ \"\$(stat -c %a /etc/holaclaw/token)\" = 600 ]"
}

# browser.md, as your own user: the sidecar in the background in place of a
# terminal of its own, then the daemon. Chromium and Xvfb come from root.
sc_guide_browser_user() {
  local g=deployments/linux.md b=browser.md s9="## 9. As your own user"
  guide_machine base
  x 'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq chromium xvfb >/dev/null'
  add $g "## 2. Download and verify" 1
  add $g "$s9" 2
  add $g "$s9" 3
  add $b "### Setting it up" 2 '/^/d'
  ours 'printf "\n[browser]\nsocket = \"%s/holaclaw-browser.sock\"\n" "$XDG_RUNTIME_DIR" >>~/.config/holaclaw/config.toml'
  add $b "### Setting it up" 3 's/^holaclaw browser .*/holaclaw browser \&/'
  ours 'for i in $(seq 30); do [ -S "$XDG_RUNTIME_DIR/holaclaw-browser.sock" ] && break; sleep 1; done'
  add $g "$s9" 4 's/^holaclaw check --offline$/holaclaw check --offline || [ $? -eq 1 ]/; s/^holaclaw run$/holaclaw run \&/; /holaclaw chat/d'
  ours '! holaclaw check --offline 2>&1 | grep -E "^BRW00[1-4]"'
  ours 'for i in $(seq 30); do holaclaw status && break; sleep 1; done'
  ours 'kill %1 %2'
  follow bob $'sk-e2e\n'
  expect_rc 0
  expect_out "sessions active"
}

# --- Fixtures -----------------------------------------------------------------

fixtures() {
  local image slim stream assets="$work/assets" port
  mkdir -p "$assets"

  echo "building the release binary and the images"
  CGO_ENABLED=0 go build -C "$root" -trimpath -ldflags "-s -w -X holaclaw/internal/daemon.Version=e2e" \
    -o "$assets/holaclaw-linux-amd64" ./cmd/holaclaw
  cp "$root/deploy/holaclaw.service" "$root/deploy/holaclaw-browser.service" "$assets/"
  sed -e 's|^image=.*|image=registry:5000/holaclaw:latest|' -e 's|^base=.*|base=http://assets|' \
    "$root/deploy/install.sh" >"$assets/install.sh"
  if ! grep -q '^image=registry:5000/' "$assets/install.sh" || ! grep -q '^base=http://assets$' "$assets/install.sh"; then
    echo "install.sh no longer sets image= and base= on lines of their own" >&2
    exit 1
  fi
  (cd "$assets" && sha256sum holaclaw-linux-amd64 holaclaw.service holaclaw-browser.service install.sh >SHA256SUMS)

  image=${HOLACLAW_IMAGE:-}
  if [ -z "$image" ]; then
    stream=$(nix build "$root#container" --no-link --print-out-paths)
    image=$("$stream" | docker load | sed -n 's/^Loaded image: //p')
  fi
  slim=${HOLACLAW_SLIM_IMAGE:-}
  if [ -z "$slim" ]; then
    stream=$(nix build "$root#container-slim" --no-link --print-out-paths)
    slim=$("$stream" | docker load | sed -n 's/^Loaded image: //p')
  fi

  docker network create --label "$label" "$net" >/dev/null
  docker run -d --label "$label" --name hc-e2e-registry --network "$net" --network-alias registry \
    -p 127.0.0.1::5000 registry:2 >/dev/null
  docker run -d --label "$label" --name hc-e2e-assets --network "$net" --network-alias assets \
    -v "$assets:/usr/share/nginx/html:ro" nginx:alpine >/dev/null
  port=$(docker port hc-e2e-registry 5000/tcp | head -n1 | sed 's/.*://')
  sleep 2
  docker tag "$image" "127.0.0.1:$port/holaclaw:latest"
  docker tag "$slim" "127.0.0.1:$port/holaclaw:slim"
  docker push -q "127.0.0.1:$port/holaclaw:latest" >/dev/null
  docker push -q "127.0.0.1:$port/holaclaw:slim" >/dev/null
  docker rmi "127.0.0.1:$port/holaclaw:latest" "127.0.0.1:$port/holaclaw:slim" >/dev/null

  echo "building the machines"
  for target in base docker docker-rootless podman podman-polkit; do
    docker build -q -f "$here/machines.Dockerfile" --target "$target" -t "hc-e2e-$target" "$here" >/dev/null
  done
}

# run_scenario NAME runs one scenario into its log and exits with its failures.
run_scenario() {
  scenario=$1
  fails=0
  trap - EXIT
  "sc_${scenario//-/_}" || fail "the scenario stopped: exit $?"
  [ -n "${KEEP:-}" ] || docker rm -f "hc-e2e-$scenario" >/dev/null 2>&1 || true
  exit "$fails"
}

main() {
  local s logs pids=() names=() failed=() i
  [ $# -gt 0 ] && scenarios=("$@")
  for s in "${scenarios[@]}"; do
    declare -F "sc_${s//-/_}" >/dev/null || { echo "no scenario $s" >&2; exit 2; }
  done

  KEEP="" cleanup
  work=$(mktemp -d)
  logs=$(mktemp -d -t holaclaw-e2e.XXXXXX)
  trap cleanup EXIT
  fixtures

  for s in "${scenarios[@]}"; do
    while [ "$(jobs -rp | wc -l)" -ge "$jobs" ]; do wait -n || true; done
    echo "running $s"
    (run_scenario "$s") >"$logs/$s.log" 2>&1 &
    pids+=($!)
    names+=("$s")
  done
  for i in "${!pids[@]}"; do
    if wait "${pids[$i]}"; then
      printf 'PASS  %s\n' "${names[$i]}"
    else
      printf 'FAIL  %s\n' "${names[$i]}"
      failed+=("${names[$i]}")
    fi
  done

  for s in "${failed[@]}"; do
    printf '\n--- %s\n' "$s"
    grep -E '^(FAIL|\$)' "$logs/$s.log"
  done
  echo
  echo "logs: $logs"
  [ "${#failed[@]}" -eq 0 ]
}

main "$@"
