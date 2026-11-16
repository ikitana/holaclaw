package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// eventsPayload is the events_api envelope payload.
type eventsPayload struct {
	EventID string     `json:"event_id"`
	TeamID  string     `json:"team_id"`
	Event   slackEvent `json:"event"`
}

// slackEvent is the subset of a message / app_mention event we map.
type slackEvent struct {
	Type        string      `json:"type"`
	Subtype     string      `json:"subtype"`
	Channel     string      `json:"channel"`
	ChannelType string      `json:"channel_type"`
	User        string      `json:"user"`
	BotID       string      `json:"bot_id"`
	Text        string      `json:"text"`
	TS          string      `json:"ts"`
	ThreadTS    string      `json:"thread_ts"`
	EventTS     string      `json:"event_ts"`
	Files       []slackFile `json:"files"`
}

// slackFile is one shared file.
type slackFile struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Title              string `json:"title"`
	Mimetype           string `json:"mimetype"`
	Size               int64  `json:"size"`
	URLPrivateDownload string `json:"url_private_download"`
	URLPrivate         string `json:"url_private"`
}

// handleEventsAPI maps one events_api payload to an envelope. It runs after
// the ack, so it may take as long as core needs.
func (a *Adapter) handleEventsAPI(ctx context.Context, in channel.Inbound, payload json.RawMessage) {
	var p eventsPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		a.log.Warn("slack: undecodable event payload", "err", a.redact(err.Error()))
		return
	}
	if p.EventID != "" && a.seen.add("event:"+p.EventID) {
		return // Slack retry of an event we already took
	}
	ev := p.Event
	if !a.wanted(ev) {
		return
	}
	// Slack delivers both `message` and `app_mention` for a mention in a
	// channel; they share the ts.
	if a.seen.add("msg:" + ev.Channel + ":" + ev.TS) {
		return
	}
	env, err := a.toEnvelope(ctx, p, ev)
	if err != nil {
		a.log.Error("slack: building envelope failed", "err", a.redact(err.Error()))
		return
	}
	// A duplicate was handled before: nothing to report.
	if err := in(ctx, env); err != nil && ctx.Err() == nil && !errors.Is(err, channel.ErrDuplicate) {
		a.log.Error("slack: inbound rejected", "err", a.redact(err.Error()))
	}
}

// wanted filters the event types and subtypes this adapter takes.
func (a *Adapter) wanted(ev slackEvent) bool {
	botUser, _ := a.identity()
	if ev.BotID != "" || (botUser != "" && ev.User == botUser) {
		return false // our own messages and every other bot
	}
	if ev.Channel == "" || ev.TS == "" {
		return false
	}
	switch ev.Type {
	case "app_mention":
		return true
	case "message":
		switch ev.Subtype {
		case "", "file_share", "thread_broadcast":
			return true
		default: // message_changed, message_deleted, bot_message, joins…
			return false
		}
	default:
		return false
	}
}

// conversationKind classifies by Slack's conversation type: only a one-person
// DM is a DM. mpim and any unknown type are rooms. Events that omit
// channel_type (app_mention, reactions) fall back to the D channel-id prefix.
func conversationKind(ev slackEvent) event.ConversationKind {
	switch ev.ChannelType {
	case "im":
		return event.ConversationDM
	case "":
		if strings.HasPrefix(ev.Channel, "D") {
			return event.ConversationDM
		}
	}
	return event.ConversationRoom
}

// toEnvelope maps a Slack event to the canonical inbound envelope.
func (a *Adapter) toEnvelope(ctx context.Context, p eventsPayload, ev slackEvent) (event.Envelope, error) {
	botUser, team := a.identity()
	if p.TeamID != "" {
		team = p.TeamID
	}
	kind := conversationKind(ev)
	mentioned := kind == event.ConversationDM || mentions(ev.Text, botUser)

	// A thread is set only when the user is already in one; a top-level
	// mention stays in the channel session, and Send answers it in a thread
	// rooted at the mention when reply_to asks for a native reply.
	// A thread's root is the message whose ts names it: the new thread
	// session is seeded with the turn that message started.
	thread, root := ev.ThreadTS, ""
	if thread != "" {
		root = ev.Channel + ":" + thread
	}

	text := markdownFromSlack(stripMention(ev.Text, botUser), func(id string) string {
		return a.displayName(ctx, id)
	})

	msg := event.Message{
		Schema:    event.SchemaVersion,
		ID:        event.NewID(),
		Role:      event.RoleUser,
		Text:      text,
		CreatedAt: tsTime(ev.TS),
	}
	for _, f := range ev.Files {
		att, err := a.fetchFile(ctx, f)
		if err != nil {
			a.log.Warn("slack: file not taken", "file", f.ID, "err", a.redact(err.Error()))
			continue
		}
		msg.Attachments = append(msg.Attachments, att)
	}

	return event.Envelope{
		Schema: event.SchemaVersion,
		Origin: event.Origin{
			Channel:          a.opt.Name,
			ChannelType:      "slack",
			Conversation:     ev.Channel,
			ConversationKind: kind,
			Thread:           thread,
			ThreadRoot:       root,
			PlatformMsgID:    ev.Channel + ":" + ev.TS,
		},
		Sender: event.Sender{
			Aliases:     []string{alias(team, ev.User)},
			DisplayName: a.displayName(ctx, ev.User),
		},
		Mentioned: mentioned,
		Message:   msg,
		Received:  time.Now().UTC(),
	}, nil
}

// markdownFromSlack is slackToMarkdown with the trimming callers expect.
func markdownFromSlack(s string, resolve func(string) string) string {
	return strings.TrimSpace(slackToMarkdown(s, resolve))
}

// alias is the channel identity form: slack:<team>:<user>.
func alias(team, user string) string { return "slack:" + team + ":" + user }

// tsTime converts a Slack ts ("1727090000.000200") to a time.
func tsTime(ts string) time.Time {
	secs, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Now().UTC()
	}
	var micros int64
	if frac != "" {
		micros, _ = strconv.ParseInt(frac, 10, 64)
	}
	return time.Unix(s, micros*1000).UTC()
}

// fetchFile downloads one file with the bot token and stores it as a blob.
func (a *Adapter) fetchFile(ctx context.Context, f slackFile) (event.Attachment, error) {
	if a.opt.PutBlob == nil {
		return event.Attachment{}, errors.New("no blob store configured")
	}
	src := firstNonEmpty(f.URLPrivateDownload, f.URLPrivate)
	if src == "" {
		return event.Attachment{}, errors.New("file has no download url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return event.Attachment{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.botToken())

	// The token travels on this request, so a redirect to another host is
	// refused rather than followed.
	hc := *a.hc
	hc.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if r.URL.Host != via[0].URL.Host {
			return ErrCrossHostRedirect
		}
		return nil
	}
	resp, err := hc.Do(req)
	if err != nil {
		if errors.Is(err, ErrCrossHostRedirect) {
			return event.Attachment{}, ErrCrossHostRedirect
		}
		return event.Attachment{}, errors.New(a.redact(err.Error()))
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return event.Attachment{}, errors.New("download failed: http " + strconv.Itoa(resp.StatusCode))
	}

	mimeType := firstNonEmpty(f.Mimetype, resp.Header.Get("Content-Type"), "application/octet-stream")
	blobID, size, err := a.opt.PutBlob(ctx, &capReader{r: resp.Body, left: a.opt.MaxDownload}, mimeType)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return event.Attachment{}, ErrTooLarge
		}
		return event.Attachment{}, errors.New(a.redact(err.Error()))
	}
	return event.Attachment{
		ID:     event.NewID(),
		BlobID: blobID,
		MIME:   mimeType,
		Name:   firstNonEmpty(f.Name, f.Title, f.ID),
		Size:   size,
		SHA256: blobID,
		Kind:   event.KindFromMIME(mimeType),
	}, nil
}

// capReader fails the read once the download cap is exceeded, so an oversize
// file never reaches the blob store whole.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, ErrTooLarge
	}
	return n, err
}
