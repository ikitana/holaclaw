# Run inside the dev shell: `nix develop -c just <recipe>`
default: check

build:
    go build ./...

vet:
    go vet ./...

lint:
    golangci-lint run ./...

test:
    go test ./...

# Skips the slow tests (real Chromium, docgen, benches). CI runs only the
# short variants; run `just test` locally for the full suite.
test-short:
    go test -short ./...

# The race detector needs CGO; CI runs this as a separate job.
test-race:
    CGO_ENABLED=1 go test -race ./...

test-race-short:
    CGO_ENABLED=1 go test -short -race ./...

check: build vet lint test

bin:
    go build -trimpath -ldflags "-s -w" -o bin/holaclaw ./cmd/holaclaw

# Regenerate API types from api/openapi.yaml.
gen:
    oapi-codegen -config internal/api/gen/cfg.yaml api/openapi.yaml

# Fails when generated code is stale.
gen-check: gen
    git diff --exit-code -- internal/api/gen

# Static release binaries (pure Go, no CGO).
release version="dev":
    mkdir -p dist
    for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X holaclaw/internal/daemon.Version={{version}}" \
        -o dist/holaclaw-linux-$arch ./cmd/holaclaw; \
    done

# Regenerate docs/reference from schema and code (ARCHITECTURE.md §7).
docs:
    go run ./tools/docgen --out docs/reference

# Fails on drift between docs/reference and what the generator produces now,
# without touching the working tree.
docs-check:
    #!/usr/bin/env bash
    set -euo pipefail
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT
    go run ./tools/docgen --out "$tmp"
    diff -ru docs/reference "$tmp"

# Replay the recorded guard evals offline (no network, no key).
eval-guard:
    go run ./tools/eval guard --model deepseek-v4-flash-free
    go run ./tools/eval guard --api decisions --model jev-guard --paid

# Replay the recorded taint-scorer evals offline (no network, no key).
eval-taint:
    go run ./tools/eval taint --model jev-guard --paid

# Dev-only live view of the browser sidecar's screencast, straight off its
# unix socket (no daemon, no auth). addr="0.0.0.0:8090" to reach it from
# another machine on your network.
browserstream addr="127.0.0.1:8090":
    go run ./tools/browserstream --addr {{addr}}

# End-to-end tests of deploy/install.sh in throwaway Debian machines with
# Docker, Podman or neither (deploy/test/install-e2e.sh). Needs a rootful
# Docker. `just install-e2e podman-rootless` runs one scenario.
install-e2e *scenarios:
    deploy/test/install-e2e.sh {{scenarios}}
