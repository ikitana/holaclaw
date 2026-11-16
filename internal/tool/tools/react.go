package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"holaclaw/internal/channel/emoji"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// Reacter puts one emoji on the turn's inbound message. The daemon implements
// it: the target comes from the Call's trace and origin, never from the model,
// and an error is a sentence the model can read (an emoji the channel does not
// take, no message to react to).
type Reacter interface {
	React(ctx context.Context, c tool.Call, emoji string) error
}

// RegisterReact adds react.
func RegisterReact(r *tool.Registry, re Reacter) error {
	if re == nil {
		return errors.New("tools: RegisterReact needs a reacter")
	}
	return r.Register(&reactTool{re: re})
}

type reactTool struct{ re Reacter }

func (*reactTool) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        tool.ReactTool,
		Description: "React to the user's message with one emoji. Enough on its own for a short acknowledgement.",
		Parameters: object{
			Properties: map[string]prop{"emoji": {Type: tString}},
			Required:   []string{"emoji"},
		}.raw(),
	}
}

// Class read: a reaction reaches the same person as the reply text, which is
// not gated at all; an operator can still deny it with policy.tools.
func (*reactTool) Risk() tool.Risk { return tool.RiskRead }

func (t *reactTool) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		Emoji string `json:"emoji"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("react: bad arguments: %v", err), nil
	}
	// The rule a person's reaction is held to on the way in: anything it
	// would strip is refused, not silently changed.
	e := strings.TrimSpace(a.Emoji)
	if e == "" || emoji.Clean(e) != e {
		return errf("react: emoji must be one emoji, like 👍"), nil
	}
	if err := t.re.React(ctx, c, e); err != nil {
		return errf("react: %v", err), nil
	}
	return tool.Result{Text: fmt.Sprintf("reacted %s", e), Reacted: true}, nil
}
