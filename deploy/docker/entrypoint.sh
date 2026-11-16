#!/bin/sh
# Container entrypoint. On first start it seeds a config with no provider and
# waits, without the daemon, until `holaclaw init` has written one. Then it
# runs the browser sidecar as its own user when the browser is on, and the
# daemon as holaclaw. Any other argument is passed to holaclaw as the holaclaw
# user (e.g. `check`).
set -eu

cfg=/etc/holaclaw
bin=/usr/libexec/holaclaw/holaclaw

# run_as USER CMD... execs CMD as USER, with USER's home.
run_as() {
  u=$1
  shift
  exec setpriv --reuid="$u" --regid="$u" --init-groups env HOME="/var/lib/$u" "$@"
}

# `holaclaw init` replaces this config, keeping its data_dir.
if [ ! -f "$cfg/config.toml" ]; then
  cat >"$cfg/config.toml" <<'EOF'
# holaclaw: no provider yet. Run `holaclaw init --provider ...` to write one.
version = 1

[server]
data_dir = "/var/lib/holaclaw"
EOF
fi
chown -R holaclaw:holaclaw-browser "$cfg"
chmod 750 "$cfg"
chown -R holaclaw:holaclaw /etc/holaclaw-secrets
chmod 700 /etc/holaclaw-secrets
chown -R holaclaw:holaclaw /var/lib/holaclaw

if [ "${1:-run}" != "run" ]; then
  run_as holaclaw "$bin" "$@"
fi

# The daemon needs a provider and an agent: until init has written them, stay
# up and wait, so `--restart always` does not loop. The image has no grep.
ready() {
  [ -f "$cfg/agents/main/agent.toml" ] &&
    [ -n "$(sed -n '/^\[providers\./p' "$cfg/config.toml" 2>/dev/null)" ]
}
if ! ready; then
  echo "holaclaw: no provider yet; waiting for \`holaclaw init --provider ...\` in this container" >&2
  trap 'exit 0' TERM INT
  while ! ready; do
    sleep 1 &
    wait $! || true
  done
  trap - TERM INT
  # Let init finish the files it writes after the agent.
  sleep 1
fi

# The browser: $HOLACLAW_BROWSER_ENABLED (a boolean, as the daemon reads it)
# wins; without it, a [browser] table turns it on unless it says
# `enabled = false`. Never without Chromium, which the slim image lacks.
browser=off
case "${HOLACLAW_BROWSER_ENABLED:-}" in
  1 | t | T | true | TRUE | True) browser=on ;;
  "")
    if [ -n "$(sed -n '/^\[browser\]/p' "$cfg/config.toml")" ] &&
      [ -z "$(sed -n '/^\[browser\]/,/^\[/{/^enabled *= *false/p}' "$cfg/config.toml")" ]; then
      browser=on
    fi
    ;;
esac
command -v chromium >/dev/null 2>&1 || browser=off

if [ "$browser" = off ]; then
  run_as holaclaw "$bin" run
fi

# The sidecar reads the config, which init writes for holaclaw only.
for f in config.toml channels.toml agents; do
  if [ -e "$cfg/$f" ]; then
    chgrp -R holaclaw-browser "$cfg/$f"
    chmod -R g+rX "$cfg/$f"
  fi
done

# The sidecar: its own user, no keys. It refuses to run without Chromium's
# sandbox; Docker's default seccomp profile blocks the user namespaces that
# sandbox needs, hence `--security-opt seccomp=unconfined` in docs/deployments/container.md.
# A stop signal reaches both processes.
stopping=
sidecar=
daemon=
trap 'stopping=1; kill -TERM $daemon $sidecar 2>/dev/null || true' TERM INT
run_as holaclaw-browser env STATE_DIRECTORY=/var/lib/holaclaw-browser XDG_RUNTIME_DIR=/run/holaclaw-browser \
  "$bin" browser &
sidecar=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if [ -S /run/holaclaw-browser/browser.sock ] || [ -n "$stopping" ]; then break; fi
  sleep 0.5
done
status=0
if [ -z "$stopping" ]; then
  run_as holaclaw "$bin" run &
  daemon=$!
  # A signal interrupts wait: wait again until the daemon has stopped.
  wait "$daemon" || status=$?
  while kill -0 "$daemon" 2>/dev/null; do
    status=0
    wait "$daemon" || status=$?
  done
fi
kill -TERM "$sidecar" 2>/dev/null || true
wait "$sidecar" 2>/dev/null || true
exit "$status"
