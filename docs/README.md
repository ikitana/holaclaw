# HolaClaw documentation

## Start here

- [Deployments](deployments/README.md): the ways to run HolaClaw, and where
  each one keeps its config, secrets and data.
  - [In a container](deployments/container.md)
  - [On Linux, with systemd](deployments/linux.md), or
    [as your own user](deployments/linux.md#9-as-your-own-user)
  - [On NixOS](deployments/nix.md)
- [Getting started](getting-started.md): chat, change the model, pair a
  device.

## Guides

Step by step, for whoever runs HolaClaw.

| Guide | |
|---|---|
| [Operator manual](guides/operator.md) | day to day: config changes, upgrades, invites, backups, common issues |
| [Telegram](channels/telegram.md) | talk to the agent from a Telegram bot |
| [Slack](channels/slack.md) | from a Slack app of your own |
| [Google Chat](guides/googlechat.md) | from a Chat app, set up in Google Cloud |
| [WhatsApp](guides/whatsapp.md) | from a WhatsApp account, linked as a device |
| [Connectors](guides/connectors.md) | give an agent the tools of a remote MCP server |

## How it works

| Topic | |
|---|---|
| [Sessions and messages](sessions.md) | conversations, turns and lanes, rooms, attachments, replies |
| [Security](security.md) | what the agent may do on its own, policies, approvals, secrets, hardening |
| [Identities](identity.md) | people, rooms, roles, bindings, invites, devices and tokens |
| [Memory](memory.md) | what the agent remembers, and who can read it |
| [Context](context.md) | how a prompt fits the model's window, and compaction |
| [Skills](skills.md) | procedures the agent loads, and learns from you |
| [Tasks](tasks.md) | work the agent does later or on a schedule |
| [Browser](browser.md) | the browser the agent drives, and its sidecar |
| [Connectors](connectors.md) | remote MCP servers: sign-in, tools, health |
| [Google Chat channel](channels/googlechat.md), [WhatsApp channel](channels/whatsapp.md) | how those two adapters work |
| [Models](models.md) | providers, model profiles and the roles an agent gives them |
| [Traceability](traceability.md) | traces, events, and the usage and cost of every model call |

## Reference

Generated from the code.

- [Config keys](reference/config.md)
- [Commands](reference/cli.md)
- [API](reference/api.md)
- [Events](reference/events.md)
- [`holaclaw check` rules](reference/check-rules.md)

## Design

- [ARCHITECTURE.md](../ARCHITECTURE.md): how the project fits together, and a map of the code.
