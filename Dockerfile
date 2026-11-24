FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/holaclaw ./cmd/holaclaw

# What both images share. `holaclaw` on PATH is a wrapper that runs the binary
# as the holaclaw user.
FROM debian:bookworm-slim AS base
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --home /var/lib/holaclaw --shell /usr/sbin/nologin holaclaw \
 && useradd --system --home /var/lib/holaclaw-browser --shell /usr/sbin/nologin holaclaw-browser \
 && usermod -aG holaclaw-browser holaclaw \
 && install -d -o holaclaw -g holaclaw -m 700 /var/lib/holaclaw \
 && install -d -o holaclaw-browser -g holaclaw-browser -m 700 /var/lib/holaclaw-browser \
 && install -d -o holaclaw -g holaclaw -m 700 /run/holaclaw \
 && install -d -o holaclaw-browser -g holaclaw-browser -m 750 /run/holaclaw-browser \
 && install -d -o holaclaw -g holaclaw-browser -m 750 /etc/holaclaw \
 && install -d -o holaclaw -g holaclaw -m 700 /etc/holaclaw-secrets
COPY deploy/docker/entrypoint.sh /usr/local/bin/holaclaw-entrypoint
COPY deploy/docker/holaclaw.sh /usr/local/bin/holaclaw
# HOLACLAW_CONTAINER has `holaclaw restart` point at a restart of the container.
ENV HOLACLAW_CONTAINER=1 \
    HOLACLAW_CONFIG=/etc/holaclaw \
    HOLACLAW_LISTEN=0.0.0.0:8484 \
    HOLACLAW_ALLOW_INSECURE_TRANSPORT=true
EXPOSE 8484
VOLUME ["/etc/holaclaw", "/etc/holaclaw-secrets", "/var/lib/holaclaw"]
ENTRYPOINT ["/usr/local/bin/holaclaw-entrypoint"]
CMD ["run"]

# `docker build --target slim .`: no browser.
FROM base AS slim
COPY --from=build /out/holaclaw /usr/libexec/holaclaw/holaclaw

# The default image: Chromium, Xvfb and the browser's fonts (docs/browser.md
# §2), and the browser on.
FROM base
RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium xvfb \
    fonts-dejavu-core fonts-liberation fonts-noto-cjk fonts-noto-color-emoji \
 && rm -rf /var/lib/apt/lists/*
ENV HOLACLAW_BROWSER_ENABLED=true
COPY --from=build /out/holaclaw /usr/libexec/holaclaw/holaclaw
