package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"holaclaw/internal/event"
	"holaclaw/internal/tokens"
)

// Stage 1 constants (docs/context.md: "mechanical and at ingest").
const (
	// MaxToolResultTokens is the cap on one tool result in the transcript.
	// What is cut is spilled to a blob and replaced by a pointer.
	MaxToolResultTokens = 2000
	// ImageKeepMessages is how many of the newest user messages keep their
	// image attachments. Older images are dropped; their stub line stays.
	ImageKeepMessages = 2
)

// MechanicalOpts tunes stage 1. The zero value is the compaction default.
type MechanicalOpts struct {
	// Protect leaves the newest n messages untouched (the min-tail portion).
	Protect int
	// MaxToolResult overrides MaxToolResultTokens.
	MaxToolResult int
	// KeepImages overrides ImageKeepMessages.
	KeepImages int
	// Spill stores the full text of an over-long tool result and returns its
	// blob id. Nil truncates without a blob pointer.
	Spill func(ctx context.Context, text string) (string, error)
}

// Mechanical is stage 1 of compaction: the cheap, model-free pass.
//
//   - every tool result is capped at MaxToolResultTokens; the full text is
//     spilled to a blob and replaced by head + a pointer line;
//   - a tool result identical to an earlier one (same tool, same text) is
//     replaced by a pointer to that earlier call;
//   - image attachments are dropped from all but the newest ImageKeepMessages
//     user messages, leaving the one-line stub in the text.
//
// It is pure with respect to the store: it returns new messages and never
// touches the rows it was given. Stored messages are immutable, so at
// compaction time this output is only what gets summarised and what the
// summary describes — old rows keep their full text, and the reclaim is
// measured on the rows that survive the cut. The same function runs at ingest,
// where its output is what gets stored in the first place.
func Mechanical(ctx context.Context, msgs []event.Message, opt MechanicalOpts) []event.Message {
	maxTool := opt.MaxToolResult
	if maxTool <= 0 {
		maxTool = MaxToolResultTokens
	}
	keepImages := opt.KeepImages
	if keepImages <= 0 {
		keepImages = ImageKeepMessages
	}
	protect := len(msgs) - opt.Protect
	if opt.Protect <= 0 {
		protect = len(msgs)
	}

	// The newest user messages that keep their images.
	withImages := map[int]bool{}
	for i, n := len(msgs)-1, 0; i >= 0 && n < keepImages; i-- {
		if msgs[i].Role == event.RoleUser {
			withImages[i] = true
			n++
		}
	}

	seen := map[string]event.Message{} // tool name + text hash -> first message
	out := make([]event.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if i >= protect {
			break
		}
		switch out[i].Role {
		case event.RoleTool:
			key := out[i].ToolName + "\x00" + hashText(out[i].Text)
			if first, dup := seen[key]; dup {
				out[i].Text = fmt.Sprintf("[same result as earlier call %s]", first.ToolCallID)
				continue
			}
			seen[key] = out[i]
			out[i].Text = capResult(ctx, out[i].Text, maxTool, opt.Spill)
		case event.RoleUser:
			if !withImages[i] {
				out[i] = dropImages(out[i])
			}
		}
	}
	return out
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// capResult cuts an over-long tool result to max tokens and points at the full
// text: a blob when one could be written, a plain notice otherwise.
func capResult(ctx context.Context, text string, max int, spill func(context.Context, string) (string, error)) string {
	n := tokens.Estimate(text)
	if n <= max {
		return text
	}
	var pointer string
	if spill != nil {
		if id, err := spill(ctx, text); err == nil && id != "" {
			pointer = fmt.Sprintf("[… full result: blob %s, %d tokens]", id, n)
		}
	}
	if pointer == "" {
		pointer = fmt.Sprintf("[… full result truncated, %d tokens]", n)
	}
	head := max - tokens.Estimate(pointer) - 1 // the newline before the pointer
	if head < 0 {
		head = 0
	}
	return strings.TrimRight(headTokens(text, head), " \t\n") + "\n" + pointer
}

// headTokens returns the longest prefix of s costing at most max tokens.
func headTokens(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	lo, hi := 0, len(r)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if tokens.Estimate(string(r[:mid])) <= max {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(r[:lo])
}

// dropImages removes image attachments and leaves their stub in the text, so
// the model still knows an image was there and can ask for it by id.
func dropImages(m event.Message) event.Message {
	var kept []event.Attachment
	var stubs []string
	for _, a := range m.Attachments {
		if a.Kind != event.KindImage {
			kept = append(kept, a)
			continue
		}
		stubs = append(stubs, attachmentStub(a))
	}
	if len(stubs) == 0 {
		return m
	}
	m.Attachments = kept
	m.Text = strings.TrimRight(m.Text+"\n"+strings.Join(stubs, "\n"), "\n")
	if strings.HasPrefix(m.Text, "\n") {
		m.Text = strings.TrimLeft(m.Text, "\n")
	}
	return m
}
