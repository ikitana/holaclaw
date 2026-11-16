# WhatsApp: setup and daily use

This guide links holaclaw to a WhatsApp account and walks through running it
day to day: configuring the channel, linking the phone, inviting people,
checking status, and unlinking. It is for whoever runs holaclaw and has the
phone in hand. For how the adapter works and why, see
[docs/channels/whatsapp.md](../channels/whatsapp.md); this guide never sends you there for
a step you have to perform yourself.

## Contents

- [1. What you need](#1-what-you-need)
- [2. Configure holaclaw](#2-configure-holaclaw)
- [3. Link the phone](#3-link-the-phone)
- [4. Check and status](#4-check-and-status)
- [5. Invites](#5-invites)
- [6. Unlinking](#6-unlinking)
- [7. Daily use](#7-daily-use)
- [8. Common issues](#8-common-issues)

## 1. What you need

- **A dedicated number for the bot.** Use a second SIM, an eSIM, or the
  WhatsApp Business app with a second number on the same phone. Do not link
  your own personal number: the bot would see every one of your chats, your
  contacts would reach the access gate instead of you, and you could only
  talk to the bot through "Message yourself".
- **That number already on WhatsApp**, with the app set up on a phone. The
  phone is the primary device, and holaclaw becomes one of its linked devices
  (WhatsApp allows four).
- **The phone online every ~14 days.** WhatsApp unlinks a device whose
  primary phone has not connected for about that long. If the bot's phone
  will sit in a drawer, plug it in and keep it connected to Wi-Fi or data.
- **holaclaw installed and initialised**, with the daemon running
  continuously on a machine you control: see the
  [deployments](../deployments/README.md). Commands are written as
  `holaclaw …`; run them
  [the way your deployment does](../deployments/README.md#run-a-command).
- **Accepting the ban risk.** WhatsApp's terms do not allow automated or
  unofficial clients, and Meta can ban a number it considers abusive. HolaClaw
  is built to look like a person answering their own phone (never starts a
  conversation, drops strangers before replying, sends at a human pace,
  rejects calls instead of answering), which reduces the risk but does not
  remove it. Use a number you can afford to lose, especially while you are
  still testing.

## 2. Configure holaclaw

Add a channel instance in `channels.toml`:

```toml
# channels.toml
[channels.whatsapp]
type  = "whatsapp"
agent = "main"                    # the agent that answers on this channel
[channels.whatsapp.access]
dm     = "invite"                 # invite | open
rooms  = "approved"               # off | approved | open
reply  = "mention"                # mention | always
```

`type` and `agent` are required. The daemon reads `channels.toml` only when it
starts, so [restart](../deployments/README.md#restart) it after adding the instance; until then
`holaclaw login whatsapp` answers that the daemon has no whatsapp channel of
that name. There are no WhatsApp-specific config keys. The phone number is not one of
them: it is needed once, to link, and afterwards the session itself knows it.

The access settings work as on every channel
([identity.md](../identity.md) §2). Anyone who has the number can message
it, the same as on Telegram, so `dm = "invite"` is the sensible default;
`dm = "open"` answers any stranger who finds the number.
`reply = "mention"` does not starve the agent of context in a group: every
message is still stored in the room's transcript, and a bounded window of
recent messages is injected once someone actually mentions the bot. The
mention only decides when a turn starts.

## 3. Link the phone

The daemon must already be running. Then, with the bot's
phone next to you:

```
$ holaclaw login whatsapp --phone "+34 600 000 000"
On the phone with +34 600 000 000, open WhatsApp → Settings → Linked devices → Link a device
→ Link with phone number instead, and enter:

    ABCD-EFGH

The code expires at 10:02:40. WhatsApp also shows a notification on the phone that opens this screen.
waiting for the phone… (Ctrl-C stops waiting; the daemon keeps the code alive until it expires)
whatsapp: linked as +34600000000
```

1. **`--phone`** takes the number in international format: country code and
   digits. Spaces, dashes and a leading `+` are all accepted and ignored.
2. On the bot's phone, open **WhatsApp → Settings → Linked devices → Link a
   device**, then tap **Link with phone number instead** (there is no QR
   code to scan here) and type the 8-character code shown in the terminal.
3. The code lasts about 160 seconds. If it expires before you type it, just
   run the command again; nothing is left behind to clean up.
4. Once the phone confirms, the terminal prints `whatsapp: linked as
   +34600000000` and returns. The phone lists the device as **holaclaw** under
   Linked devices.
5. `Ctrl-C` while "waiting for the phone…" is showing only stops the CLI from
   watching; the daemon keeps the code alive in the background until it
   expires, so you can still type the code already on screen into the phone
   even after leaving the terminal. To watch again, run
   `holaclaw login whatsapp` once more: while the code is pending it shows the
   same code and waits, and `--phone` is not needed. Once the code has
   expired, `--phone` is required again, because a new code is requested.

`holaclaw login whatsapp` acts on the instance named `whatsapp` by default. With
a channel called something else, pass `--name`, for example
`holaclaw login whatsapp --name sales --phone "+34 600 000 000"`; the same goes
for `holaclaw logout whatsapp --name sales` (§6). A name that is not a
WhatsApp channel on the daemon is refused rather than guessed at, and the
message lists the WhatsApp channels that are there instead:

```
holaclaw: no whatsapp channel named "whatsapp" (whatsapp channels: sales, support); pass --name <channel>
```

## 4. Check and status

```
holaclaw check
```

This warns if a configured WhatsApp instance is not linked — the counterpart
of the credential checks on token-based channels, since WhatsApp has nothing
to put in `auth.toml`:

```
SEC006 warning: channels.toml: channels.whatsapp: channel "whatsapp" is not linked to a WhatsApp account (fix: run `holaclaw login whatsapp --phone <number>`)
```

Link it with the command in §3 to clear the warning. `holaclaw check` reads
the session file locally, so run it on the machine where the daemon keeps its
data.

```
holaclaw status
```

Once linked, the channel's row shows the bot's own number as its detail, so
you never have to remember which number the bot answers on:

```
whatsapp  whatsapp  main  connected  +34600000000
```

Other status values you may see: `unlinked` (no device paired yet, or just
unlinked; the detail reads ``not linked: run `holaclaw login whatsapp --phone …` ``), `linking` (a login is in progress, waiting for the phone),
`reconnecting` (a transient drop; the adapter retries on its own with
backoff), and `logged_out` (see §8).

## 5. Invites

**Direct messages.** An invite binds a WhatsApp account to an identity that
already exists. Take its id from `holaclaw identity list` for the command
below; for someone new, `holaclaw identity create --name Carla --invite
whatsapp` creates them and prints their first invite. Either way you get a
ready-to-send link and its QR code:

```
$ holaclaw invite create --identity <id> --channel whatsapp
code     <code>
channel  whatsapp
expires  2026-10-16 10:00:00Z
link     https://wa.me/34600000000?text=<code>
```

Send that `wa.me` link to the person you want to invite (over any channel —
email, another chat app, in person), or let them scan the QR code. Opening
it starts a WhatsApp chat with the bot's number with the code already typed
in, so all they do is tap **send**. The code is valid for 15 minutes
(`--ttl`, up to 1 h, or 24 h for someone with no bindings yet). If the
instance is not currently connected, the command prints the bare code
instead of a link, since it needs the bot's own number to build the `wa.me`
URL; send that code to the person and have them type it themselves once the
bot is back up.

**Groups.** Add the bot's number to the group the way you'd add any contact.
If an owner or admin who is already linked on WhatsApp is the one who adds
it, the group is approved automatically. Otherwise, mint a room code and have
someone post it, mentioning the bot, inside the group:

```
holaclaw invite create --room --channel whatsapp
```

```
@bot <code>
```

The adapter strips the bot's own `@mention` from the text before reading the
code, so this reaches the access gate as a bare invite code. Room invites
have no link to send ahead of time — the code is only useful typed inside
the group itself.

## 6. Unlinking

From the machine running holaclaw:

```
$ holaclaw logout whatsapp
whatsapp: unlinked
```

This removes the device from the phone's Linked devices list and deletes the
local session. You can also do it from the phone side: open **Settings →
Linked devices**, tap **holaclaw** in the list, and choose **Log out**. Either
way, the other side notices on its own — unlinking from the CLI tells the
phone directly, and removing the device from the phone moves the channel to
`logged_out` and wipes the session automatically. There is no need to do
both. Relinking afterwards costs one fresh code (§3); the daemon needs no
restart.

Linking and unlinking go through the daemon's API, so an admin-scope token is
enough to relink or unlink the account: mint admin tokens sparingly.

## 7. Daily use

- **Who can talk to it** is governed entirely by the access settings in §2;
  there is nothing extra to configure per contact.
- **Groups** answer only when the bot is mentioned (`reply = "mention"`),
  but every message in an approved group still reaches the agent's context
  for the next time it is mentioned.
- **Approvals** show up as plain text with a line explaining how to answer by
  reacting to the message (👍 / 🔁 / ✅ / 👎), since WhatsApp gives unofficial
  clients no buttons.
- **Calls are always rejected.** HolaClaw has no voice stack. A stranger's
  call is declined silently; someone who already had a text conversation
  with the bot is told "I can't take calls. Please write to me instead." in
  the chat.
- **Media:** images, video, voice notes, documents and stickers are received
  normally; view-once media shows as a stub instead of being downloaded.
  Outgoing voice notes only work when the source audio is already Ogg/Opus —
  anything else goes out as a regular file attachment.

## 8. Common issues

| Symptom | What it means | What to do |
|---|---|---|
| `holaclaw login whatsapp --phone …` prints `holaclaw: whatsapp: the code expired; run the command again` | The ~160 s pairing window closed before the code was entered on the phone. | Run the login command again and enter the new code promptly. |
| ``holaclaw: whatsapp: already linked as +34…; run `holaclaw logout whatsapp` first`` | This instance already has a linked device. | Run `holaclaw logout whatsapp` (§6) if you want to replace it, then link again. No restart needed either way. |
| `holaclaw: whatsapp: --phone is required: holaclaw login whatsapp --phone <number>` | No code is pending, so a new one has to be requested for a number. | Add `--phone` with the bot's number (§3). |
| `holaclaw: no whatsapp channel named "…" (…); pass --name <channel>`, followed for login by a `channels.toml` snippet | The named instance (`whatsapp` by default) is not a WhatsApp channel the running daemon knows: it is missing from `channels.toml`, or the daemon was started before you added it. The parentheses list the WhatsApp channels it does know, or say there are none. | For a missing instance, add the snippet the command prints to `channels.toml` (§2), restart the daemon, and run the login again. For one with another name, pass it: `holaclaw login whatsapp --name <name> --phone …`. |
| `holaclaw: "…" is a telegram channel, not a whatsapp one; pass --name <channel>` | `--name` named a channel that exists but is of another type. | Pass the name of an actual WhatsApp channel (§3). |
| `holaclaw logout whatsapp` prints `holaclaw: whatsapp: not linked` | The instance has no linked device, so there is nothing to unlink. | Nothing to do; link it with §3 when you want it. |
| `holaclaw logout whatsapp` prints `holaclaw: whatsapp: the session is deleted, but WhatsApp was not told (…); remove the device in Linked devices on the phone` | The local session is gone, but the daemon could not reach WhatsApp to remove the device (offline, or the connection dropped). | On the phone, open **Settings → Linked devices**, tap **holaclaw** and choose **Log out**. |
| `holaclaw: whatsapp: use the international format, with the country code` | `--phone` started with a `0` (a national-format number). | Pass the number with its country code instead, e.g. `+34600000000`, not `0600000000`. |
| `holaclaw status` shows `logged_out`, detail "unlinked from the phone, or WhatsApp logged this device out; run login again" | Either you (or someone) removed the device from the phone's Linked devices, or WhatsApp logged it out server-side. | Run `holaclaw login whatsapp --phone …` again (§3); the session was already wiped automatically. |
| `holaclaw status` shows `logged_out`, detail "another client is using this session" | WhatsApp's `StreamReplaced`: a second client connected with the same linked-device session, so this one backed off instead of fighting for it. | Make sure only one holaclaw instance (and no other copy of this session) is running, then relink. |
| `holaclaw status` shows `logged_out`, detail "WhatsApp banned this number temporarily: …" | A temporary ban from WhatsApp (`TemporaryBan`), with the reason and duration in the detail. | Wait out the ban; the adapter retries connecting on its own once it expires. No action needed unless it keeps recurring, which is a sign to slow down or stop using that number. |
| `holaclaw status` shows `logged_out`, detail "WhatsApp rejected this client version; update holaclaw" | whatsmeow's protocol version has fallen behind what WhatsApp expects (`ClientOutdated`). | Update holaclaw to a release that bundles a newer whatsmeow, then relink. |
| The bot's phone was wiped, reset, or restored from a backup | The session database at `<data_dir>/whatsapp/<instance>.db` is deliberately left out of backups — a restored copy would hold a stale Signal ratchet that can't decrypt anything, and would collide with the live device if the original is still running. | Restoring the rest of holaclaw's data is fine; just relink this channel afterwards (§3), the same as any fresh instance. |
