package googlechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// Chat interaction event types holaclaw handles.
const (
	evMessage     = "MESSAGE"
	evAdded       = "ADDED_TO_SPACE"
	evRemoved     = "REMOVED_FROM_SPACE"
	evCardClicked = "CARD_CLICKED"
)

// wireEvent is a Chat interaction event as Chat publishes it to Pub/Sub.
type wireEvent struct {
	Type      string        `json:"type"`
	EventTime flexTime      `json:"eventTime"`
	Space     *wireSpace    `json:"space"`
	Message   *wireMessage  `json:"message"`
	User      *wireUser     `json:"user"`
	Action    *wireAction   `json:"action"`
	Common    *wireCommon   `json:"common"`
	Thread    *wireThreadID `json:"thread"`
}

// wireSpace accepts both the current spaceType and the legacy type field.
type wireSpace struct {
	Name            string `json:"name"`
	Type            string `json:"type"`      // legacy: ROOM | DM
	SpaceType       string `json:"spaceType"` // SPACE | GROUP_CHAT | DIRECT_MESSAGE
	SingleUserBotDm bool   `json:"singleUserBotDm"`
	DisplayName     string `json:"displayName"`
}

// isDM follows spaceType when it is set, so an unknown type is a room. The
// legacy type and singleUserBotDm fields apply only to events that carry no
// spaceType.
func (s *wireSpace) isDM() bool {
	if s.SpaceType != "" {
		return s.SpaceType == "DIRECT_MESSAGE"
	}
	return s.Type == "DM" || s.SingleUserBotDm
}

type wireUser struct {
	Name        string `json:"name"` // users/<id>
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Type        string `json:"type"` // HUMAN | BOT
}

type wireThreadID struct {
	Name string `json:"name"`
}

type wireMessage struct {
	Name         string           `json:"name"` // spaces/<s>/messages/<m>
	Sender       *wireUser        `json:"sender"`
	CreateTime   flexTime         `json:"createTime"`
	Text         string           `json:"text"`
	ArgumentText *string          `json:"argumentText"` // text without the app's mention
	Annotations  []wireAnnotation `json:"annotations"`
	Thread       *wireThreadID    `json:"thread"`
	ThreadReply  bool             `json:"threadReply"`
	SlashCommand *json.RawMessage `json:"slashCommand"`
	Attachment   []wireAttachment `json:"attachment"`
}

type wireAnnotation struct {
	Type        string `json:"type"` // USER_MENTION | SLASH_COMMAND | ...
	UserMention *struct {
		User *wireUser `json:"user"`
		Type string    `json:"type"` // ADD | MENTION
	} `json:"userMention"`
}

type wireAttachment struct {
	Name              string `json:"name"`
	ContentName       string `json:"contentName"`
	ContentType       string `json:"contentType"`
	Source            string `json:"source"` // UPLOADED_CONTENT | DRIVE_FILE
	AttachmentDataRef *struct {
		ResourceName string `json:"resourceName"`
	} `json:"attachmentDataRef"`
	DriveDataRef *struct {
		DriveFileID string `json:"driveFileId"`
	} `json:"driveDataRef"`
}

type wireParam struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type wireAction struct {
	ActionMethodName string      `json:"actionMethodName"`
	Parameters       []wireParam `json:"parameters"`
}

type wireCommon struct {
	InvokedFunction string            `json:"invokedFunction"`
	Parameters      map[string]string `json:"parameters"`
}

// flexTime reads a protobuf Timestamp in either JSON form: an RFC 3339
// string or {"seconds":..,"nanos":..}. Anything else is the zero time, never a
// parse failure.
type flexTime struct{ time.Time }

func (t *flexTime) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		if v, err := time.Parse(time.RFC3339Nano, s); err == nil {
			t.Time = v.UTC()
		}
		return nil
	}
	var o struct {
		Seconds json.Number `json:"seconds"`
		Nanos   int64       `json:"nanos"`
	}
	if json.Unmarshal(b, &o) == nil {
		if sec, err := o.Seconds.Int64(); err == nil {
			t.Time = time.Unix(sec, o.Nanos).UTC()
		}
	}
	return nil
}

// sortTime orders a batch: the message's createTime, else the event's.
func (ev *wireEvent) sortTime() time.Time {
	if ev.Message != nil && !ev.Message.CreateTime.IsZero() {
		return ev.Message.CreateTime.Time
	}
	return ev.EventTime.Time
}

// handleEvent handles one decoded event. An error means Inbound refused the
// envelope and the event must be redelivered.
func (a *Adapter) handleEvent(ctx context.Context, in channel.Inbound, ev *wireEvent) error {
	switch ev.Type {
	case evMessage:
		return a.handleMessage(ctx, in, ev)
	case evAdded:
		a.handleAdded(ctx, ev)
		if ev.Message != nil {
			// Added through an @-mention: the mention is a message too.
			return a.handleMessage(ctx, in, ev)
		}
	case evRemoved:
		a.log.Info("googlechat app removed from a space", "space", ev.Space.Name)
	case evCardClicked:
		a.handleCardClick(ctx, ev)
	}
	return nil
}

// handleMessage routes one message, once.
func (a *Adapter) handleMessage(ctx context.Context, in channel.Inbound, ev *wireEvent) error {
	m := ev.Message
	if m.Name == "" || a.seen.has("msg:"+m.Name) {
		return nil
	}
	env, ok := a.envelope(ctx, ev)
	if !ok {
		return nil
	}
	// A duplicate was handled before: acked like a success.
	if err := in(ctx, env); err != nil && !errors.Is(err, channel.ErrDuplicate) {
		return err
	}
	a.seen.add("msg:" + m.Name)
	return nil
}

// handleAdded reports the app joining a space. A DM is not a room to approve.
func (a *Adapter) handleAdded(ctx context.Context, ev *wireEvent) {
	if ev.Space.isDM() || ev.User == nil || ev.User.Type == "BOT" || a.opt.OnJoined == nil {
		return
	}
	// Redelivery after a refused mention must not report the join twice.
	key := "join:" + ev.Space.Name + "|" + ev.EventTime.Format(time.RFC3339Nano)
	if a.seen.has(key) {
		return
	}
	a.seen.add(key)
	a.opt.OnJoined(ctx, ev.Space.Name, aliases(ev.User))
}

// envelope maps a message event onto an Envelope.
func (a *Adapter) envelope(ctx context.Context, ev *wireEvent) (event.Envelope, bool) {
	m := ev.Message
	snd := m.Sender
	if snd == nil {
		snd = ev.User
	}
	if snd == nil || snd.Name == "" {
		// Acked, or it would come back forever. Only the reason is logged.
		a.log.Warn("googlechat message dropped", "reason", "no sender", "space", ev.Space.Name)
		return event.Envelope{}, false
	}
	kind := event.ConversationRoom
	if ev.Space.isDM() {
		kind = event.ConversationDM
	}
	thread := ""
	if m.ThreadReply && m.Thread != nil {
		thread = m.Thread.Name
	}

	// argumentText is the text with the app's mention removed. Unverified:
	// whether Chat sets it in DMs; when it is absent the full text is used.
	text := strings.TrimSpace(m.Text)
	if m.ArgumentText != nil {
		text = strings.TrimSpace(*m.ArgumentText)
	}

	atts, notes := a.attachments(ctx, m.Attachment)
	if len(notes) > 0 {
		text = strings.TrimSpace(text + "\n" + strings.Join(notes, "\n"))
	}

	created := m.CreateTime.Time
	if created.IsZero() {
		created = time.Now().UTC()
	}
	return event.Envelope{
		Schema: event.SchemaVersion,
		Origin: event.Origin{
			Channel:          a.opt.Name,
			ChannelType:      a.Type(),
			Conversation:     ev.Space.Name,
			ConversationKind: kind,
			Thread:           thread,
			PlatformMsgID:    m.Name,
		},
		Sender: event.Sender{
			Aliases:     aliases(snd),
			DisplayName: snd.DisplayName,
			// A Chat app is not sent its own messages, but a BOT sender can
			// only be an app: marking it FromSelf keeps two apps from
			// answering each other in a loop.
			FromSelf: snd.Type == "BOT",
		},
		Mentioned: kind == event.ConversationDM || mentionsApp(m),
		Message: event.Message{
			Schema:      event.SchemaVersion,
			ID:          event.NewID(),
			Role:        event.RoleUser,
			Text:        text,
			Attachments: atts,
			CreatedAt:   created,
		},
		Received: time.Now().UTC(),
	}, true
}

// aliases are the authenticated ids Chat gives for a user: users/<id> and,
// when Chat includes it, the email.
func aliases(u *wireUser) []string {
	if u == nil || u.Name == "" {
		return nil
	}
	out := []string{u.Name}
	if e := strings.ToLower(strings.TrimSpace(u.Email)); e != "" {
		out = append(out, e)
	}
	return out
}

// mentionsApp reports whether the message addresses the app: a USER_MENTION
// of a BOT user (in a space Chat only delivers the app's own mentions), or a
// slash command.
func mentionsApp(m *wireMessage) bool {
	if m.SlashCommand != nil {
		return true
	}
	for _, an := range m.Annotations {
		if an.Type == "USER_MENTION" && an.UserMention != nil && an.UserMention.User != nil &&
			an.UserMention.User.Type == "BOT" {
			return true
		}
	}
	return false
}

// attachments downloads uploaded files and returns notes for what could not
// be taken (docs/sessions.md).
func (a *Adapter) attachments(ctx context.Context, list []wireAttachment) ([]event.Attachment, []string) {
	var atts []event.Attachment
	var notes []string
	for _, w := range list {
		name := sanitiseName(w.ContentName)
		if name == "" {
			name = "file"
		}
		if w.DriveDataRef != nil || w.Source == "DRIVE_FILE" {
			notes = append(notes, "[Drive file not downloaded: "+name+"]")
			continue
		}
		if w.AttachmentDataRef == nil || w.AttachmentDataRef.ResourceName == "" {
			notes = append(notes, "[file unavailable: "+name+"]")
			continue
		}
		att, note := a.download(ctx, w.AttachmentDataRef.ResourceName, name, w.ContentType)
		if note != "" {
			notes = append(notes, note)
			continue
		}
		atts = append(atts, att)
	}
	return atts, notes
}

// download fetches one file with media.download, capped at MaxDownload, and
// stores it as a blob.
func (a *Adapter) download(ctx context.Context, resource, name, mimeType string) (event.Attachment, string) {
	unavailable := "[file unavailable: " + name + "]"
	u, ok := a.mediaURL(resource)
	if !ok || a.opt.PutBlob == nil {
		return event.Attachment{}, unavailable
	}
	resp, _, cancel, err := a.send(ctx, request{op: "media.download", method: http.MethodGet, url: func(string) string { return u }})
	if err != nil {
		a.log.Warn("googlechat download failed", "error", describe(err))
		return event.Attachment{}, unavailable
	}
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		a.log.Warn("googlechat download failed", "status", resp.StatusCode)
		return event.Attachment{}, unavailable
	}
	if resp.ContentLength > a.opt.MaxDownload {
		return event.Attachment{}, "[file too large: " + name + "]"
	}
	// One byte past the cap tells an oversized file from an exact fit.
	data, err := io.ReadAll(io.LimitReader(resp.Body, a.opt.MaxDownload+1))
	if err != nil {
		a.log.Warn("googlechat download failed", "error", a.scrub(err.Error()))
		return event.Attachment{}, unavailable
	}
	if int64(len(data)) > a.opt.MaxDownload {
		return event.Attachment{}, "[file too large: " + name + "]"
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	blobID, size, err := a.opt.PutBlob(ctx, bytes.NewReader(data), mimeType)
	if err != nil {
		a.log.Warn("googlechat blob store failed", "error", err.Error())
		return event.Attachment{}, unavailable
	}
	if size == 0 {
		size = int64(len(data))
	}
	return event.Attachment{
		ID:     event.NewID(),
		BlobID: blobID,
		MIME:   mimeType,
		Name:   name,
		Size:   size,
		SHA256: blobID, // blob ids are the content address: hex sha256
		Kind:   event.KindFromMIME(mimeType),
	}, ""
}

// mediaURL builds the media.download URL. The resource name is opaque and may
// hold slashes, but it comes from an event, so a segment that would climb out
// of /v1/media/ is refused.
func (a *Adapter) mediaURL(resource string) (string, bool) {
	segs := strings.Split(resource, "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", false
		}
		segs[i] = url.PathEscape(s)
	}
	return a.chat + "/v1/media/" + strings.Join(segs, "/") + "?alt=media", true
}

// sanitiseName reduces a sender-supplied file name to a safe base name.
func sanitiseName(s string) string {
	s = strings.ReplaceAll(s, "\\", "/")
	s = path.Base(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || unicode.IsSpace(r) && r != ' ' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(strings.Trim(s, "."))
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
