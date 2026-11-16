package daemon

import (
	"context"
	"testing"
	"time"

	"holaclaw/internal/app"
	"holaclaw/internal/config"
	"holaclaw/internal/store"
)

// TestPruneInboundWithoutRetention: inbound dedupe keys are pruned even when
// events are kept forever.
func TestPruneInboundWithoutRetention(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for k, at := range map[string]time.Time{
		"old":    now.Add(-inboundKeep - time.Hour),
		"recent": now.Add(-inboundKeep + time.Hour),
	} {
		if _, err := st.ClaimInbound(ctx, "wa", []string{k}, at); err != nil {
			t.Fatal(err)
		}
	}
	d := &Daemon{App: &app.App{Store: st, Config: &config.Config{}}, log: testLogger(t)}
	d.prune(ctx, now)

	if fresh, _ := st.ClaimInbound(ctx, "wa", []string{"old"}, now); !fresh {
		t.Fatal("the old key was not pruned")
	}
	if fresh, _ := st.ClaimInbound(ctx, "wa", []string{"recent"}, now); fresh {
		t.Fatal("the recent key was pruned")
	}
}
