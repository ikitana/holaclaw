# docs/deployments/nix.md's install with `browser.enable`, then browser.md's NixOS
# steps: the config and the agents readable by the sidecar's
# group and the [browser] table. The sidecar runs as its own
# user and the daemon reaches it.
{ pkgs, module }:
pkgs.testers.runNixOSTest {
  name = "holaclaw-browser";

  nodes.machine = {
    imports = [ module ];
    virtualisation.memorySize = 3072;
    environment.systemPackages = [ pkgs.fontconfig ]; # fc-match
    services.holaclaw = {
      enable = true;
      secretsDir = "/etc/holaclaw/secrets";
      browser.enable = true;
    };
  };

  testScript = ''
    hb = "runuser -u holaclaw -- env HOLACLAW_CONFIG=/etc/holaclaw holaclaw"

    machine.wait_for_unit("multi-user.target")
    machine.succeed("sudo -u holaclaw holaclaw init --config /etc/holaclaw --provider openai-compat --base-url http://127.0.0.1:9/v1 --model test --token-file /etc/holaclaw/token </dev/null")
    machine.succeed("systemctl restart holaclaw")
    machine.wait_until_succeeds(f"{hb} status")

    # browser.md: an empty [browser] table turns it on, edited as root.
    machine.succeed("printf '\\n[browser]\\n' >> /etc/holaclaw/config.toml")

    # Without the next step, init's files (0600 and 0700, holaclaw's) are
    # closed to the sidecar's user.
    machine.succeed("systemctl restart holaclaw-browser")
    machine.sleep(5)
    print(machine.execute("stat -c '%U:%G %a %n' /etc/holaclaw /etc/holaclaw/config.toml")[1])
    print(machine.execute("journalctl -u holaclaw-browser -n 5 --no-pager")[1])
    machine.fail("test -S /run/holaclaw-browser/browser.sock")

    # browser.md: readable by the group the module gives /etc/holaclaw.
    machine.succeed("cd /etc/holaclaw && chgrp -R holaclaw-browser config.toml agents && chmod -R g+rX config.toml agents")
    machine.succeed("systemctl restart holaclaw-browser")
    machine.wait_until_succeeds("test -S /run/holaclaw-browser/browser.sock")
    machine.succeed("systemctl restart holaclaw")
    machine.wait_until_succeeds(f"{hb} status")
    machine.fail(f"{hb} check --offline 2>&1 | grep -E '^BRW00[1-4]'")
    # The module's fonts: none of these falls back to DejaVu.
    for query, family in [("Arial", "Liberation Sans"), (":lang=ja", "Noto Sans CJK"), ("emoji", "Noto Color Emoji")]:
        got = machine.succeed(f"runuser -u holaclaw-browser -- fc-match '{query}' family")
        assert got.startswith(family), f"fc-match {query}: got {got}, want {family}"
    # Everything else stays the daemon's.
    machine.succeed('test "$(stat -c %a /etc/holaclaw/data)" = 700')
  '';
}
