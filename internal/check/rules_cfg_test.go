package check

import (
	"context"
	"testing"

	"holaclaw/internal/config"
)

func TestCFGPassthrough(t *testing.T) {
	in := &Input{LoadProblems: config.Problems{
		{Code: "CFG001", Severity: config.SeverityError, File: "config.toml", Key: "foo", Message: "unknown key", Hint: "did you mean bar?"},
	}}
	fs := cfgRule().Run(context.Background(), in)
	if len(fs) != 1 {
		t.Fatalf("findings = %d, want 1", len(fs))
	}
	f := fs[0]
	if f.ID != "CFG001" || f.Severity != SeverityError || f.File != "config.toml" || f.Key != "foo" ||
		f.Message != "unknown key" || f.Fix != "did you mean bar?" {
		t.Errorf("finding = %+v", f)
	}
}

func TestCFGPassthroughNone(t *testing.T) {
	fs := cfgRule().Run(context.Background(), &Input{})
	if len(fs) != 0 {
		t.Errorf("findings = %v, want none", fs)
	}
}
