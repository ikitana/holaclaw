# Telegram

This page starts with HolaClaw running: see the
[deployments](../deployments/README.md). Commands are written as
`holaclaw …`; run them
[the way your deployment does](../deployments/README.md#run-a-command), and
`channels.toml` is in the
[config directory](../deployments/README.md#config-directory).

1. Create a bot with [@BotFather](https://t.me/BotFather) and copy its token.
2. Add the channel to `channels.toml`:

   ```toml
   [channels.tg-main]
   type  = "telegram"
   agent = "main"
   [channels.tg-main.access]
   dm       = "invite"     # invite | open
   rooms    = "approved"   # off | approved | open
   reply    = "mention"    # mention | always
   reply_to = "rooms"      # rooms | always | off
   ```

3. Store the token and mint a one-shot invite for an identity:

   ```
   printf %s "$BOT_TOKEN" | holaclaw auth set channels.tg-main bot_token
   holaclaw identity list                         # the id for the next command
   holaclaw invite create --identity <id> --channel tg-main
   ```

4. Send `/start <code>` to the bot, or open the deep link the invite printed
   (or scan its QR code), and talk.

To invite someone else, create their identity with its first invite:
`holaclaw identity create --name Carla --invite tg-main`.

[Restart](../deployments/README.md#restart) the daemon after editing
`channels.toml`, which it reads only at start.

## Groups

`holaclaw invite create --room --channel tg-main` prints a link that asks
which group, adds the bot and approves that group in one tap. It needs
`rooms = "approved"`. Tapping it does not link the person who taps. A linked
owner or admin can also add the bot to a group themselves, which approves it.
If anyone else adds the bot, nothing is approved.

Who may talk to the agent, and with what role: [identity.md](../identity.md).

## How it works

- **Inbound** is long polling, with the update offset persisted, so a restart
  picks up where it stopped. The photos of an album arrive as one message:
  the adapter waits 0.8 seconds for the rest of a `media_group_id`.
- **Text out** is Telegram's HTML parse mode, safer to split than
  MarkdownV2. A message holds 4096 characters and a caption 1024: a reply
  with one attachment goes as its caption when the text fits, else the text
  goes first.
- **Typing.** `sendChatAction` is refreshed every 4 seconds while a turn
  runs, and backs off after a failure.
- **Forum topics** are threads: each topic is its own session, and the reply
  goes to the same topic (`message_thread_id`).
- **Approvals** are inline keyboard buttons, with a short id in
  `callback_data`, which Telegram caps at 64 bytes.
- **Rate limits.** The adapter waits as long as Telegram's `retry_after` says
  before it sends again.

How every channel handles sessions, attachments and replies:
[sessions.md](../sessions.md).
For Slack, see [slack.md](slack.md).
