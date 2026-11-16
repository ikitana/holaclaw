# docs/deployments/nix.md's "NixOS container" recipe, run: a host boots a user-namespaced
# container that imports the module, holaclaw is initialised inside it and the
# daemon answers on its control socket.
{ pkgs, module }:
pkgs.testers.runNixOSTest {
  name = "holaclaw-container";

  nodes.host = {
    virtualisation.memorySize = 2048;
    # privateUsers needs idmapped mounts of /nix/store; the default virtiofs
    # share has none, a disk image does (as a real host's ext4 or btrfs).
    virtualisation.useNixStoreImage = true;
    # nspawn binds the Nix database into the container; a store image has none.
    systemd.tmpfiles.rules = [ "d /nix/var/nix/db 0755 root root -" ];

    containers.holaclaw = {
      autoStart = true;
      privateUsers = "pick";
      config = {
        imports = [ module ];
        services.holaclaw = {
          enable = true;
          secretsDir = "/etc/holaclaw/secrets";
        };
        system.stateVersion = "26.05";
      };
    };
  };

  testScript = ''
    def inside(cmd):
        return host.succeed(f"nixos-container run holaclaw -- {cmd}")

    host.wait_for_unit("container@holaclaw.service")
    host.wait_until_succeeds("nixos-container run holaclaw -- systemctl is-system-running --wait | grep -E 'running|degraded'")

    # privateUsers: the container's root is a shifted UID on the host.
    host.succeed('test "$(stat -c %u /proc/$(machinectl show holaclaw -p Leader --value))" != 0')

    inside("runuser -u holaclaw -- holaclaw init --config /etc/holaclaw --provider openai-compat --base-url http://127.0.0.1:9/v1 --model test --token-file /etc/holaclaw/token </dev/null")
    inside("runuser -u holaclaw -- env HOLACLAW_CONFIG=/etc/holaclaw holaclaw check --offline")
    # A stored secret lands in the secrets_dir init wrote.
    inside("runuser -u holaclaw -- sh -c 'printf k | env HOLACLAW_CONFIG=/etc/holaclaw holaclaw auth set providers.openai-compat api_key'")
    inside("test -f /etc/holaclaw/secrets/auth.toml")
    inside("systemctl restart holaclaw.service")
    host.wait_until_succeeds("nixos-container run holaclaw -- test -S /run/holaclaw/holaclaw.sock")
    inside("runuser -u holaclaw -- env HOLACLAW_CONFIG=/etc/holaclaw holaclaw status")
  '';
}
