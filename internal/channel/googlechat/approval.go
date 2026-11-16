package googlechat

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/oklog/ulid/v2"

	"holaclaw/internal/event"
)

// choiceShape is what an approval choice may look like ("once", "deny", ...).
// The button parameters come back in an event, so they are checked here and
// core decides whether the choice is valid.
var choiceShape = regexp.MustCompile(`^[a-z_]{1,32}$`)

// PromptApproval posts the request as text with a card holding one button per
// choice. A press arrives as CARD_CLICKED through Pub/Sub.
func (a *Adapter) PromptApproval(ctx context.Context, target event.Origin, approvalID event.ID, text string, choices []string) error {
	if err := checkTarget(target); err != nil {
		return err
	}
	chunks := chunkText(markdownToChat(text), maxTextChars)
	if len(chunks) == 0 {
		chunks = []string{"Approval required."}
	}
	buttons := make([]any, 0, len(choices))
	for _, c := range choices {
		buttons = append(buttons, map[string]any{
			"text": c,
			"onClick": map[string]any{"action": map[string]any{
				"function": approvalFunction,
				"parameters": []wireParam{
					{Key: "approval_id", Value: string(approvalID)},
					{Key: "choice", Value: c},
				},
			}},
		})
	}
	card := map[string]any{
		"cardId": "approval-" + string(approvalID),
		"card": map[string]any{"sections": []any{
			map[string]any{"widgets": []any{map[string]any{"buttonList": map[string]any{"buttons": buttons}}}},
		}},
	}
	for i, chunk := range chunks {
		msg := chatMessage{Text: chunk}
		if i == len(chunks)-1 {
			msg.CardsV2 = []any{card}
		}
		if _, err := a.createMessage(ctx, target, msg); err != nil {
			return err
		}
	}
	return nil
}

// handleCardClick resolves an approval button press. The presser is the
// event's authenticated user; the button parameters only name the approval
// and the choice, and are checked for shape.
//
// When OnApproval accepts the press, the card is rewritten without its
// buttons to record the outcome. A refused press (not an owner or admin, or
// an approval already settled) leaves the card as it is, so an admin can
// still answer it.
func (a *Adapter) handleCardClick(ctx context.Context, ev *wireEvent) {
	fn, params := clickParams(ev)
	if fn != approvalFunction {
		a.log.Info("googlechat card click ignored", "reason", "unknown function")
		return
	}
	id, choice := params["approval_id"], params["choice"]
	if _, err := ulid.ParseStrict(id); err != nil || !choiceShape.MatchString(choice) {
		a.log.Warn("googlechat card click ignored", "reason", "malformed parameters")
		return
	}
	if ev.User == nil || ev.User.Name == "" || ev.User.Type == "BOT" {
		a.log.Warn("googlechat card click ignored", "reason", "no human presser")
		return
	}
	// A redelivered click is the same press.
	key := "click:" + id + "|" + ev.User.Name + "|" + choice
	if a.seen.has(key) {
		return
	}
	a.seen.add(key)
	if a.opt.OnApproval == nil || !a.opt.OnApproval(ctx, event.ID(id), choice, aliases(ev.User)) {
		a.log.Debug("googlechat approval press not accepted; card left as is")
		return
	}
	a.settle(ctx, ev, choice)
}

// clickParams reads the pressed function and its parameters, from action
// (Chat apps) or common (the same data in the add-on layout).
func clickParams(ev *wireEvent) (string, map[string]string) {
	params := map[string]string{}
	fn := ""
	if ev.Common != nil {
		fn = ev.Common.InvokedFunction
		for k, v := range ev.Common.Parameters {
			params[k] = v
		}
	}
	if ev.Action != nil {
		if ev.Action.ActionMethodName != "" {
			fn = ev.Action.ActionMethodName
		}
		for _, p := range ev.Action.Parameters {
			params[p.Key] = p.Value
		}
	}
	return fn, params
}

// settle removes the buttons and appends the accepted outcome to the message
// text.
func (a *Adapter) settle(ctx context.Context, ev *wireEvent, choice string) {
	if ev.Message == nil || !messageShape.MatchString(ev.Message.Name) {
		return
	}
	who := strings.TrimSpace(ev.User.DisplayName)
	if who == "" {
		who = ev.User.Name
	}
	who = defuse(strings.NewReplacer("*", "", "_", "", "~", "", "`", "").Replace(who))
	text := strings.TrimSpace(ev.Message.Text)
	if text != "" {
		text += "\n\n"
	}
	outcome := "Approved (" + choice + ") by "
	if choice == "deny" {
		outcome = "Denied by "
	}
	text += "_" + outcome + who + "_"
	u := a.chat + "/v1/" + ev.Message.Name + "?" + url.Values{"updateMask": {"text,cardsV2"}}.Encode()
	// cardsV2 must be sent empty to clear the card, so it is not omitempty here.
	body := map[string]any{"text": text, "cardsV2": []any{}}
	if err := a.call(ctx, request{op: "messages.patch", method: http.MethodPatch,
		url: func(string) string { return u }, body: body}, nil); err != nil {
		a.log.Warn("googlechat card update failed", "error", describe(err))
	}
}
