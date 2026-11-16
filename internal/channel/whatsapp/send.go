package whatsapp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"mime"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"holaclaw/internal/channel"
	"holaclaw/internal/channel/textutil"
	"holaclaw/internal/event"
)

// Send delivers one reply: text converted to WhatsApp's markup and split at
// 4000 characters, files through Upload. A single file with short text goes
// as one captioned message. Parts go out about a second apart, one send at a
// time per instance, and each carries the conversation's disappearing timer
// (§5.1, §7).
func (a *Adapter) Send(ctx context.Context, out channel.Outbound) (channel.Receipt, error) {
	var rec channel.Receipt
	conv := out.Target.Conversation
	to, err := types.ParseJID(conv)
	if err != nil || to.User == "" ||
		to.Server != types.DefaultUserServer && to.Server != types.HiddenUserServer && to.Server != types.GroupServer {
		return rec, fmt.Errorf("whatsapp: bad target conversation %q", conv)
	}
	cli := a.currentClient()
	if cli == nil {
		return rec, errors.New("whatsapp: the channel is not running")
	}
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	s := &sender{a: a, cli: cli, to: to, conv: conv, exp: a.expiration(ctx, conv), rec: &rec}

	text := ""
	if strings.TrimSpace(out.Message.Text) != "" {
		text = mdToWA(out.Message.Text)
	}
	atts := out.Message.Attachments
	if len(atts) == 1 && text != "" && textutil.UTF16Len(text) <= maxCaptionChars && captionable(atts[0]) {
		if err := s.attachment(ctx, atts[0], text); err != nil {
			return rec, err
		}
		if len(rec.Degraded) == 0 {
			return rec, nil
		}
		atts = nil // the file did not travel, so the text still has to
	}
	if text != "" {
		for _, part := range splitText(text, maxTextChars) {
			if err := s.send(ctx, s.textMessage(part)); err != nil {
				return rec, err
			}
		}
	}
	for _, att := range atts {
		if err := s.attachment(ctx, att, ""); err != nil {
			return rec, err
		}
	}
	return rec, nil
}

// sender is one reply on its way out.
type sender struct {
	a     *Adapter
	cli   waClient
	to    types.JID
	conv  string
	exp   uint32 // disappearing timer copied from the inbound message
	rec   *channel.Receipt
	parts int
}

// send posts one message. Its id is generated before the first try and
// reused on every retry, so a retry after a lost answer shows once.
func (s *sender) send(ctx context.Context, msg *waE2E.Message) error {
	if s.parts > 0 && !s.a.sleep(ctx, partGap) {
		return ctx.Err()
	}
	s.parts++
	id := s.cli.GenerateMessageID()
	var err error
	for attempt := 1; attempt <= maxSendAttempts; attempt++ {
		if _, err = s.cli.SendMessage(ctx, s.to, msg, whatsmeow.SendRequestExtra{ID: id}); err == nil {
			s.rec.PlatformMsgIDs = append(s.rec.PlatformMsgIDs, s.conv+"/"+id)
			s.a.noteAuthor(s.conv+"/"+id, types.EmptyJID)
			return nil
		}
		if ctx.Err() != nil || attempt == maxSendAttempts || !s.a.sleep(ctx, retryWait*time.Duration(attempt)) {
			break
		}
	}
	return fmt.Errorf("whatsapp: send: %w", err)
}

// context is the context info every outbound message carries, or nil.
func (s *sender) context() *waE2E.ContextInfo {
	if s.exp == 0 {
		return nil
	}
	return &waE2E.ContextInfo{Expiration: ptr(s.exp)}
}

func (s *sender) textMessage(text string) *waE2E.Message {
	if ci := s.context(); ci != nil {
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: ptr(text), ContextInfo: ci}}
	}
	return &waE2E.Message{Conversation: ptr(text)}
}

// attachment uploads and sends one file. A file that cannot travel becomes a
// degraded reason, not an error for the whole reply.
func (s *sender) attachment(ctx context.Context, att event.Attachment, caption string) error {
	a := s.a
	name := att.Name
	if name == "" {
		name = string(att.Kind)
	}
	degrade := func(reason string) error {
		s.rec.Degraded = append(s.rec.Degraded, name+": "+reason)
		return nil
	}
	if att.Size > maxUploadBytes {
		return degrade(fmt.Sprintf("too large for WhatsApp (%d MB)", att.Size>>20))
	}
	if a.blobs == nil {
		return degrade("no blob store")
	}
	data, err := a.blobs.OpenAttachment(ctx, att)
	if err != nil {
		return degrade(err.Error())
	}
	if int64(len(data)) > maxUploadBytes {
		return degrade(fmt.Sprintf("too large for WhatsApp (%d MB)", int64(len(data))>>20))
	}

	mt := whatsmeow.MediaDocument
	switch {
	case att.Kind == event.KindImage:
		mt = whatsmeow.MediaImage
	case att.Kind == event.KindVideo:
		mt = whatsmeow.MediaVideo
	case att.Kind == event.KindAudio && isVoice(att.MIME):
		mt = whatsmeow.MediaAudio
	}
	up, err := s.cli.Upload(ctx, data, mt)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return degrade("upload failed: " + err.Error())
	}
	var capt *string
	if caption != "" {
		capt = ptr(caption)
	}
	ci := s.context()
	msg := &waE2E.Message{}
	switch mt {
	case whatsmeow.MediaImage:
		msg.ImageMessage = &waE2E.ImageMessage{
			URL: ptr(up.URL), DirectPath: ptr(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: ptr(up.FileLength),
			Mimetype: ptr(att.MIME), Caption: capt, ContextInfo: ci,
		}
	case whatsmeow.MediaVideo:
		msg.VideoMessage = &waE2E.VideoMessage{
			URL: ptr(up.URL), DirectPath: ptr(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: ptr(up.FileLength),
			Mimetype: ptr(att.MIME), Caption: capt, ContextInfo: ci,
		}
	case whatsmeow.MediaAudio:
		// Already Ogg/Opus, so it plays as a voice note; holaclaw has no ffmpeg.
		msg.AudioMessage = &waE2E.AudioMessage{
			URL: ptr(up.URL), DirectPath: ptr(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: ptr(up.FileLength),
			Mimetype: ptr("audio/ogg; codecs=opus"), PTT: ptr(true), ContextInfo: ci,
		}
	default:
		msg.DocumentMessage = &waE2E.DocumentMessage{
			URL: ptr(up.URL), DirectPath: ptr(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: ptr(up.FileLength),
			Mimetype: ptr(cmp.Or(att.MIME, "application/octet-stream")), FileName: ptr(name), Title: ptr(name),
			Caption: capt, ContextInfo: ci,
		}
	}
	if err := s.send(ctx, msg); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return degrade(err.Error())
	}
	return nil
}

// captionable reports whether an attachment's message can carry a caption:
// a voice note cannot.
func captionable(att event.Attachment) bool {
	return att.Kind != event.KindAudio || !isVoice(att.MIME)
}

// isVoice reports whether a mime type is Opus in Ogg, which WhatsApp plays as
// a voice note.
func isVoice(m string) bool {
	mt, params, err := mime.ParseMediaType(m)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(m))
	}
	if mt != "audio/ogg" && mt != "audio/opus" {
		return false
	}
	return mt == "audio/opus" || strings.Contains(strings.ToLower(params["codecs"]), "opus")
}

func ptr[T any](v T) *T { return &v }

// sleepCtx waits d or until ctx ends, reporting whether the wait completed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
