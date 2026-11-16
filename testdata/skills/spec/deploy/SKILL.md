---
name: deploy
description: When deploying the service to production.
license: Apache-2.0
compatibility: Requires git and network access to the registry.
allowed-tools: [Bash, WebFetch, exec, browser]
metadata:
  author: example-org
  version: "1.2"
  openclaw:
    emoji: "🚀"
    requires:
      config: [deploy.target]
      env:
        - DEPLOY_TOKEN
    install: [{kind: brew, formula: flyctl, bins: [fly]}]
  holaclaw:
    version: 1
    requires:
      tools: [exec]
      bins: [git]
---
# Deploy

## Steps

1. Run the tests.
2. Push the image.
