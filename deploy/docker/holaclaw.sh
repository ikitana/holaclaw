#!/bin/sh
# holaclaw on the image's PATH: the binary, run as the holaclaw user even from
# a root `docker exec`, so what it writes in the volumes stays holaclaw's.
# `docker exec` may start it with umask 0; the files are holaclaw's alone.
umask 077
bin=/usr/libexec/holaclaw/holaclaw
if [ "$(id -u)" = 0 ]; then
  exec setpriv --reuid=holaclaw --regid=holaclaw --init-groups env HOME=/var/lib/holaclaw "$bin" "$@"
fi
exec "$bin" "$@"
