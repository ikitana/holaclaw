{
  description = "holaclaw agent harness";

  inputs = {
    flake-utils.url = "github:numtide/flake-utils";
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = {
    self,
    flake-utils,
    nixpkgs,
  }: let
    # A release passes its tag: HOLACLAW_VERSION=v1.2.3 nix build --impure ...
    version = let tag = builtins.getEnv "HOLACLAW_VERSION"; in
      if tag != "" then tag else self.shortRev or self.dirtyShortRev or "dev";

    # Pure Go, no CGO: same flags as `just release`.
    mkHolaClaw = pkgs: pkgs.buildGoModule {
      pname = "holaclaw";
      inherit version;
      src = pkgs.lib.cleanSource ./.;
      vendorHash = "sha256-lnLQGyolMLOKNkyqKAPGZLuZ4LYCZ/avo8YJRHO5U2k=";
      subPackages = [ "cmd/holaclaw" ];
      env.CGO_ENABLED = 0;
      ldflags = [ "-s" "-w" "-X holaclaw/internal/daemon.Version=${version}" ];
      # The suite (real Chromium, sockets) runs in CI through the dev shell.
      doCheck = false;
      meta = {
        description = "holaclaw agent harness";
        license = pkgs.lib.licenses.asl20;
        mainProgram = "holaclaw";
        # The browser sidecar uses Linux-only process attributes (Pdeathsig).
        platforms = pkgs.lib.platforms.linux;
      };
    };

    # holaclaw for another architecture, cross-compiled by the host's Go.
    mkHolaClawFor = pkgs: goarch: (mkHolaClaw pkgs).overrideAttrs (old: {
      env = old.env // { GOARCH = goarch; };
      postBuild = (old.postBuild or "") + ''
        dir="$GOPATH/bin/linux_${goarch}"
        if [ -d "$dir" ]; then mv "$dir"/* "$GOPATH/bin/" && rmdir "$dir"; fi
      '';
    });

    # The release image for one architecture (nix/container.nix): latest with
    # the browser, slim without.
    mkContainer = pkgs: system: goarch: browser: import ./nix/container.nix {
      inherit pkgs version browser;
      target = nixpkgs.legacyPackages.${system};
      holaclaw = mkHolaClawFor pkgs goarch;
    };
  in
    flake-utils.lib.eachDefaultSystem (system: let
      pkgs = import nixpkgs { inherit system; };
    in {
      packages = pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux rec {
        holaclaw = mkHolaClaw pkgs;
        default = holaclaw;
        # `./result | docker load`. Both architectures build on any Linux host.
        container-amd64 = mkContainer pkgs "x86_64-linux" "amd64" true;
        container-arm64 = mkContainer pkgs "aarch64-linux" "arm64" true;
        container = if pkgs.stdenv.hostPlatform.isAarch64 then container-arm64 else container-amd64;
        container-slim-amd64 = mkContainer pkgs "x86_64-linux" "amd64" false;
        container-slim-arm64 = mkContainer pkgs "aarch64-linux" "arm64" false;
        container-slim = if pkgs.stdenv.hostPlatform.isAarch64 then container-slim-arm64 else container-slim-amd64;
      };

      # Keep docs/deployments/nix.md's container recipe and browser.md's NixOS steps
      # accurate. Need a builder with KVM.
      checks = pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
        container = import ./nix/tests/container.nix {
          inherit pkgs;
          module = self.nixosModules.default;
        };
        browser = import ./nix/tests/browser.nix {
          inherit pkgs;
          module = self.nixosModules.default;
        };
        # The browser image's font set; needs no KVM.
        fonts = import ./nix/tests/fonts.nix { inherit pkgs; };
      };

      devShells.default = pkgs.mkShell {
        packages = with pkgs; [
          # Go daemon + CLI
          go
          gopls
          gotools          # goimports, stringer, ...
          golangci-lint
          delve

          # API contract: spec-first codegen. Breaking-change detection uses
          # oasdiff, which is not in nixpkgs: install a pinned version with
          # `go install github.com/oasdiff/oasdiff@<version>` (lands in .go/bin,
          # already on PATH via the shellHook).
          oapi-codegen

          # Misc
          sqlite           # CLI to inspect the store
          just
          curl             # scripts/report.sh
          jq               # scripts/report.sh
          docker-client    # deploy/test/install-e2e.sh, against the system's rootful daemon
          shellcheck       # deploy/install.sh, deploy/test/install-e2e.sh
        ]
        # Browser (docs/browser.md): tests drive a real Chromium on a virtual
        # display. Both are Linux-only in nixpkgs; elsewhere the browser
        # integration tests skip (chromium.Find finds nothing on PATH).
        ++ lib.optionals stdenv.hostPlatform.isLinux [
          chromium
          xvfb
        ];

        # Keep Go's module cache and build cache inside the project so a
        # fresh clone or a CI runner does not pollute $HOME.
        shellHook = ''
          export GOPATH="$PWD/.go"
          export GOMODCACHE="$GOPATH/pkg/mod"
          export GOCACHE="$GOPATH/cache"
          # Writable module cache dirs, so worktrees (and .go/) can be deleted.
          export GOFLAGS="-modcacherw''${GOFLAGS:+ $GOFLAGS}"
          export PATH="$GOPATH/bin:$PATH"

          # Pure Go only: the binary stays static and cross-compiles.
          export CGO_ENABLED=0

          # Load private env files (gitignored), e.g. OPENROUTER_KEY for evals.
          for f in .env .env.local; do
            if [ -f "$f" ]; then
              set -a
              . "$f"
              set +a
            fi
          done
        '';
      };

      formatter = pkgs.nixfmt-rfc-style;
    })
    // {
      overlays.default = final: _prev: {
        holaclaw = mkHolaClaw final;
      };

      # NixOS counterpart of deploy/holaclaw.service and
      # deploy/holaclaw-browser.service. Usage:
      #   imports = [ holaclaw.nixosModules.default ];
      #   services.holaclaw = { enable = true; secretsDir = "/etc/holaclaw/secrets"; };
      # (init puts secrets_dir in configDir/secrets), then run
      #   sudo -u holaclaw holaclaw init --config /etc/holaclaw --token-file /etc/holaclaw/token
      nixosModules.default = { config, lib, pkgs, ... }: let
        cfg = config.services.holaclaw;

        # Shared by both units. holaclaw never assumes it is sandboxed; this
        # narrows the damage of an allowed `exec` tool call, it does not
        # replace the policy gate.
        hardening = {
          ProtectSystem = "strict";
          ProtectHome = true;
          PrivateTmp = true;
          PrivateDevices = true;
          ProtectKernelTunables = true;
          ProtectKernelModules = true;
          ProtectKernelLogs = true;
          ProtectControlGroups = true;
          ProtectClock = true;
          ProtectHostname = true;
          RestrictRealtime = true;
          RestrictSUIDSGID = true;
          LockPersonality = true;
          RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
          SystemCallArchitectures = "native";
          CapabilityBoundingSet = "";
          AmbientCapabilities = "";
          UMask = "0077";
          Restart = "on-failure";
          RestartSec = 3;
          # Input interrupted by a stop is re-queued and replayed on start.
          TimeoutStopSec = 20;
        };
      in {
        options.services.holaclaw = {
          enable = lib.mkEnableOption "the holaclaw agent harness";

          package = lib.mkOption {
            type = lib.types.package;
            default = self.packages.${pkgs.stdenv.hostPlatform.system}.holaclaw;
            defaultText = lib.literalExpression "holaclaw.packages.\${system}.holaclaw";
            description = "The holaclaw package to run.";
          };

          configDir = lib.mkOption {
            type = lib.types.path;
            default = "/etc/holaclaw";
            description = "Configuration directory (HOLACLAW_CONFIG). Populate it from deploy/example.";
          };

          secretsDir = lib.mkOption {
            type = lib.types.path;
            default = "/etc/holaclaw-secrets";
            description = "Directory holding auth.toml; must match secrets_dir in config.toml (`holaclaw init` sets it to configDir/secrets).";
          };

          browser.enable = lib.mkEnableOption ''
            the browser sidecar (Chromium on Xvfb, as its own user). Also add an
            empty `[browser]` table to config.toml; its socket defaults to this unit's
          '';
        };

        config = lib.mkIf cfg.enable (lib.mkMerge [
          {
            environment.systemPackages = [ cfg.package ];

            users.users.holaclaw = {
              isSystemUser = true;
              group = "holaclaw";
              home = "/var/lib/holaclaw";
            };
            users.groups.holaclaw = { };

            systemd.tmpfiles.settings.holaclaw = {
              ${cfg.configDir}.d = {
                user = "holaclaw";
                group = if cfg.browser.enable then "holaclaw-browser" else "holaclaw";
                mode = if cfg.browser.enable then "0750" else "0700";
              };
              # Secrets stay holaclaw-only: the sidecar never reads them.
              ${cfg.secretsDir}.d = {
                user = "holaclaw";
                group = "holaclaw";
                mode = "0700";
              };
            };

            systemd.services.holaclaw = {
              description = "holaclaw agent harness";
              after = [ "network-online.target" ];
              wants = [ "network-online.target" ];
              wantedBy = [ "multi-user.target" ];
              environment.HOLACLAW_CONFIG = cfg.configDir;
              serviceConfig = hardening // {
                User = "holaclaw";
                Group = "holaclaw";
                ExecStart = "${lib.getExe cfg.package} run";
                # Holds the control socket, /run/holaclaw/holaclaw.sock.
                RuntimeDirectory = "holaclaw";
                RuntimeDirectoryMode = "0700";
                StateDirectory = "holaclaw";
                StateDirectoryMode = "0700";
                # The daemon writes only its state and auth.toml (rotating OAuth tokens).
                ReadWritePaths = [ "/var/lib/holaclaw" cfg.secretsDir cfg.configDir ]
                  # The browser sidecar's socket (§21). "-": the directory is
                  # gone while the sidecar is down, which must not stop the daemon.
                  ++ lib.optional cfg.browser.enable "-/run/holaclaw-browser";
                NoNewPrivileges = true;
                RestrictNamespaces = true;
                MemoryDenyWriteExecute = true;
                SystemCallFilter = [ "@system-service" ];
              };
            };
          }

          (lib.mkIf cfg.browser.enable {
            users.users.holaclaw-browser = {
              isSystemUser = true;
              group = "holaclaw-browser";
              home = "/var/lib/holaclaw-browser";
            };
            users.groups.holaclaw-browser = { };
            # The daemon reaches the 0660 socket through the group.
            users.users.holaclaw.extraGroups = [ "holaclaw-browser" ];

            systemd.services.holaclaw.after = [ "holaclaw-browser.service" ];

            # The same fonts as the container images, system-wide.
            fonts.packages = (import ./nix/fonts.nix { inherit pkgs; }).packages;

            # See deploy/holaclaw-browser.service for why this unit relaxes
            # NoNewPrivileges, RestrictNamespaces, MemoryDenyWriteExecute and
            # SystemCallFilter: each is forced by Chromium's own sandbox.
            systemd.services.holaclaw-browser = {
              description = "holaclaw browser sidecar (Chromium)";
              before = [ "holaclaw.service" ];
              wantedBy = [ "multi-user.target" ];
              path = [ pkgs.chromium pkgs.xvfb ];
              environment = {
                HOLACLAW_CONFIG = cfg.configDir;
                XDG_RUNTIME_DIR = "/run/holaclaw-browser";
              };
              serviceConfig = hardening // {
                User = "holaclaw-browser";
                Group = "holaclaw-browser";
                ExecStart = "${lib.getExe cfg.package} browser";
                RuntimeDirectory = "holaclaw-browser";
                RuntimeDirectoryMode = "0750";
                StateDirectory = "holaclaw-browser";
                StateDirectoryMode = "0700";
                # A browser is not a small program: bound what one runaway page can take.
                MemoryMax = "2G";
                TasksMax = 512;
                NoNewPrivileges = false;
                RestrictNamespaces = false;
              };
            };
          })
        ]);
      };
    };
}
