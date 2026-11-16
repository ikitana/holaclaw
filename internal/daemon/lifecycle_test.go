package daemon

import (
	"context"
	"testing"

	"holaclaw/internal/testmodel"
	"holaclaw/internal/tool"
)

// A disabled admin is no admin: its answers are ignored.
func TestDisabledAdminCannotAnswerApprovals(t *testing.T) {
	rc := &reactChannel{}
	rr := newReactRig(t, rc, rc)
	ctx := context.Background()
	a := rr.ask(t)
	if _, err := rr.d.App.Store.DB().ExecContext(ctx, `UPDATE identities SET role = 'admin', status = 'disabled' WHERE id = ?`, string(rr.sess.UserID)); err != nil {
		t.Fatal(err)
	}
	if rr.cs.isAdmin(ctx, rr.sess.UserID) {
		t.Fatal("a disabled admin counts as an admin")
	}
	if rr.cs.resolveApproval(ctx, "fake", a.ID, "once", []string{"fake:1"}) {
		t.Fatal("a disabled admin's press resolved the approval")
	}
	if got := rr.status(t, a.ID); got.Status != tool.ApprovalPending {
		t.Fatalf("status = %s", got.Status)
	}

	if _, err := rr.d.App.Store.DB().ExecContext(ctx, `UPDATE identities SET status = 'confirmed' WHERE id = ?`, string(rr.sess.UserID)); err != nil {
		t.Fatal(err)
	}
	if !rr.cs.resolveApproval(ctx, "fake", a.ID, "once", []string{"fake:1"}) {
		t.Fatal("enabled again, the press was ignored")
	}
}

// The daemon wires the hooks that put a re-enabled creator's tasks through the
// missed-run policy, before the flip, and wake the scheduler after it.
func TestDaemonWiresEnableHooks(t *testing.T) {
	e := startTG(t, tgOpts{dm: "open"}, testmodel.Step{Text: "hi"})
	if e.daemon.Auth.BeforeEnable == nil || e.daemon.Auth.AfterEnable == nil {
		t.Fatal("the enable hooks are not wired")
	}
}
