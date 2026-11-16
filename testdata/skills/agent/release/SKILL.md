---
name: release
description: "Cutting a release: tests, version bump, tag, announce."
license: MIT
metadata:
  holaclaw:
    version: 1
    requires:
      tools: [exec, files]   # agent tools it needs
      bins: [gh, {name: rg, pkg: ripgrep}, {name: bpython, pkg: python3Packages.bpython}]
      net: [gh]
---
# Release

Cut a release of a Go service.

## When

The person asks for a release.

## Steps

1. Run the tests.
2. Bump the version and tag it.
