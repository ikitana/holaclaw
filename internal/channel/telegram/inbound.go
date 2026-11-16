package telegram

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"holaclaw/internal/channel/textutil"
	"holaclaw/internal/event"
)

// envelope maps one message — or a whole media group — onto an Envelope. It
// reports false when there is nothing to route (a message from the bot itself).
func (a *Adapter) envelope(ctx context.Context, msgs []*wireMessage) (event.Envelope, bool) {
	head := msgs[0]
	botID, botUser := a.bot()
	if head.From != nil && head.From.ID == botID {
		return event.Envelope{}, false
	}

	// In an album the caption rides on whichever message carried it.
	text, ents := "", []wireEntity(nil)
	for _, m := range msgs {
		if t, e := messageText(m); t != "" {
			text, ents = t, e
			break
		}
	}

	kind := event.ConversationRoom
	if head.Chat.Type == "private" {
		kind = event.ConversationDM
	}
	thread := ""
	if head.IsTopicMessage && head.MessageThreadID != 0 {
		thread = strconv.FormatInt(head.MessageThreadID, 10)
	}
	chat := strconv.FormatInt(head.Chat.ID, 10)

	mentioned := kind == event.ConversationDM
	if !mentioned {
		mentioned, text = groupMention(head, text, ents, botID, botUser)
	} else {
		_, text = groupMention(head, text, ents, botID, botUser)
	}

	var atts []event.Attachment
	var notes []string
	for _, m := range msgs {
		att, note, ok := a.attachment(ctx, m)
		switch {
		case ok:
			atts = append(atts, att)
		case note != "":
			notes = append(notes, note)
		}
	}
	if len(notes) > 0 {
		text = strings.TrimSpace(text + "\n" + strings.Join(notes, "\n"))
	}

	env := event.Envelope{
		Schema: event.SchemaVersion,
		Origin: event.Origin{
			Channel:          a.name,
			ChannelType:      a.Type(),
			Conversation:     chat,
			ConversationKind: kind,
			Thread:           thread,
			PlatformMsgID:    chat + ":" + strconv.FormatInt(head.MessageID, 10),
		},
		Sender:    sender(head.From),
		Mentioned: mentioned,
		Message: event.Message{
			Schema:      event.SchemaVersion,
			ID:          event.NewID(),
			Role:        event.RoleUser,
			Text:        text,
			Attachments: atts,
			CreatedAt:   time.Unix(head.Date, 0).UTC(),
		},
		Received: time.Now().UTC(),
	}
	// reply_to stays empty: a Telegram message id is not a holaclaw message id.
	return env, true
}

// messageText returns the text or caption of m with its entities.
func messageText(m *wireMessage) (string, []wireEntity) {
	if m.Text != "" {
		return m.Text, m.Entities
	}
	return m.Caption, m.CaptionEntities
}

// sender builds the platform identity. Aliases are every id the platform knows
// this sender by (ARCHITECTURE.md §6).
func sender(u *wireUser) event.Sender {
	if u == nil {
		return event.Sender{}
	}
	// The numeric id only: a username can be dropped and claimed by someone
	// else, and core links every alias of an envelope (docs/identity.md §2).
	s := event.Sender{Aliases: []string{"tg:" + strconv.FormatInt(u.ID, 10)}}
	s.DisplayName = strings.TrimSpace(u.FirstName + " " + u.LastName)
	if s.DisplayName == "" {
		s.DisplayName = u.Username
	}
	return s
}

// groupMention reports whether the bot was addressed in a group and returns the
// text with a leading @botusername removed (docs/sessions.md).
func groupMention(m *wireMessage, text string, ents []wireEntity, botID int64, botUser string) (bool, string) {
	// A reply to one of the bot's own messages addresses the bot.
	mentioned := m.ReplyToMessage != nil && m.ReplyToMessage.From != nil && m.ReplyToMessage.From.ID == botID
	at := "@" + botUser
	strip := -1 // UTF-16 end offset of a leading @botusername
	for _, e := range ents {
		switch e.Type {
		case "mention":
			if botUser == "" {
				continue
			}
			if strings.EqualFold(textutil.UTF16Sub(text, e.Offset, e.Length), at) {
				mentioned = true
				if e.Offset == 0 {
					strip = e.Length
				}
			}
		case "text_mention":
			if e.User != nil && e.User.ID == botID {
				mentioned = true
			}
		case "bot_command":
			// "/cmd@botusername" addresses this bot; the command itself stays.
			cmd := textutil.UTF16Sub(text, e.Offset, e.Length)
			if i := strings.IndexByte(cmd, '@'); i >= 0 && botUser != "" &&
				strings.EqualFold(cmd[i:], at) {
				mentioned = true
			}
		}
	}
	if strip >= 0 {
		text = strings.TrimLeft(textutil.UTF16Sub(text, strip, textutil.UTF16Len(text)-strip), " \t\n")
	}
	return mentioned, text
}

// mediaRef is the media field of a message, normalised.
type mediaRef struct {
	fileID string
	name   string
	mime   string
	size   int64
}

// mediaOf picks the single media field of a message. Stickers are skipped: they
// carry no content holaclaw can use.
func mediaOf(m *wireMessage) (mediaRef, bool) {
	switch {
	case len(m.Photo) > 0:
		best := m.Photo[0]
		for _, p := range m.Photo[1:] {
			if p.FileSize > best.FileSize || (p.FileSize == best.FileSize && p.Width > best.Width) {
				best = p
			}
		}
		return mediaRef{fileID: best.FileID, name: "photo.jpg", mime: "image/jpeg", size: best.FileSize}, true
	case m.Document != nil:
		return fileRef(m.Document, "document", ""), true
	case m.Voice != nil:
		// A voice note is always opus in ogg (docs/sessions.md).
		r := fileRef(m.Voice, "voice.ogg", "audio/ogg; codecs=opus")
		r.mime = "audio/ogg; codecs=opus"
		return r, true
	case m.Audio != nil:
		return fileRef(m.Audio, "audio", "audio/mpeg"), true
	case m.VideoNote != nil:
		return fileRef(m.VideoNote, "video_note.mp4", "video/mp4"), true
	case m.Video != nil:
		return fileRef(m.Video, "video.mp4", "video/mp4"), true
	case m.Animation != nil:
		return fileRef(m.Animation, "animation.mp4", "video/mp4"), true
	case m.Sticker != nil:
		return mediaRef{}, false
	}
	return mediaRef{}, false
}

func fileRef(f *wireFile, fallbackName, fallbackMIME string) mediaRef {
	r := mediaRef{fileID: f.FileID, name: f.FileName, mime: f.MIMEType, size: f.FileSize}
	if r.name == "" {
		r.name = fallbackName
	}
	if r.mime == "" {
		r.mime = fallbackMIME
	}
	return r
}

// attachment downloads one message's media and stores it as a blob. When the
// file cannot be delivered it returns a note for the text instead
// (docs/sessions.md).
func (a *Adapter) attachment(ctx context.Context, m *wireMessage) (event.Attachment, string, bool) {
	ref, ok := mediaOf(m)
	if !ok {
		return event.Attachment{}, "", false
	}
	name := sanitiseName(ref.name)
	if ref.size > a.maxDownload {
		return event.Attachment{}, "[file too large: " + name + "]", false
	}
	var f wireFile
	if err := a.call(ctx, "getFile", map[string]any{"file_id": ref.fileID}, &f); err != nil {
		a.log.Warn("telegram getFile failed", "channel", a.name, "error", describe(err))
		return event.Attachment{}, "[file unavailable: " + name + "]", false
	}
	if f.FilePath == "" {
		return event.Attachment{}, "[file unavailable: " + name + "]", false
	}
	if f.FileSize > a.maxDownload {
		return event.Attachment{}, "[file too large: " + name + "]", false
	}
	data, tooBig, err := a.download(ctx, f.FilePath, a.maxDownload)
	if err != nil {
		a.log.Warn("telegram download failed", "channel", a.name, "error", describe(err))
		return event.Attachment{}, "[file unavailable: " + name + "]", false
	}
	if tooBig {
		return event.Attachment{}, "[file too large: " + name + "]", false
	}
	mimeType := ref.mime
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	if a.putBlob == nil {
		return event.Attachment{}, "[file unavailable: " + name + "]", false
	}
	blobID, size, err := a.putBlob(ctx, bytes.NewReader(data), mimeType)
	if err != nil {
		a.log.Warn("telegram blob store failed", "channel", a.name, "error", err.Error())
		return event.Attachment{}, "[file unavailable: " + name + "]", false
	}
	if size == 0 {
		size = int64(len(data))
	}
	if name == "" {
		name = generatedName(mimeType)
	}
	return event.Attachment{
		ID:     event.NewID(),
		BlobID: blobID,
		MIME:   mimeType,
		Name:   name,
		Size:   size,
		SHA256: blobID, // blob ids are the content address: hex sha256
		Kind:   event.KindFromMIME(mimeType),
	}, "", true
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

// generatedName names an unnamed file after its kind.
func generatedName(mimeType string) string {
	ext := ".bin"
	if i := strings.IndexByte(mimeType, '/'); i >= 0 {
		sub := mimeType[i+1:]
		if j := strings.IndexAny(sub, "; "); j >= 0 {
			sub = sub[:j]
		}
		if sub != "" {
			ext = "." + sub
		}
	}
	return fmt.Sprintf("file-%d%s", time.Now().UnixNano(), ext)
}

// handleChatMember reports the bot being added to a group. Such updates carry
// no message, so they produce no envelope.
func (a *Adapter) handleChatMember(ctx context.Context, u *wireChatMemberUpdated) {
	botID, _ := a.bot()
	if u.NewChatMember.User == nil || u.NewChatMember.User.ID != botID {
		return
	}
	if u.Chat.Type != "group" && u.Chat.Type != "supergroup" {
		return
	}
	joined := u.NewChatMember.Status == "member" || u.NewChatMember.Status == "administrator" ||
		u.NewChatMember.Status == "restricted"
	was := u.OldChatMember.Status
	if !joined || (was != "left" && was != "kicked" && was != "") {
		return
	}
	if a.onJoined != nil {
		a.onJoined(ctx, strconv.FormatInt(u.Chat.ID, 10), sender(u.From).Aliases)
	}
}
