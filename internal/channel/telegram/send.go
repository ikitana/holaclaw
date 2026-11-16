package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strconv"
	"strings"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// Send delivers one reply. Text is converted to Telegram HTML and split; a
// single attachment with short text travels as a caption (docs/channels).
// When Target.PlatformMsgID names a message in the chat, the first message
// that goes out is a native reply to it.
func (a *Adapter) Send(ctx context.Context, out channel.Outbound) (channel.Receipt, error) {
	var rec channel.Receipt
	chat := out.Target.Conversation
	if chat == "" {
		return rec, &Error{Method: "send", Kind: KindBadRequest, Description: "empty target conversation"}
	}
	d := &dest{chat: chat, thread: out.Target.Thread, replyTo: replyToID(out.Target)}
	atts := out.Message.Attachments
	html := ""
	if strings.TrimSpace(out.Message.Text) != "" {
		html = mdToHTML(out.Message.Text)
	}

	// Text + exactly one attachment that fits the caption limit: one message.
	if len(atts) == 1 && html != "" && visibleLen(html) <= maxCaptionChars {
		id, degraded, err := a.sendAttachment(ctx, d, atts[0], html)
		if err != nil {
			return rec, err
		}
		appendReceipt(&rec, chat, id, degraded)
		if degraded != "" {
			// The file did not travel, so the text still has to.
			return a.sendText(ctx, d, html, rec)
		}
		return rec, nil
	}

	if html != "" {
		var err error
		rec, err = a.sendText(ctx, d, html, rec)
		if err != nil {
			return rec, err
		}
	}
	for _, att := range atts {
		id, degraded, err := a.sendAttachment(ctx, d, att, "")
		if err != nil {
			return rec, err
		}
		appendReceipt(&rec, chat, id, degraded)
	}
	return rec, nil
}

// dest is where one Send goes. replyTo is the message the reply answers; it
// is cleared once a message went out, so only the first one is marked.
type dest struct {
	chat, thread string
	replyTo      int64
}

// replyParams is the reply_parameters value for the next message, or "" when
// it is a plain one. A reply to a message that is gone is sent anyway.
func (d *dest) replyParams() string {
	if d.replyTo == 0 {
		return ""
	}
	return `{"message_id":` + strconv.FormatInt(d.replyTo, 10) + `,"allow_sending_without_reply":true}`
}

// replyToID reads the message id out of a "<chat>:<message>" PlatformMsgID
// when it belongs to the target chat; anything else answers plainly.
func replyToID(t event.Origin) int64 {
	chat, msg, ok := strings.Cut(t.PlatformMsgID, ":")
	if !ok || chat != t.Conversation {
		return 0
	}
	id, err := strconv.ParseInt(msg, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

func appendReceipt(rec *channel.Receipt, chat string, id int64, degraded string) {
	if id != 0 {
		rec.PlatformMsgIDs = append(rec.PlatformMsgIDs, chat+":"+strconv.FormatInt(id, 10))
	}
	if degraded != "" {
		rec.Degraded = append(rec.Degraded, degraded)
	}
}

// sendText splits html into messages and sends them in order.
func (a *Adapter) sendText(ctx context.Context, d *dest, html string, rec channel.Receipt) (channel.Receipt, error) {
	for _, chunk := range splitHTML(html, maxTextChars) {
		id, err := a.sendMessage(ctx, d, chunk, nil)
		if err != nil {
			return rec, err
		}
		appendReceipt(&rec, d.chat, id, "")
	}
	return rec, nil
}

// sendMessage posts one chunk. A rejected parse falls back to plain text, so a
// conversion bug degrades formatting instead of losing the reply.
func (a *Adapter) sendMessage(ctx context.Context, d *dest, html string, markup *inlineKeyboard) (int64, error) {
	body := map[string]any{"chat_id": d.chat, "text": html, "parse_mode": "HTML"}
	if d.thread != "" {
		body["message_thread_id"] = d.thread
	}
	if p := d.replyParams(); p != "" {
		body["reply_parameters"] = json.RawMessage(p)
	}
	if markup != nil {
		body["reply_markup"] = markup
	}
	var msg wireMessage
	err := a.retry(ctx, func() error { return a.call(ctx, "sendMessage", body, &msg) })
	var te *Error
	if err != nil && errors.As(err, &te) && replyGone(te) {
		delete(body, "reply_parameters")
		err = a.retry(ctx, func() error { return a.call(ctx, "sendMessage", body, &msg) })
	}
	if err != nil && errors.As(err, &te) && parseErrorText(te) {
		a.log.Warn("telegram rejected HTML, resending as plain text", "channel", a.name)
		delete(body, "parse_mode")
		body["text"] = htmlToPlain(html)
		err = a.retry(ctx, func() error { return a.call(ctx, "sendMessage", body, &msg) })
	}
	if err != nil {
		return 0, err
	}
	d.replyTo = 0
	return msg.MessageID, nil
}

// sendAttachment uploads one attachment with the method its kind calls for.
// It returns a degraded reason instead of an error when the file itself is the
// problem (docs/sessions.md).
func (a *Adapter) sendAttachment(ctx context.Context, d *dest, att event.Attachment, caption string) (int64, string, error) {
	name := att.Name
	if name == "" {
		name = generatedName(att.MIME)
	}
	if att.Size > maxUploadBytes {
		return 0, fmt.Sprintf("%s: too large for Telegram (%d MB)", name, att.Size>>20), nil
	}
	if a.blobs == nil {
		return 0, name + ": no blob store", nil
	}
	data, err := a.blobs.OpenAttachment(ctx, att)
	if err != nil {
		return 0, name + ": " + err.Error(), nil
	}
	if int64(len(data)) > maxUploadBytes {
		return 0, fmt.Sprintf("%s: too large for Telegram (%d MB)", name, int64(len(data))>>20), nil
	}

	method, field := sendMethod(att, int64(len(data)))
	fields := map[string]string{"chat_id": d.chat}
	if d.thread != "" {
		fields["message_thread_id"] = d.thread
	}
	if p := d.replyParams(); p != "" {
		fields["reply_parameters"] = p
	}
	if caption != "" {
		fields["caption"] = caption
		fields["parse_mode"] = "HTML"
	}
	file := multipartFile{Field: field, Name: name, MIME: att.MIME, Data: data}

	var msg wireMessage
	err = a.retry(ctx, func() error { return a.upload(ctx, method, fields, file, &msg) })
	var te *Error
	if err != nil && errors.As(err, &te) && replyGone(te) {
		delete(fields, "reply_parameters")
		err = a.retry(ctx, func() error { return a.upload(ctx, method, fields, file, &msg) })
	}
	if err != nil && errors.As(err, &te) && parseErrorText(te) && caption != "" {
		delete(fields, "parse_mode")
		fields["caption"] = htmlToPlain(caption)
		err = a.retry(ctx, func() error { return a.upload(ctx, method, fields, file, &msg) })
	}
	if err != nil && method == "sendPhoto" {
		// Telegram is picky about photos; anything it refuses goes as a file.
		a.log.Warn("telegram sendPhoto failed, retrying as document", "channel", a.name, "error", describe(err))
		file.Field = "document"
		err = a.retry(ctx, func() error { return a.upload(ctx, "sendDocument", fields, file, &msg) })
	}
	if err != nil {
		if errors.Is(err, ErrForbidden) || errors.Is(err, ErrAuth) || ctx.Err() != nil {
			return 0, "", err
		}
		return 0, name + ": " + describe(err), nil
	}
	d.replyTo = 0
	return msg.MessageID, "", nil
}

// sendMethod picks the Bot API method and form field for an attachment.
func sendMethod(att event.Attachment, size int64) (method, field string) {
	switch att.Kind {
	case event.KindImage:
		if size > maxPhotoBytes {
			return "sendDocument", "document"
		}
		return "sendPhoto", "photo"
	case event.KindAudio:
		if isVoice(att.MIME) {
			return "sendVoice", "voice"
		}
		return "sendAudio", "audio"
	case event.KindVideo:
		return "sendVideo", "video"
	default:
		return "sendDocument", "document"
	}
}

// isVoice reports whether a mime type is the opus-in-ogg voice note Telegram
// has a dedicated method for (docs/sessions.md).
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

// retry honours Telegram's retry_after, at most maxSendAttempts times.
func (a *Adapter) retry(ctx context.Context, fn func() error) error {
	for attempt := 1; ; attempt++ {
		err := fn()
		var te *Error
		if err == nil || attempt >= maxSendAttempts || !errors.As(err, &te) || te.Kind != KindRateLimit {
			return err
		}
		d := te.RetryAfter
		if d <= 0 {
			d = backoffBase
		}
		if !sleepCtx(ctx, d) {
			return err
		}
	}
}
