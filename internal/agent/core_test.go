package agent

import (
	"testing"

	"holaclaw/internal/tokens"
)

func TestCorePromptUnderCap(t *testing.T) {
	if n := tokens.Estimate(CorePrompt()); n > MaxCoreTokens || n == 0 {
		t.Fatalf("core prompt is %d tokens, cap %d", n, MaxCoreTokens)
	} else {
		t.Logf("core prompt: %d tokens", n)
	}
}
