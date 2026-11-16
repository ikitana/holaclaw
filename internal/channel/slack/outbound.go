package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// postMessageReq is a prose reply: plain mrkdwn, no Block Kit (docs/channels).
type postMessageReq struct {
	Channel  string `json:"channel"`
	Text     string `json:"text"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Mrkdwn   bool   `json:"mrkdwn"`
	Blocks   []any  `json:"blocks,omitempty"`
}

type postMessageResp struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// Send delivers one reply: text in ~4000-char chunks, then each attachment
// through the files.uploadV2 flow. Attachment failures degrade, they do not
// fail the send. Slack's native reply is a thread: when Target.PlatformMsgID
// names a message and the target is not in a thread already, the whole reply
// goes into a thread rooted at that message.
func (a *Adapter) Send(ctx context.Context, out channel.Outbound) (channel.Receipt, error) {
	var rec channel.Receipt
	ch := out.Target.Conversation
	target := out.Target
	if target.Thread == "" {
		target.Thread = replyTS(target)
	}
	chunks := chunkText(markdownToMrkdwn(out.Message.Text), chunkChars)

	// A short text rides along as the first upload's comment instead of a
	// separate message.
	comment := ""
	if len(out.Message.Attachments) > 0 && len(chunks) == 1 {
		comment, chunks = chunks[0], nil
	}

	for _, c := range chunks {
		ts, err := a.postReply(ctx, &target, out.Target.Thread, c)
		if err != nil {
			return rec, err
		}
		rec.PlatformMsgIDs = append(rec.PlatformMsgIDs, ch+":"+ts)
	}

	for i, att := range out.Message.Attachments {
		initial := ""
		if i == 0 {
			initial = comment
		}
		ts, err := a.upload(ctx, target, att, initial)
		if err != nil {
			rec.Degraded = append(rec.Degraded, fmt.Sprintf("%s: %s", att.Name, a.redact(err.Error())))
			if i == 0 && comment != "" {
				// the comment never went out with the file; send it as text
				if mts, perr := a.postReply(ctx, &target, out.Target.Thread, comment); perr == nil {
					rec.PlatformMsgIDs = append(rec.PlatformMsgIDs, ch+":"+mts)
				}
				comment = ""
			}
			continue
		}
		if ts != "" {
			rec.PlatformMsgIDs = append(rec.PlatformMsgIDs, ch+":"+ts)
		}
	}
	return rec, nil
}

// replyTS is the ts of the "<channel>:<ts>" message a reply answers, when it
// is in the target conversation; "" otherwise.
func replyTS(t event.Origin) string {
	ch, ts, ok := strings.Cut(t.PlatformMsgID, ":")
	if !ok || ch != t.Conversation || ts == "" {
		return ""
	}
	return ts
}

// postReply posts one chunk of a reply. When the thread was opened for the
// reply (it differs from the one the user wrote in) and its root is gone, the
// rest of the reply goes to the conversation instead.
func (a *Adapter) postReply(ctx context.Context, target *event.Origin, userThread, text string) (string, error) {
	req := postMessageReq{Channel: target.Conversation, Text: text, ThreadTS: target.Thread, Mrkdwn: true}
	ts, err := a.postMessage(ctx, req)
	var se *Error
	if err != nil && target.Thread != userThread && errors.As(err, &se) && se.Code == "thread_not_found" {
		target.Thread = userThread
		req.ThreadTS = userThread
		ts, err = a.postMessage(ctx, req)
	}
	return ts, err
}

func (a *Adapter) postMessage(ctx context.Context, req postMessageReq) (string, error) {
	var r postMessageResp
	if err := a.callJSON(ctx, "chat.postMessage", a.botToken(), req, &r); err != nil {
		return "", err
	}
	return r.TS, nil
}

// upload runs the three-step external upload: reserve a URL, POST the bytes,
// complete into the conversation.
func (a *Adapter) upload(ctx context.Context, target event.Origin, att event.Attachment, comment string) (string, error) {
	if a.opt.Blobs == nil {
		return "", errors.New("no blob opener configured")
	}
	data, err := a.opt.Blobs.OpenAttachment(ctx, att)
	if err != nil {
		return "", err
	}
	if int64(len(data)) > maxUploadBytes {
		return "", fmt.Errorf("%w (%d bytes, cap %d)", ErrTooLarge, len(data), int64(maxUploadBytes))
	}
	name := att.Name
	if name == "" {
		name = string(att.ID)
	}

	var got struct {
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	form := url.Values{"filename": {name}, "length": {strconv.Itoa(len(data))}}
	if err := a.callForm(ctx, "files.getUploadURLExternal", a.botToken(), form, &got); err != nil {
		return "", err
	}
	if got.UploadURL == "" || got.FileID == "" {
		return "", errors.New("files.getUploadURLExternal: no upload url")
	}
	if err := a.putBytes(ctx, got.UploadURL, data); err != nil {
		return "", err
	}

	type fileRef struct {
		ID    string `json:"id"`
		Title string `json:"title,omitempty"`
	}
	req := struct {
		Files          []fileRef `json:"files"`
		ChannelID      string    `json:"channel_id"`
		ThreadTS       string    `json:"thread_ts,omitempty"`
		InitialComment string    `json:"initial_comment,omitempty"`
	}{
		Files:          []fileRef{{ID: got.FileID, Title: name}},
		ChannelID:      target.Conversation,
		ThreadTS:       target.Thread,
		InitialComment: comment,
	}
	var done completeResp
	if err := a.callJSON(ctx, "files.completeUploadExternal", a.botToken(), req, &done); err != nil {
		return "", err
	}
	for _, f := range done.Files {
		for _, shares := range []map[string][]share{f.Shares.Public, f.Shares.Private} {
			for _, s := range shares {
				if len(s) > 0 && s[0].TS != "" {
					return s[0].TS, nil
				}
			}
		}
	}
	return "", nil
}

// share is one conversation a completed upload was posted to.
type share struct {
	TS string `json:"ts"`
}

type completeResp struct {
	Files []struct {
		ID     string `json:"id"`
		Shares struct {
			Public  map[string][]share `json:"public"`
			Private map[string][]share `json:"private"`
		} `json:"shares"`
	} `json:"files"`
}

// putBytes uploads the file body to the URL Slack handed out.
func (a *Adapter) putBytes(ctx context.Context, uploadURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(data))
	if err != nil {
		// The URL is signed: its query is a credential, so it never travels
		// out of here unredacted, not even in a malformed-URL error.
		return errors.New(a.redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := a.hc.Do(req)
	if err != nil {
		return errors.New(a.redact(err.Error()))
	}
	defer resp.Body.Close() //nolint:errcheck // status is all we need
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("upload failed: http %d", resp.StatusCode)
	}
	return nil
}

// --- approvals -----------------------------------------------------------

// PromptApproval renders the request with one button per choice. The button
// value carries the approval id so no adapter-side state is needed.
func (a *Adapter) PromptApproval(ctx context.Context, target event.Origin, approvalID event.ID, text string, choices []string) error {
	elements := make([]any, 0, len(choices))
	for _, c := range choices {
		elements = append(elements, map[string]any{
			"type":      "button",
			"text":      map[string]any{"type": "plain_text", "text": c},
			"action_id": approvalActionID,
			"value":     string(approvalID) + ":" + c,
		})
	}
	blocks := []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": markdownToMrkdwn(text)}},
		map[string]any{"type": "actions", "elements": elements},
	}
	_, err := a.postMessage(ctx, postMessageReq{
		Channel:  target.Conversation,
		ThreadTS: target.Thread,
		Text:     markdownToMrkdwn(text),
		Mrkdwn:   true,
		Blocks:   blocks,
	})
	return err
}

// interaction is the block_actions payload we read.
type interaction struct {
	Type string `json:"type"`
	User struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"user"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
		Text     string `json:"text"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// handleInteractive reports the pressed choice and rewrites the message so
// the buttons are gone and the decision is on the record.
func (a *Adapter) handleInteractive(ctx context.Context, payload json.RawMessage) {
	var p interaction
	if err := json.Unmarshal(payload, &p); err != nil {
		a.log.Warn("slack: undecodable interaction", "err", a.redact(err.Error()))
		return
	}
	if p.Type != "block_actions" {
		return
	}
	for _, act := range p.Actions {
		if act.ActionID != approvalActionID {
			continue
		}
		id, choice, ok := strings.Cut(act.Value, ":")
		if !ok {
			a.log.Warn("slack: malformed approval value")
			continue
		}
		_, team := a.identity()
		if p.Team.ID != "" {
			team = p.Team.ID
		}
		if a.opt.OnApproval != nil {
			a.opt.OnApproval(ctx, event.ID(id), choice, []string{alias(team, p.User.ID)})
		}
		a.settleApproval(ctx, p, choice)
	}
}

// settleApproval removes the buttons and appends the decision.
func (a *Adapter) settleApproval(ctx context.Context, p interaction, choice string) {
	decided := p.Message.Text + "\n\n*" + escapeEntities(choice) + "* — <@" + p.User.ID + ">"
	req := struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
		Text    string `json:"text"`
		Blocks  []any  `json:"blocks"`
	}{
		Channel: p.Channel.ID,
		TS:      p.Message.TS,
		Text:    decided,
		Blocks: []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": decided}},
		},
	}
	if err := a.callJSON(ctx, "chat.update", a.botToken(), req, nil); err != nil {
		a.log.Warn("slack: chat.update failed", "err", a.redact(err.Error()))
	}
}
