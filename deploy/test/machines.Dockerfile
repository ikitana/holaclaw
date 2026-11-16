# Not HolaClaw's image (that is the top-level Dockerfile, or nix/container.nix):
# the machines deploy/test/install-e2e.sh installs HolaClaw on, Debian with
# systemd as PID 1 (base), plus Docker (docker) or Podman (podman, and
# podman-polkit with polkit). They pull the image from the runner's registry
# at registry:5000, over plain HTTP.
FROM debian:trixie AS base
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus dbus-user-session sudo curl ca-certificates procps util-linux passwd expect \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask getty.target console-getty.service systemd-firstboot.service
COPY drive /usr/local/bin/drive
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]

FROM base AS docker
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends docker.io docker-cli \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /etc/docker && echo '{"insecure-registries":["registry:5000"]}' > /etc/docker/daemon.json

FROM base AS podman
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      podman uidmap passt slirp4netns fuse-overlayfs catatonit nftables \
 && rm -rf /var/lib/apt/lists/* \
 && printf '[[registry]]\nlocation = "registry:5000"\ninsecure = true\n' > /etc/containers/registries.conf.d/50-e2e.conf

# Lets a user linger by themselves, as on a desktop.
FROM podman AS podman-polkit
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends polkitd \
 && rm -rf /var/lib/apt/lists/*

# Docker rootless only: the system daemon off, rootlesskit for each user's.
# Debian's dockerd-rootless-setuptool.sh ends by running the docker CLI from
# its own directory, where Debian does not put it.
FROM docker AS docker-rootless
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      rootlesskit uidmap slirp4netns iptables iproute2 \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask docker.service docker.socket \
 && ln -s /usr/bin/docker /usr/share/docker.io/contrib/docker
