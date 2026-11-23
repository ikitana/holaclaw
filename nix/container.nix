# The release container images: the Dockerfile's images, built by Nix. With
# `browser` (latest), the daemon and the browser sidecar run side by side as
# two users, started by deploy/docker/entrypoint.sh; without it (slim), no
# Chromium or Xvfb.
#
# `pkgs` builds the image; `target` is the package set of the image's
# architecture. Everything from `target` is a store path substituted from the
# binary cache and holaclaw is cross-compiled (pure Go), so an x86_64 builder
# produces the arm64 image too, with no aarch64 builder or emulation.
{ pkgs, target, holaclaw, version, browser ? true }:
let
  arch = target.go.GOARCH;
  lib = pkgs.lib;

  # The binary where the Dockerfile puts it, and `holaclaw` on PATH as the
  # wrapper that runs it as the holaclaw user.
  bin = pkgs.runCommand "holaclaw-image-bin" { } ''
    mkdir -p $out/bin $out/usr/libexec/holaclaw
    ln -s ${holaclaw}/bin/holaclaw $out/usr/libexec/holaclaw/holaclaw
    install -m 755 ${../deploy/docker/holaclaw.sh} $out/bin/holaclaw
    install -m 755 ${../deploy/docker/entrypoint.sh} $out/bin/holaclaw-entrypoint
  '';

  root = pkgs.buildEnv {
    name = "holaclaw-image-root";
    paths = [
      bin
      target.bash # /bin/sh, for the entrypoint
      target.coreutils
      target.gnused
      target.nano # for editing the config inside the container
      target.util-linux # setpriv
      target.cacert
    ] ++ lib.optionals browser [
      target.chromium
      target.xvfb
    ];
    pathsToLink = [ "/bin" "/etc/ssl" "/usr/libexec" ];
  };

  fonts = pkgs.makeFontsConf { fontDirectories = [ target.dejavu_fonts ]; };

  # The same users as the Dockerfile, with fixed ids so volumes keep their owners.
  passwd = pkgs.writeText "passwd" ''
    root:x:0:0:root:/root:/bin/sh
    holaclaw:x:999:999::/var/lib/holaclaw:/bin/false
    holaclaw-browser:x:998:998::/var/lib/holaclaw-browser:/bin/false
    nobody:x:65534:65534::/nonexistent:/bin/false
  '';
  group = pkgs.writeText "group" ''
    root:x:0:
    holaclaw:x:999:
    holaclaw-browser:x:998:holaclaw
    nogroup:x:65534:
  '';
in
pkgs.dockerTools.streamLayeredImage {
  name = "ghcr.io/ikitana/holaclaw";
  tag = if browser then version else "${version}-slim";
  architecture = arch;
  contents = [ root ];

  fakeRootCommands = ''
    mkdir -p etc tmp root \
      etc/holaclaw etc/holaclaw-secrets var/lib/holaclaw var/lib/holaclaw-browser \
      run/holaclaw run/holaclaw-browser
    cp ${passwd} etc/passwd
    cp ${group} etc/group
    chmod 1777 tmp
    chown 999:999 var/lib/holaclaw run/holaclaw etc/holaclaw-secrets
    chown 998:998 var/lib/holaclaw-browser run/holaclaw-browser
    chown 999:998 etc/holaclaw
    chmod 700 var/lib/holaclaw run/holaclaw etc/holaclaw-secrets var/lib/holaclaw-browser
    chmod 750 run/holaclaw-browser etc/holaclaw
  '';

  config = {
    Entrypoint = [ "/bin/holaclaw-entrypoint" ];
    Cmd = [ "run" ];
    Env = [
      "PATH=/bin"
      "HOLACLAW_CONTAINER=1"
      "HOLACLAW_CONFIG=/etc/holaclaw"
      "HOLACLAW_LISTEN=0.0.0.0:8484"
      "HOLACLAW_ALLOW_INSECURE_TRANSPORT=true"
      "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
      "ZONEINFO=${target.tzdata}/share/zoneinfo"
    ] ++ lib.optionals browser [
      "HOLACLAW_BROWSER_ENABLED=true"
      "FONTCONFIG_FILE=${fonts}"
    ];
    ExposedPorts = { "8484/tcp" = { }; };
    Volumes = {
      "/etc/holaclaw" = { };
      "/etc/holaclaw-secrets" = { };
      "/var/lib/holaclaw" = { };
    };
    Labels = {
      "org.opencontainers.image.title" = "HolaClaw";
      "org.opencontainers.image.source" = "https://github.com/ikitana/holaclaw";
      "org.opencontainers.image.version" = version;
      "org.opencontainers.image.licenses" = "Apache-2.0";
    };
  };
}
