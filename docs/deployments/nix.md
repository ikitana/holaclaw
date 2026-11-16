# HolaClaw on NixOS

The flake exports a NixOS module (`nixosModules.default`), a package
(`packages.<system>.holaclaw`), an overlay (`overlays.default`) and the
container images (`packages.<system>.container`, with the browser, and
`packages.<system>.container-slim`, without; see
[container.md](container.md)). They work on Linux only. The module installs
the same hardened units as `deploy/`. As on other distributions, the binary,
the module and the paths keep the name holaclaw until the rename lands; see
the note in [README.md](README.md), which also lists the other deployments.

## Install

1. Enable the module and switch (below).
2. Run `init` with your provider, and restart the service, which fails
   until a config exists. The providers are those of
   [README.md §2](README.md#2-choose-a-provider), with
   `--config /etc/holaclaw` added.
3. Continue with [getting-started.md](../getting-started.md): chat with
   `sudo -u holaclaw holaclaw --config /etc/holaclaw chat`. A ChatGPT
   subscription signs in first with `login codex`.

```nix
# flake.nix
inputs.holaclaw.url = "github:ikitana/holaclaw";

# configuration.nix, with holaclaw.nixosModules.default in `modules`
services.holaclaw = {
  enable = true;
  # `holaclaw init` sets `server.secrets_dir` to the `secrets` directory
  # inside the config directory. This option must match it.
  secretsDir = "/etc/holaclaw/secrets";
  browser.enable = true;   # optional: runs the browser sidecar with Chromium and Xvfb
};
```

Create the config and the owner's token, here for OpenRouter, which asks for
the API key:

```
sudo -u holaclaw holaclaw init --config /etc/holaclaw --provider openrouter --token-file /etc/holaclaw/token
sudo systemctl restart holaclaw
```

To change the model later, edit the files in `/etc/holaclaw` with `sudoedit`
as described in [getting-started.md §3](../getting-started.md#4-change-the-model-or-the-provider),
or declare them in Nix (below).

To upgrade, update the flake input and switch. To remove it, set
`enable = false` and switch; the config directory and the state stay.

The unit sets `HOLACLAW_CONFIG`, but your shell does not. Pass
`--config /etc/holaclaw`, or set `HOLACLAW_CONFIG=/etc/holaclaw`, when you run
other commands as the `holaclaw` user.

## Declaring the config

You can declare `config.toml`, `channels.toml` and agents in Nix instead of
writing them with `init`. If `config.toml` already exists, `init` only creates
the database and the owner.

```nix
environment.etc = let
  file = source: {
    inherit source;
    # With a mode, NixOS copies the file instead of linking it to the store.
    mode = "0440";
    user = "holaclaw";
    group = "holaclaw";
  };
in {
  "holaclaw/config.toml" = file ./config.toml;
  "holaclaw/channels.toml" = file ./channels.toml;
  # Declare agents file by file, so that agents/ stays a directory.
  "holaclaw/agents/main/agent.toml" = file ./agents/main/agent.toml;
  "holaclaw/agents/main/prompt.md" = file ./agents/main/prompt.md;
};

# NixOS creates the parent directories of these files as root, mode 0700. The
# daemon cannot read them, so give them to holaclaw.
systemd.tmpfiles.settings.holaclaw-agents = {
  "/etc/holaclaw/agents".d = { user = "holaclaw"; group = "holaclaw"; mode = "0700"; };
  "/etc/holaclaw/agents/main".d = { user = "holaclaw"; group = "holaclaw"; mode = "0700"; };
};
```

Set `mode` on every file. Without it, the file is a symlink with mode 0777.
`holaclaw check` does not follow symlinks, so it reports SEC005
(world-writable).

These files are read-only, and every switch rewrites them. Edit them in Nix,
not on the machine. The rest of the config directory keeps its state: the data
directory, the secrets and the owner's token.

## Running isolated

On NixOS, the `exec` tool runs commands from `/run/current-system/sw/bin`. It
can run every program installed on the machine. If the machine does anything
besides holaclaw, run holaclaw in a container or a VM, and install there only the
programs you want `exec` to use. Every option below uses the same module.

**NixOS container.** The container runs under systemd-nspawn and shares the
host's kernel.
`nix build .#checks.<system>.container` tests this setup.

```nix
containers.holaclaw = {
  autoStart = true;
  # Maps container users to unprivileged host users. Without it, root in the
  # container is root on the host.
  privateUsers = "pick";
  config = {
    imports = [ holaclaw.nixosModules.default ];
    services.holaclaw = {
      enable = true;
      secretsDir = "/etc/holaclaw/secrets";
    };
    environment.systemPackages = [ ];   # the programs exec can run
    system.stateVersion = "26.05";
  };
};
```

```
nixos-container run holaclaw -- runuser -u holaclaw -- holaclaw init --config /etc/holaclaw --provider openrouter --token-file /etc/holaclaw/token
nixos-container run holaclaw -- systemctl restart holaclaw
nixos-container run holaclaw -- runuser -u holaclaw -- env HOLACLAW_CONFIG=/etc/holaclaw holaclaw status
```

To run `holaclaw` from the host, install a wrapper on the host. It runs the CLI
in the container as the `holaclaw` user, next to the config, the secrets and the
control socket. `nixos-container run` passes the terminal through, so `chat`,
`memory` and the other TUIs work too.

```nix
environment.systemPackages = [
  (pkgs.writeShellApplication {
    name = "holaclaw";
    text = ''
      if [ "$(id -u)" -ne 0 ]; then
        exec /run/wrappers/bin/sudo "$0" "$@"
      fi
      exec nixos-container run holaclaw -- \
        runuser -u holaclaw -- env HOLACLAW_CONFIG=/etc/holaclaw holaclaw "$@"
    '';
  })
];
```

The container uses the host's network, so it reaches a host Ollama at
`localhost:11434`. If a host name resolves on the host but not in the
container (for example, a tailnet name), put the IP address in `base_url`. The
container keeps its files in `/var/lib/nixos-containers/holaclaw`.

`privateUsers` requires `/nix/store` on a filesystem that supports idmapped
mounts: ext4, btrfs, xfs or ZFS 2.2 and later. On other filesystems the
container does not start.

**MicroVM.** [microvm.nix](https://github.com/microvm-nix/microvm.nix) runs
holaclaw in a VM with its own kernel. This is the strongest isolation. Use it if
you enable the browser: an attacker who escapes Chromium ends up in the VM, not
on the host. CI does not test this setup. Set up host networking for the tap
interface as microvm.nix's documentation describes.

```nix
# flake inputs: microvm.url = "github:microvm-nix/microvm.nix";
imports = [ microvm.nixosModules.host ];
microvm.vms.holaclaw.config = {
  imports = [ microvm.nixosModules.microvm holaclaw.nixosModules.default ];
  microvm = {
    vcpu = 2;
    mem = 4096;
    interfaces = [{ type = "tap"; id = "vm-holaclaw"; mac = "02:00:00:00:00:01"; }];
    shares = [{ proto = "virtiofs"; tag = "ro-store"; source = "/nix/store"; mountPoint = "/nix/.ro-store"; }];
    # The VM loses its root filesystem on reboot. Keep config, secrets and
    # state on volumes. The secrets are in /etc/holaclaw/secrets.
    volumes = [
      { image = "config.img"; mountPoint = "/etc/holaclaw"; size = 64; }
      { image = "state.img";  mountPoint = "/var/lib";     size = 4096; }
    ];
  };
  services.holaclaw = {
    enable = true;
    secretsDir = "/etc/holaclaw/secrets";
    browser.enable = true;
  };
  system.stateVersion = "26.05";
};
```

**Dedicated VM or server.** Install NixOS on it and enable the module as shown
at the top of this section.
