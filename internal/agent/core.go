package agent

import (
	_ "embed"
	"strings"
)

//go:embed core_prompt.md
var corePrompt string

// MaxCoreTokens is the hard cap of the built-in core fragment, enforced by a
// test and at load time (docs/context.md).
const MaxCoreTokens = 300

// CorePrompt returns the built-in core prompt fragment.
func CorePrompt() string { return strings.TrimSpace(corePrompt) }
