package access

import (
	"context"
	"errors"
	"testing"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// duplicates counts the router.dropped events with reason duplicate.
func (h *harness) duplicates() int {
	n := 0
	for _, d := range h.dropped() {
		if d["reason"] == "duplicate" {
			n++
		}
	}
	return n
}

func TestResendUnderAnotherAddressingIsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	h.identity("9", store.RoleUser)
	first := dm("9", "hi")
	first.Origin.Conversation = "phone-9"
	first.Origin.PlatformMsgID = "phone-9:M1"
	first.DedupeKeys = []string{"phone-9|M1", "lid-9|M1"}
	h.mustIn(first)

	// The resend comes LID-addressed: another conversation id and message id,
	// one key in common.
	resend := dm("9", "hi")
	resend.Origin.Conversation = "lid-9"
	resend.Origin.PlatformMsgID = "lid-9:M1"
	resend.DedupeKeys = []string{"lid-9|M1"}
	if err := h.in(resend); !errors.Is(err, channel.ErrDuplicate) {
		t.Fatalf("resend: err = %v, want ErrDuplicate", err)
	}
	routed, notices := h.counts()
	if routed != 1 || len(notices) != 0 {
		t.Fatalf("routed %d, notices %v", routed, notices)
	}
	if n := h.duplicates(); n != 1 {
		t.Fatalf("duplicate drops = %d, want 1", n)
	}
}

func TestResendAfterRestartIsDropped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env := dm("9", "hi")
	env.Origin.PlatformMsgID = "c-9:1"

	before := newHarnessIn(t, dir, invite("invite"))
	before.identity("9", store.RoleUser)
	before.mustIn(env)
	if routed, _ := before.counts(); routed != 1 {
		t.Fatalf("routed %d before the restart", routed)
	}
	if err := before.st.Close(); err != nil {
		t.Fatal(err)
	}

	after := newHarnessIn(t, dir, invite("invite"))
	if err := after.in(env); !errors.Is(err, channel.ErrDuplicate) {
		t.Fatalf("resend after restart: err = %v, want ErrDuplicate", err)
	}
	if routed, notices := after.counts(); routed != 0 || len(notices) != 0 {
		t.Fatalf("routed %d, notices %v after the restart", routed, notices)
	}
	if n := after.duplicates(); n != 1 {
		t.Fatalf("duplicate drops = %d, want 1", n)
	}
}

// TestThirdCopyAfterRestartIsDropped: a phone copy {p}, then a duplicate
// {p, l} that adds the LID key, then after a restart a copy carrying {l}
// only. The key the duplicate added was recorded durably, so the third copy
// is dropped too.
func TestThirdCopyAfterRestartIsDropped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	copyOf := func(conv string, keys ...string) event.Envelope {
		env := dm("9", "hi")
		env.Origin.Conversation = conv
		env.Origin.PlatformMsgID = conv + "/M1"
		env.DedupeKeys = keys
		return env
	}

	before := newHarnessIn(t, dir, invite("invite"))
	before.identity("9", store.RoleUser)
	before.mustIn(copyOf("phone-9", "p/M1"))
	if err := before.in(copyOf("lid-9", "l/M1", "p/M1")); !errors.Is(err, channel.ErrDuplicate) {
		t.Fatalf("second copy: err = %v, want ErrDuplicate", err)
	}
	if err := before.st.Close(); err != nil {
		t.Fatal(err)
	}

	after := newHarnessIn(t, dir, invite("invite"))
	if err := after.in(copyOf("lid-9", "l/M1")); !errors.Is(err, channel.ErrDuplicate) {
		t.Fatalf("third copy after restart: err = %v, want ErrDuplicate", err)
	}
	if r1, _ := before.counts(); r1 != 1 {
		t.Fatalf("routed %d before the restart, want 1", r1)
	}
	if r2, _ := after.counts(); r2 != 0 {
		t.Fatalf("routed %d after the restart, want 0", r2)
	}
}

func TestConsumedInviteResentAfterLinkingIsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	env := dm("9", "/start "+h.code(t, "tg-main"))
	env.Origin.PlatformMsgID = "c-9:1"
	h.mustIn(env)
	if _, notices := h.counts(); len(notices) != 1 || notices[0] != noticeLinked {
		t.Fatalf("notices = %v", notices)
	}

	// Linked now, so without the record the code would route as a message.
	if err := h.in(env); !errors.Is(err, channel.ErrDuplicate) {
		t.Fatalf("resent code: err = %v, want ErrDuplicate", err)
	}
	routed, notices := h.counts()
	if routed != 0 || len(notices) != 1 {
		t.Fatalf("routed %d, notices %v", routed, notices)
	}
}

func TestDroppedStrangerIsRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	env := dm("9", "hello?")
	env.Origin.PlatformMsgID = "c-9:1"
	h.mustIn(env)
	h.identity("9", store.RoleUser)
	if err := h.in(env); !errors.Is(err, channel.ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
	if routed, _ := h.counts(); routed != 0 {
		t.Fatalf("routed %d", routed)
	}
}

func TestPrunedKeyIsProcessedAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	h.identity("9", store.RoleUser)
	env := dm("9", "hi")
	env.Origin.PlatformMsgID = "c-9:1"
	h.mustIn(env)
	h.advance(15 * 24 * time.Hour)
	n, err := h.st.PruneInbound(context.Background(), h.now().Add(-14*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	h.mustIn(env)
	if routed, _ := h.counts(); routed != 2 {
		t.Fatalf("routed %d, want the pruned message routed again", routed)
	}
}

func TestFailedHandlingReleasesKeys(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	h.identity("9", store.RoleUser)
	env := dm("9", "hi")
	env.Origin.PlatformMsgID = "c-9:1"
	env.DedupeKeys = []string{"9|1", "lid9|1"}

	h.mu.Lock()
	h.routeErr = errTest
	h.mu.Unlock()
	if err := h.in(env); !errors.Is(err, errTest) {
		t.Fatalf("err = %v, want the route error", err)
	}
	h.mu.Lock()
	h.routeErr = nil
	h.mu.Unlock()

	h.mustIn(env)
	if routed, _ := h.counts(); routed != 1 {
		t.Fatalf("routed %d, want the redelivery routed", routed)
	}
	if n := h.duplicates(); n != 0 {
		t.Fatalf("redelivery dropped as duplicate")
	}
}

func TestNoKeysNoDedupe(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	h.identity("9", store.RoleUser)
	h.mustIn(dm("9", "hi"))
	h.mustIn(dm("9", "hi"))
	if routed, _ := h.counts(); routed != 2 {
		t.Fatalf("routed %d, want both", routed)
	}
}

func TestNilDedupeRoutesEveryCopy(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	h.g.d.Dedupe = nil
	h.identity("9", store.RoleUser)
	env := dm("9", "hi")
	env.Origin.PlatformMsgID = "c-9:1"
	h.mustIn(env)
	h.mustIn(env)
	if routed, _ := h.counts(); routed != 2 {
		t.Fatalf("routed %d, want both", routed)
	}
}

func TestClaimFailureIsTransient(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	env := dm("9", "hi")
	env.Origin.PlatformMsgID = "c-9:1"
	_ = h.st.Close()
	err := h.in(env)
	if err == nil || errors.Is(err, channel.ErrDuplicate) {
		t.Fatalf("err = %v, want a transient error", err)
	}
}
