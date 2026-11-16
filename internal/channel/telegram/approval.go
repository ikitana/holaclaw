package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"holaclaw/internal/event"
)

// callbackData is limited to 64 bytes by the Bot API, so the approval id
// travels as an opaque short id resolved from memory (docs/channels).
const (
	callbackPrefix  = "a:"
	maxCallbackData = 64
	shortIDBytes    = 6
)

// PromptApproval renders an approval request with one button per choice.
func (a *Adapter) PromptApproval(ctx context.Context, target event.Origin, approvalID event.ID, text string, choices []string) error {
	if target.Conversation == "" {
		return &Error{Method: "PromptApproval", Kind: KindBadRequest, Description: "empty target conversation"}
	}
	short := a.registerApproval(approvalID)
	row := make([]inlineButton, 0, len(choices))
	for _, c := range choices {
		row = append(row, inlineButton{Text: c, CallbackData: callbackDataFor(short, c)})
	}
	kb := &inlineKeyboard{InlineKeyboard: [][]inlineButton{row}}

	chunks := splitHTML(mdToHTML(text), maxTextChars)
	if len(chunks) == 0 {
		chunks = []string{"Approval required."}
	}
	d := &dest{chat: target.Conversation, thread: target.Thread}
	for i, chunk := range chunks {
		var markup *inlineKeyboard
		if i == len(chunks)-1 {
			markup = kb
		}
		if _, err := a.sendMessage(ctx, d, chunk, markup); err != nil {
			return err
		}
	}
	return nil
}

// callbackDataFor builds "a:<shortid>:<choice>", truncated to 64 bytes.
func callbackDataFor(short, choice string) string {
	d := callbackPrefix + short + ":" + choice
	if len(d) > maxCallbackData {
		d = d[:maxCallbackData]
	}
	return d
}

// registerApproval maps a fresh short id onto an approval id for one hour.
func (a *Adapter) registerApproval(id event.ID) string {
	b := make([]byte, shortIDBytes)
	_, _ = rand.Read(b)
	short := hex.EncodeToString(b)
	now := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range a.pending {
		if now.After(v.expires) {
			delete(a.pending, k)
		}
	}
	if len(a.pending) >= maxApprovals {
		// Bounded: drop the soonest to expire rather than grow without limit.
		var oldest string
		for k, v := range a.pending {
			if oldest == "" || v.expires.Before(a.pending[oldest].expires) {
				oldest = k
			}
		}
		delete(a.pending, oldest)
	}
	a.pending[short] = pendingApproval{id: id, expires: now.Add(approvalTTL)}
	return short
}

func (a *Adapter) lookupApproval(short string) (event.ID, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pending[short]
	if !ok || time.Now().After(p.expires) {
		delete(a.pending, short)
		return "", false
	}
	return p.id, true
}

// handleCallback resolves a button press, answers the query so the client stops
// spinning, and rewrites the message without its keyboard.
func (a *Adapter) handleCallback(ctx context.Context, q *wireCallbackQuery) {
	parts := strings.SplitN(q.Data, ":", 3)
	if len(parts) != 3 || parts[0] != strings.TrimSuffix(callbackPrefix, ":") {
		a.answerCallback(ctx, q.ID, "")
		return
	}
	id, ok := a.lookupApproval(parts[1])
	if !ok {
		a.answerCallback(ctx, q.ID, "This request has expired.")
		return
	}
	choice := parts[2]
	if a.onApproval != nil {
		a.onApproval(ctx, id, choice, sender(q.From).Aliases)
	}
	a.answerCallback(ctx, q.ID, choice)
	a.finishPrompt(ctx, q, choice)
}

func (a *Adapter) answerCallback(ctx context.Context, id, text string) {
	body := map[string]any{"callback_query_id": id}
	if text != "" {
		body["text"] = text
	}
	if err := a.call(ctx, "answerCallbackQuery", body, nil); err != nil {
		a.log.Warn("telegram answerCallbackQuery failed", "channel", a.name, "error", describe(err))
	}
}

// finishPrompt removes the keyboard and records the decision in the message.
func (a *Adapter) finishPrompt(ctx context.Context, q *wireCallbackQuery, choice string) {
	if q.Message == nil {
		return
	}
	chat := strconv.FormatInt(q.Message.Chat.ID, 10)
	decision := "<i>" + escapeHTML(choice) + "</i>"
	body := map[string]any{
		"chat_id":    chat,
		"message_id": q.Message.MessageID,
		"parse_mode": "HTML",
	}
	method := "editMessageText"
	switch {
	case q.Message.Text != "":
		body["text"] = escapeHTML(q.Message.Text) + "\n\n" + decision
	case q.Message.Caption != "":
		method = "editMessageCaption"
		body["caption"] = escapeHTML(q.Message.Caption) + "\n\n" + decision
	default:
		method = "editMessageReplyMarkup"
		delete(body, "parse_mode")
	}
	// No reply_markup: the keyboard goes away.
	if err := a.call(ctx, method, body, nil); err != nil {
		a.log.Warn("telegram edit after approval failed", "channel", a.name, "error", describe(err))
	}
}
