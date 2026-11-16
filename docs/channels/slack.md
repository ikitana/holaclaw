# Slack

HolaClaw talks to Slack as a Slack app of your own, over Socket Mode: the
daemon opens the connection to Slack, so it needs no public URL and no open
port. The app has two tokens, and HolaClaw needs both:

| Secret | Token | What it is for |
|---|---|---|
| `bot_token` | `xoxb-…`, the Bot User OAuth Token | reading users and files, posting replies |
| `app_token` | `xapp-…`, an app-level token with `connections:write` | opening the Socket Mode connection |

One app serves one workspace. For a second workspace, create a second app and
a second channel instance (`[channels.slack-other]`).

This page starts with HolaClaw running: see the
[deployments](../deployments/README.md). Commands are written as
`holaclaw …`; run them
[the way your deployment does](../deployments/README.md#run-a-command), and
`channels.toml` is in the
[config directory](../deployments/README.md#config-directory).

## 1. Create the Slack app

On [api.slack.com/apps](https://api.slack.com/apps), choose **Create New
App**, then **From a manifest**, pick the workspace and paste this manifest.
Rename `HolaClaw` to whatever the bot should be called:

```yaml
display_information:
  name: HolaClaw
features:
  app_home:
    messages_tab_enabled: true
    messages_tab_read_only_enabled: false
  bot_user:
    display_name: HolaClaw
    always_online: true
oauth_config:
  scopes:
    bot:
      - app_mentions:read   # mentions in channels
      - channels:history    # messages in public channels it is in
      - groups:history      # private channels
      - im:history          # direct messages
      - mpim:history        # group direct messages
      - chat:write          # replies, and updating approval prompts
      - files:read          # files people send it
      - files:write         # files it sends
      - users:read          # people's display names
settings:
  event_subscriptions:
    bot_events:
      - app_mention
      - message.channels
      - message.groups
      - message.im
      - message.mpim
  interactivity:
    is_enabled: true        # the buttons of approval prompts
  socket_mode_enabled: true
```

Then take the two tokens:

1. **Basic Information → App-Level Tokens → Generate Token and Scopes**: add
   the scope `connections:write` and generate. This is the `xapp-…` token.
2. **Install App → Install to Workspace**, and allow. The page then shows the
   **Bot User OAuth Token**, `xoxb-…`.

## 2. Add the channel

In `channels.toml`:

```toml
[channels.slack-main]
type  = "slack"
agent = "main"
[channels.slack-main.access]
dm       = "invite"     # invite | open
rooms    = "approved"   # off | approved | open
reply    = "mention"    # mention | always
reply_to = "rooms"      # rooms | always | off
```

[identity.md](../identity.md) explains each knob. In a Slack channel the bot
answers mentions only, as `reply = "mention"` says; in a direct message it
answers everything.

## 3. Store the tokens and restart

Each command reads the token from stdin, so it never lands in your shell
history:

```
read -rs TOKEN && printf %s "$TOKEN" | holaclaw auth set channels.slack-main bot_token; unset TOKEN
read -rs TOKEN && printf %s "$TOKEN" | holaclaw auth set channels.slack-main app_token; unset TOKEN
holaclaw check --offline
```

Then [restart](../deployments/README.md#restart) the daemon, which reads
`channels.toml` only at start. `holaclaw check` reports `SEC002` while a
token is missing.

## 4. Talk to it

With `dm = "invite"`, the bot answers only people who have redeemed an
invite. An invite binds a Slack account to an existing identity. Mint one:

```
holaclaw identity list                         # the id for the next command
holaclaw invite create --identity <id> --channel slack-main
```

In Slack, open the app (under **Apps** in the sidebar, or search for its
name), and send it the code as a direct message, on its own. The code is a
credential: the bot never routes or stores that message. Then talk.

To invite someone else, create their identity with its first invite:
`holaclaw identity create --name Carla --invite slack-main`.

## 5. Channels

Add the bot to a channel with `/invite @HolaClaw` in that channel. With
`rooms = "approved"`, the channel is approved the first time the owner or an
admin mentions the bot there; adding the bot alone approves nothing. Until
then, the bot stays silent in that channel, whoever mentions it. Slack has
no room invite links: those are for Telegram and Google Chat.

In a channel, mention the bot (`@HolaClaw …`) to talk to it. It answers in a
thread on the mention, and inside a thread it stays in that thread.

Who may talk to the agent, and with what role: [identity.md](../identity.md).

## 6. How it works

- **Inbound** is Socket Mode: one WebSocket the daemon opens with the app
  token (`xapp-`), so Slack needs no public URL. Outbound is the Web API with
  the bot token (`xoxb-`).
- **Text out** is plain `mrkdwn`, in chunks of 4000 characters; Block Kit is
  used only for approval buttons. Replies in a channel go to the thread
  (`thread_ts`).
- **Files out** go through Slack's external upload flow.
- **Typing.** Slack has no typing indicator for a bot outside its assistant
  threads, so none is shown.
- **Approvals** are Block Kit buttons, acknowledged within Slack's 3 seconds.
- **Rate limits.** The adapter waits as long as Slack's `Retry-After` says
  before it sends again.

How every channel handles sessions, attachments and replies:
[sessions.md](../sessions.md).
