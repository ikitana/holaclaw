package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"holaclaw/internal/tool"
)

type fakeReacter struct {
	emoji []string
	err   error
}

func (f *fakeReacter) React(_ context.Context, _ tool.Call, emoji string) error {
	if f.err != nil {
		return f.err
	}
	f.emoji = append(f.emoji, emoji)
	return nil
}

func TestReactSchema(t *testing.T) {
	s := (&reactTool{}).Spec()
	if n := tool.SchemaTokens(s); n > tool.MaxSchemaTokens {
		t.Fatalf("schema is %d tokens, cap %d", n, tool.MaxSchemaTokens)
	}
	if err := RegisterReact(tool.NewRegistry(), nil); err == nil {
		t.Fatal("nil reacter accepted")
	}
	if err := RegisterReact(tool.NewRegistry(), &fakeReacter{}); err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestReact(t *testing.T) {
	run := func(re *fakeReacter, args string) tool.Result {
		t.Helper()
		res, err := (&reactTool{re: re}).Run(context.Background(), tool.Call{Args: []byte(args)})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return res
	}

	re := &fakeReacter{}
	res := run(re, `{"emoji":" 👍🏽 "}`)
	if res.IsError || !res.Reacted || len(re.emoji) != 1 || re.emoji[0] != "👍🏽" {
		t.Fatalf("res = %+v, sent %q", res, re.emoji)
	}

	// The keycap form of a digit is an emoji; the digit alone is not.
	keycap := `{"emoji":"1️⃣"}`
	if res := run(re, keycap); res.IsError || re.emoji[1] != "1"+string(rune(0xFE0F))+string(rune(0x20E3)) {
		t.Fatalf("keycap: res = %+v, sent %q", res, re.emoji)
	}

	// Whatever the inbound rule (emoji.Clean) would strip is refused.
	for _, args := range []string{`{}`, `{"emoji":"ok"}`, `{"emoji":"👍 👎"}`, `{"emoji":"` + strings.Repeat("👍", 9) + `"}`, `[1]`,
		`{"emoji":"你好"}`, `{"emoji":"$$$"}`, `{"emoji":"1"}`, `{"emoji":"\u001b"}`, `{"emoji":"é"}`, `{"emoji":"👍` + string(rune(0x200B)) + `"}`} {
		re := &fakeReacter{}
		if res := run(re, args); !res.IsError || res.Reacted || len(re.emoji) != 0 {
			t.Errorf("%s: res = %+v", args, res)
		}
	}

	// The backend's refusal reaches the model as a readable error.
	res = run(&fakeReacter{err: errors.New("this channel takes only 👍 ❤️")}, `{"emoji":"🦄"}`)
	if !res.IsError || res.Reacted || !strings.Contains(res.Text, "takes only 👍 ❤️") {
		t.Fatalf("refused: res = %+v", res)
	}
}
