package googlechat

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/oklog/ulid/v2"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// Shapes of the Chat resource names holaclaw puts in a URL path. They come from
// stored origins and events, so they are checked rather than trusted.
var (
	spaceShape   = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+$`)
	threadShape  = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+/threads/[A-Za-z0-9_.-]+$`)
	messageShape = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+/messages/[A-Za-z0-9_.-]+$`)
)

// chatMessage is the body of spaces.messages.create and .patch.
type chatMessage struct {
	Text    string        `json:"text,omitempty"`
	Thread  *wireThreadID `json:"thread,omitempty"`
	CardsV2 []any         `json:"cardsV2,omitempty"`
	Quoted  *quotedMeta   `json:"quotedMessageMetadata,omitempty"`
}

// quotedMeta names the message a reply quotes. Chat refuses a quote whose
// lastUpdateTime is not the quoted message's current one.
type quotedMeta struct {
	Name           string `json:"name"`
	LastUpdateTime string `json:"lastUpdateTime"`
}

// Send delivers one reply as text in 4000-character messages. A Chat app
// cannot upload files, so every attachment is degraded with a reason. When
// Target.PlatformMsgID names a message in the space, the first message quotes
// it; a quote Chat will not take leaves a plain message.
func (a *Adapter) Send(ctx context.Context, out channel.Outbound) (channel.Receipt, error) {
	var rec channel.Receipt
	if err := checkTarget(out.Target); err != nil {
		return rec, err
	}
	quote := a.quote(ctx, out.Target)
	for _, c := range chunkText(markdownToChat(out.Message.Text), maxTextChars) {
		msg := chatMessage{Text: c, Quoted: quote}
		name, err := a.createMessage(ctx, out.Target, msg)
		var e *Error
		if err != nil && quote != nil && errors.As(err, &e) && (e.Kind == KindBadRequest || e.Kind == KindNotFound) {
			msg.Quoted = nil
			name, err = a.createMessage(ctx, out.Target, msg)
		}
		if err != nil {
			return rec, err
		}
		quote = nil
		rec.PlatformMsgIDs = append(rec.PlatformMsgIDs, name)
	}
	for _, att := range out.Message.Attachments {
		name := att.Name
		if name == "" {
			name = string(att.ID)
		}
		rec.Degraded = append(rec.Degraded, name+": a Google Chat app cannot send files")
	}
	return rec, nil
}

// quote reads the message a reply answers, for its quote metadata. A message
// that is gone or unreadable is answered plainly (nil).
func (a *Adapter) quote(ctx context.Context, t event.Origin) *quotedMeta {
	name := t.PlatformMsgID
	if !messageShape.MatchString(name) || !strings.HasPrefix(name, t.Conversation+"/messages/") {
		return nil
	}
	var got struct {
		CreateTime     string `json:"createTime"`
		LastUpdateTime string `json:"lastUpdateTime"`
	}
	err := a.call(ctx, request{op: "messages.get", method: http.MethodGet,
		url: func(string) string { return a.chat + "/v1/" + name }}, &got)
	if err != nil {
		a.log.Info("googlechat reply sent without a quote", "error", describe(err))
		return nil
	}
	at := got.LastUpdateTime
	if at == "" {
		at = got.CreateTime
	}
	if at == "" {
		return nil
	}
	return &quotedMeta{Name: name, LastUpdateTime: at}
}

func checkTarget(t event.Origin) error {
	if !spaceShape.MatchString(t.Conversation) {
		return &Error{Op: "messages.create", Kind: KindBadRequest, Message: "target is not a Chat space"}
	}
	if t.Thread != "" && !threadShape.MatchString(t.Thread) {
		return &Error{Op: "messages.create", Kind: KindBadRequest, Message: "target thread is not a Chat thread"}
	}
	return nil
}

// createMessage posts one message with a client-assigned id, reused on every
// retry: a retry after a lost response finds the message already there (409)
// instead of posting it twice. 429 and transient failures are retried at most
// maxSendAttempts times.
func (a *Adapter) createMessage(ctx context.Context, target event.Origin, msg chatMessage) (string, error) {
	id := "client-" + strings.ToLower(ulid.Make().String())
	q := url.Values{"messageId": {id}}
	if target.Thread != "" {
		msg.Thread = &wireThreadID{Name: target.Thread}
		q.Set("messageReplyOption", "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD")
	}
	u := a.chat + "/v1/" + target.Conversation + "/messages?" + q.Encode()
	for attempt := 1; ; attempt++ {
		var got struct {
			Name string `json:"name"`
		}
		err := a.call(ctx, request{op: "messages.create", method: http.MethodPost,
			url: func(string) string { return u }, body: msg}, &got)
		if err == nil {
			if got.Name == "" {
				got.Name = target.Conversation + "/messages/" + id
			}
			return got.Name, nil
		}
		var e *Error
		if !errors.As(err, &e) {
			return "", err
		}
		if e.Kind == KindConflict && attempt > 1 {
			// The earlier attempt landed; its response was lost.
			return target.Conversation + "/messages/" + id, nil
		}
		retryable := e.Kind == KindRateLimited || e.Kind == KindServer || e.Kind == KindNetwork
		if !retryable || attempt >= maxSendAttempts || ctx.Err() != nil {
			return "", err
		}
		d := e.RetryAfter
		if d <= 0 {
			d = backoffBase << (attempt - 1)
		}
		d = min(d, backoffMax)
		if !sleepCtx(ctx, d) {
			return "", err
		}
	}
}
