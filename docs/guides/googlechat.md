# Google Chat: setup and daily use

This guide connects holaclaw to Google Chat, so you and your team can talk to
your agent in a direct message or in a Chat space. It is for whoever runs
holaclaw and can use the Google Cloud console for their organisation. You
don't need to know Google Cloud already: every click is written out.

You set up Google Cloud once (§2). After that, you give holaclaw the key it
created (§4), start it, and invite people (§6).

## Contents

- [1. Requirements](#1-requirements)
- [2. Google Cloud setup](#2-google-cloud-setup)
- [3. Configure holaclaw](#3-configure-holaclaw)
- [4. Set the key](#4-set-the-key)
- [5. Check and start](#5-check-and-start)
- [6. First contact](#6-first-contact)
- [7. Daily use](#7-daily-use)
- [8. Limits](#8-limits)
- [9. Security notes](#9-security-notes)
- [10. Troubleshooting](#10-troubleshooting)

## 1. Requirements

You need all of the following before you start:

- **A Google Workspace account** (Business or Enterprise) that is allowed to
  use Chat apps. A personal `@gmail.com` account can't host a Chat app.
- **A Google Cloud project with billing enabled.** Pub/Sub, which carries
  Chat's messages to holaclaw, needs a billing account. A bot's traffic stays
  inside the free tier.
- **Permission to edit that project**, for the one-time setup in §2.
- **holaclaw installed and initialised**, on a machine that can reach Google:
  see the [deployments](../deployments/README.md). Commands are written as
  `holaclaw …`; run them
  [the way your deployment does](../deployments/README.md#run-a-command).

## 2. Google Cloud setup

Do this once, in the Google Cloud console. Pick the right project at the top
of each page before you click through.

1. **Project and billing.** Create a project (or reuse one) and link a
   billing account: <https://console.cloud.google.com/projectcreate>, then
   *Billing → Link a billing account*.
2. **Enable the APIs.** Turn on the Google Chat API and the Cloud Pub/Sub
   API:
   <https://console.cloud.google.com/flows/enableapi?apiid=chat.googleapis.com,pubsub.googleapis.com>
3. **Create the service account and its key.** *IAM & Admin → Service
   accounts → Create*, name it `holaclaw`, grant it **no project roles** (it
   only needs what step 5 grants, below). Then open it, *Keys → Add key →
   JSON*, and download the file — this is the key you give to holaclaw.
   - If the console refuses to create a key ("Service account key creation
     is disabled"), see [Allow key creation for this project](#allow-key-creation-for-this-project)
     below, then come back to this step.
4. **Create the topic.** Chat publishes its events here.
   1. *Pub/Sub → Topics → Create topic*, name it `holaclaw`, untick "Add a
      default subscription", and create it.
   2. Let Chat publish to it: in *Pub/Sub → Topics*, tick the **checkbox**
      next to `holaclaw` (don't click its name), click **Show info panel** at
      the top right if the panel is hidden, then **Add principal**.
   3. Principal: `chat-api-push@system.gserviceaccount.com`. Role: **Pub/Sub
      Publisher**. Click **Save**.
      - If the console says the **Domain-restricted sharing** policy is
        enforced, see
        [Allow Chat to publish to the topic](#allow-chat-to-publish-to-the-topic)
        below, then come back to this step.

   > **Check:** in Cloud Shell (the `>_` icon at the top right of the
   > console), `gcloud pubsub topics get-iam-policy holaclaw` must list
   > `roles/pubsub.publisher` with `chat-api-push@system.gserviceaccount.com`.
5. **Create the subscription.** HolaClaw pulls the events from here.
   1. *Pub/Sub → Subscriptions → Create subscription*, name it `holaclaw`
      (exactly this name: holaclaw always looks for it), topic `holaclaw`.
      Delivery type **Pull**, ack deadline 60 s, expiration **Never expire**.
      Create it.
   2. Let the service account pull from it. This grant goes on the
      **subscription**, not the topic: in *Pub/Sub → Subscriptions*, tick
      the **checkbox** next to `holaclaw` (don't click its name), click
      **Show info panel** if the panel is hidden, then **Add principal**.
   3. Principal: the service account's email, the `client_email` field of
      the key file (`holaclaw@<project>.iam.gserviceaccount.com`). Role:
      **Pub/Sub Subscriber**. Click **Save**.

   > **Check:** in Cloud Shell, `gcloud pubsub subscriptions get-iam-policy
   > holaclaw` must list `roles/pubsub.subscriber` with that account. Without
   > this grant, `auth set` fails with "cannot pull from subscription".
6. **Configure the Chat app.** *APIs & Services → Google Chat API →
   Configuration*:
   <https://console.cloud.google.com/apis/api/chat.googleapis.com/hangouts-chat>
   - Untick **Build this Chat app as a Google Workspace add-on**.
   - Fill in **app name, avatar URL, description** — what people see in Chat.
     The app name is how people find the app.
   - Turn on **Interactive features**, and tick **Join spaces and group
     conversations**. Without it, the app works in DMs only.
   - Under **Connection settings**, choose **Cloud Pub/Sub** and give the
     topic as `projects/<project>/topics/holaclaw`.
   - Under **Visibility**, add the people or groups in your domain who may
     find and message the app.
   - Save.

There is no API for step 6, so it always stays a console step, whatever
holaclaw does. The rest are ordinary Cloud resources you could script, but
there is no `holaclaw setup` that does it for you: creating them needs a
Google token with Cloud-wide permissions, which holaclaw does not ask for.

### Setup checklist

- [ ] Project created and a billing account linked (step 1)
- [ ] Google Chat API and Cloud Pub/Sub API enabled (step 2)
- [ ] Service account `holaclaw` created, with no project roles, and its JSON
      key downloaded (step 3)
- [ ] Topic `holaclaw` created, with **Pub/Sub Publisher** for
      `chat-api-push@system.gserviceaccount.com` (step 4)
- [ ] Subscription `holaclaw` created on that topic, delivery **Pull**, with
      **Pub/Sub Subscriber** for the service account (step 5)
- [ ] Chat app configured: add-on unticked, **Join spaces and group
      conversations** ticked, Cloud Pub/Sub topic
      `projects/<project>/topics/holaclaw`, Visibility set, saved (step 6)

### Common issues

Organisations created since 2024 enforce two security policies by default
that block steps 3 and 4. Older organisations may enforce them too. The fix
is to lift the policy for the bot's project only, never for the whole
organisation. A project used only by the bot keeps the exception narrow.

#### Allow key creation for this project

The policy **Disable service account key creation** blocks step 3. It exists
to keep long-lived keys out of an organisation, and holaclaw needs one key.

1. **Get the role to change policies.** You need **Organization Policy
   Administrator** (`roles/orgpolicy.policyAdmin`) on the *organisation*. A
   Workspace super admin doesn't have it by default. Switch the project
   picker at the top of the console to the organisation, open *IAM & Admin →
   IAM*, and grant yourself the role. Or ask whoever administers your Google
   Cloud organisation to do the steps below.
2. **Open the policies of the bot's project.** Switch the project picker
   back to the bot's **project** (not the organisation, so the change applies
   to this project alone), then open *IAM & Admin → Organization policies*:
   <https://console.cloud.google.com/iam-admin/orgpolicies>
3. **Find the policy.** Filter for `disableServiceAccountKeyCreation`. You
   may see two policies:
   - **Disable service account key creation**
     (`iam.managed.disableServiceAccountKeyCreation`)
   - the legacy one (`iam.disableServiceAccountKeyCreation`)

   Repeat the next step for each one that shows **Enforced**.
4. **Lift it.** Open the policy, click **Manage policy**, choose **Override
   parent's policy**, add (or edit) the rule so **Enforcement** is **Off**,
   and click **Set policy**.
5. **Wait a few minutes**, then go back to step 3 and create the key. Keep
   this exception: rotating the key later (§9) needs it again.

<details>
<summary>Alternative: gcloud (Cloud Shell)</summary>

Replace `PROJECT_ID`:

```
# The managed policy (organisations created since 2024)
cat > policy.yaml <<'EOF'
name: projects/PROJECT_ID/policies/iam.managed.disableServiceAccountKeyCreation
spec:
  rules:
  - enforce: false
EOF
gcloud org-policies set-policy policy.yaml

# The legacy policy (older organisations)
gcloud resource-manager org-policies disable-enforce \
  iam.disableServiceAccountKeyCreation --project=PROJECT_ID
```

</details>

#### Allow Chat to publish to the topic

The policy **Domain-restricted sharing** blocks the publisher grant of step 4.
It only lets accounts from your own domain be granted roles, and Chat
publishes as Google's own account, `chat-api-push@system.gserviceaccount.com`.
Your service account from step 3 belongs to your organisation, so the
subscription grant of step 5 is not affected. The policy only checks new
grants, so you lift it just for the moment you add the principal.

1. **Get the role to change policies.** You need **Organization Policy
   Administrator** (`roles/orgpolicy.policyAdmin`) on the *organisation*. A
   Workspace super admin doesn't have it by default. Switch the project
   picker at the top of the console to the organisation, open *IAM & Admin →
   IAM*, and grant yourself the role. Or ask whoever administers your Google
   Cloud organisation to do the steps below.
2. **Open the policies of the bot's project.** Switch the project picker
   back to the bot's **project** (not the organisation, so the change applies
   to this project alone), then open *IAM & Admin → Organization policies*:
   <https://console.cloud.google.com/iam-admin/orgpolicies>
3. **Find the policy.** Filter for **Domain restricted sharing**
   (`iam.allowedPolicyMemberDomains`). In some organisations the policy
   enforced is **Restrict allowed policy members in IAM allow policies**
   (`iam.managed.allowedPolicyMembers`) instead. Use the one your error
   message names.
4. **Lift it.** Open it, click **Manage policy**, choose **Override parent's
   policy**, then:
   - for `iam.allowedPolicyMemberDomains`: under **Policy enforcement**
     choose **Replace**, add a rule set to **Allow All**;
   - for `iam.managed.allowedPolicyMembers`: set the rule's
     **Enforcement** to **Off**.

   Click **Set policy**.
5. **Add the grant.** Wait a few minutes, then go back to step 4 and add the
   publisher.
6. **Restore the policy.** Once the grant is saved, open the policy again,
   **Manage policy → Inherit parent's policy → Set policy**. The grant you
   just made stays. If you edit the topic's permissions later and the error
   comes back, lift the policy again for the change.

<details>
<summary>Alternative: gcloud (Cloud Shell)</summary>

Replace `PROJECT_ID`:

```
# Lift it on the project
cat > policy.yaml <<'EOF'
name: projects/PROJECT_ID/policies/iam.allowedPolicyMemberDomains
spec:
  rules:
  - allowAll: true
EOF
gcloud org-policies set-policy policy.yaml

# ... add the publisher to the topic (step 4), then restore inheritance
gcloud org-policies delete iam.allowedPolicyMemberDomains --project=PROJECT_ID
```

</details>

## 3. Configure holaclaw

Add a channel instance in `channels.toml`:

```toml
# channels.toml
[channels.googlechat]
type   = "googlechat"
agent  = "main"
[channels.googlechat.access]
dm     = "invite"               # invite | open
rooms  = "approved"             # off | approved | open
reply  = "mention"              # mention only — Chat delivers nothing else
```

The access settings work as on every channel ([identity.md](../identity.md)
§2). On Chat they mean:

| Setting | Values | What it does on Chat |
|---|---|---|
| `dm` | `invite` (default), `open` | What happens to a DM from someone holaclaw doesn't know yet. `invite` ignores it unless it is a valid invite code. `open` answers at once, as a pending identity: agent memory only, read-only tools, rate-limited. |
| `rooms` | `off`, `approved`, `open` | The same for a space. `off` never answers there. `approved` answers once the space is approved: by a linked owner or admin adding the app or @-mentioning it, or by a room invite (§6). `open` answers in any space the app is in. |
| `reply` | `mention` | Chat only delivers a message from a space when the app is @-mentioned, so `always` can never work here, and `holaclaw check` warns about it (ACC003). Every DM message reaches the app. |

**Visibility is a second gate, above these settings**, set in the Cloud
console (§2 step 6), not in `channels.toml`. Only the people and groups you
listed there can find the app and message it at all; the settings above then
decide what holaclaw does with messages from people who *can* reach it. A
narrow Visibility list plus `open` is a reasonable setup for an internal team
bot.

A second bot is a second Cloud project, with its own `holaclaw` subscription
and its own `[channels.<name>]` table here. The subscription name never
collides, because each app lives in its own project.

## 4. Set the key

Pipe the downloaded JSON key file into `auth set`:

```
holaclaw auth set channels.googlechat service_account < holaclaw-key.json
```

This reads the key, checks it with Google, and stores it in `auth.toml`. If a
check fails, nothing is stored and the message names the setup step to redo
(see below). Run `holaclaw check` afterwards to confirm the key is in place.

`--no-verify` skips the check. We don't recommend it: the check catches real
setup mistakes that would otherwise only show up once the daemon runs. If
Google can't be reached at all, the key is stored anyway, with a warning.

The key says which Cloud project it belongs to, and holaclaw always uses the
subscription `holaclaw` in that project, so `channels.toml` needs no project
id.

### Common issues

| `auth set` says | What to do |
|---|---|
| "is not a service-account key" | You piped the wrong file, or a truncated one. Download the JSON key again (§2 step 3). |
| "Google rejected this key: it was deleted or disabled" | The key was revoked or the service account removed. Create a new key (§2 step 3; see rotation in §9). |
| "no subscription `holaclaw` in project `p`" | The subscription doesn't exist, or has another name or project. Redo §2 step 5; the name must be exactly `holaclaw`. |
| "the service account … cannot pull from subscription `holaclaw`" | The **Pub/Sub Subscriber** grant is missing on the subscription, or was given to another account (or to the topic). Redo §2 step 5, sub-steps 2–3, with the `client_email` from the key file, and run the check below that step. A new grant can take a few minutes to apply. |
| "the Chat app is not configured" | §2 step 6 was skipped or not saved. Redo it. |
| "could not reach Google … storing it unchecked" | This machine can't reach Google right now. Once it can, start the daemon and check `holaclaw status` (§5). |

## 5. Check and start

```
holaclaw check
```

This validates `channels.toml` and `auth.toml` together: SEC003 fails if
`service_account` is missing, ACC003 warns if `reply = "always"` is set on a
googlechat instance.

Start or [restart](../deployments/README.md#restart) the daemon, then watch
the channel's status:

```
holaclaw status
```

Status values for this channel, same vocabulary as every channel:

| Status | Meaning |
|---|---|
| `unlinked` | Where every channel starts. With a key set, it turns `connected` within a second or two. If it stays, no key is set: run `auth set` (§4), no restart needed. |
| `connected` | The key may pull from the subscription. Checked when the daemon starts and after any failure. |
| `logged_out` | Google refused the key, or the key may not pull from the subscription. Retried every 60 s, so fixing the key or the grant is enough, no restart. |
| `reconnecting` | Any other failure, including a missing subscription. Retried with backoff. |

## 6. First contact

The app's name in Chat is the **App name** you entered in §2 step 6. Only
the people and groups listed under **Visibility** on that page can find it.
A change to Visibility can take a few minutes to reach Chat.

### Talk to the app in a DM

1. Mint a one-shot code on the machine running holaclaw:

   ```
   holaclaw identity list                         # the id for the next command
   holaclaw invite create --identity <id> --channel googlechat
   ```

   It prints the code, the channel and its expiry.

   An invite for the owner is **only for the owner**, linking their own
   Chat account: whoever redeems the code acts as the owner. Never send an
   owner code to anybody else. For anybody else, create their identity
   with its first code: `holaclaw identity create --name Carla --invite
   googlechat` (`--role admin` for another admin, so they can approve tool
   calls from Chat). The code is valid for 15 minutes (`--ttl` changes
   that, up to 1 h, or 24 h for a new identity) and works once.
2. Open Google Chat (<https://chat.google.com>, or Chat inside Gmail).
3. Click **New chat** (the `+` next to "Chat" at the top left), type the
   app's name in the search box, and pick the app from the results. If it
   doesn't show up, your account isn't in the app's Visibility list yet, or
   the change hasn't reached Chat yet.
4. In the DM that opens, send the code as the **only** text of the message.
   The app links your Chat account and answers. The code is never stored or
   shown to the agent.
5. From then on, write to the app in that DM like to anyone else.

With `dm = "open"`, steps 1 and 4 are optional: the app answers strangers as
pending identities (§3).

### Add the app to a space

In a space, the app only sees messages that @-mention it, and it answers
only once the space is approved (`rooms = "approved"`, §3). Add it and
approve the space in one of two ways.

**As a linked owner or admin** (you redeemed an owner or admin code in a DM
above):

1. Open the space in Google Chat.
2. Add the app from the space's menu: the space name → **Apps &
   integrations** → **Add apps**, pick the app and confirm.
3. Because you are a linked owner or admin, adding it approves the space, and
   the app posts "This room is approved. Say hi!" there. From then on it
   answers @-mentions in that space.

You can also add it by @-mentioning it: type `@` followed by the app's name,
pick the app from the list, add a question after it, and send. Chat asks
whether to add the app: confirm. The app posts the same notice and then
answers your question.

If someone who is not a linked owner or admin adds the app, it joins the
space but does **not** approve it, and posts nothing. It stays silent until a
linked owner or admin @-mentions it there or someone sends a room invite
(below).

**With a room invite** (whoever sends the code needs no account link):

1. Mint a room code:

   ```
   holaclaw invite create --room --channel googlechat
   ```

2. Open the space, type `@` and the app's name, pick the app (confirm
   adding it if Chat asks), then type the code after the mention and send:
   `@AppName <code>`.
3. That approves the space. It links nobody: everyone in it who hasn't
   linked their own account talks to the app as a pending identity.

### Approvals from Chat

A tool call that needs approval in a conversation with an owner or admin
shows up in that conversation as a card with four buttons: once, session,
always and deny. Press one. The card is then rewritten to show the outcome
(§7).

## 7. Daily use

- **DMs** work like any other channel: every message the app receives in a
  DM reaches the agent (subject to `dm`), no @-mention needed.
- **Spaces** only deliver @-mentions to the app — that's a Chat platform
  limit, not a holaclaw setting, so every turn in a space starts with
  `@app ...`.
- **Threads are separate sessions.** A top-level message and each thread in a
  space keep their own session; a space with no threads is one session for
  the whole room.
- **Approvals arrive as cards.** A tool call needing approval posts a card
  with four buttons (once / session / always / deny). Only a press from an
  owner or admin counts. Once a press is accepted, the card is rewritten in
  place to "`Approved (<choice>) by <name>`" or "`Denied by <name>`" and the
  buttons are gone. A press that is not accepted (someone else, or an
  approval already settled) leaves the card untouched.
- **Files in, not out.** Images and documents sent to the app are downloaded
  (capped at 20 MiB) and attached to the turn. Files kept in Drive rather
  than uploaded directly are never downloaded — the agent sees
  `[Drive file not downloaded: name]` instead. The app cannot send files
  back: any attachment in a reply becomes a text notice
  ("`<name>: a Google Chat app cannot send files`").
- **No typing indicator.** Chat has no typing API; you just wait for the
  reply, same as waiting for any other slow message.
- **Long replies split** at 4000 characters, Chat's own markdown subset
  (`*bold*`, `_italic_`, `~strike~`, `` `code` ``, `<url|text>`).

## 8. Limits

- Spaces only ever see @-mentions — there is no "always reply" mode here.
- The app cannot send files.
- No typing indicator.
- Replies are asynchronous only; Chat dialogs are out of scope.
- Pub/Sub keeps undelivered events for 7 days — a daemon down longer than
  that loses what arrived while it was off.
- Chat's own quota is 60 messages per space per minute; a very long reply
  that splits into many chunks can bump into this in a busy space.

## 9. Security notes

- **The key can do exactly two things**: pull from the one `holaclaw`
  subscription, and post as the app. The service account holds no project
  roles — Subscriber is granted on the subscription alone. Someone who gets
  the key can read the app's events and speak as it until the key is
  deleted.
- **The key lives in `auth.toml`** (mode 0600), the same as every other
  secret: never in config, backups by default, logs, prompts or traces.
- **Inbound events are trusted by construction**, not by a signature: only
  Chat can publish to the topic (IAM), and only the service account can read
  the subscription. There is nothing to verify per-request, unlike a webhook.
- **Rotating the key** needs no downtime and no restart:
  1. In the console, create a new JSON key on the same service account
     (*IAM & Admin → Service accounts → holaclaw → Keys → Add key → JSON*).
  2. `holaclaw auth set channels.googlechat service_account < new-key.json`
     — the daemon re-reads the key per request and picks it up at once.
  3. Once you've confirmed the channel is `connected` again, delete the old
     key in the console (*Keys → ⋮ → Delete*).

## 10. Troubleshooting

Problems with the key itself are listed under §4 Common issues, and blocked
Cloud setup steps under §2 Common issues.

| Symptom | What to check / redo |
|---|---|
| `holaclaw status` stays `unlinked` | No key is set, or it never made it into `auth.toml`. Run `auth set` again (§4); no restart needed. |
| `holaclaw status` shows `logged_out` | The key was refused, or it may not pull from the subscription. The status detail says which. Fix the key or the grant (§2 step 5); it is retried every 60 s, no restart needed. |
| `holaclaw status` shows `reconnecting` | A transient failure, or the subscription is missing. Recheck §2 step 5. |
| `holaclaw check`: SEC003 | `service_account` is missing from `auth.toml`. Run `auth set` (§4). |
| `holaclaw check`: ACC003 | `reply = "always"` is set on this instance. Set `reply = "mention"` in `channels.toml`. |
| The app can't be found in Chat | Your account isn't in the app's **Visibility** list (§2 step 6), or a recent change hasn't reached Chat yet; it can take a few minutes. |
| The app can't be added to a space | **Join spaces and group conversations** isn't ticked on the Chat app configuration (§2 step 6). |
| Events never arrive (connected, but nothing happens when you message the app) | In order: (1) the topic (not the subscription) must grant **Pub/Sub Publisher** to `chat-api-push@system.gserviceaccount.com` (§2 step 4); (2) **Connection settings** on the Chat app configuration must name the topic exactly `projects/<project>/topics/holaclaw` (§2 step 6); (3) the subscription must be named exactly `holaclaw` (§2 step 5). |
