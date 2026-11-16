package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// roomOn makes the pending room identity the first accepted room message does.
func roomOn(t *testing.T, e *daemonEnv, conversation string) string {
	t.Helper()
	i, _, err := e.daemon.App.Store.EnsureRoom(context.Background(), "telegram", conversation, time.Now())
	if err != nil {
		t.Fatalf("ensure room: %v", err)
	}
	return string(i.ID)
}

func TestIdentityConfirmRoom(t *testing.T) {
	e := startDaemon(t, "", "")
	r := roomOn(t, e, "-100")

	code, out, errOut := runCLI(t, "", "identity", "confirm", r, "--name", "Ops", "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity confirm: exit = %d\n%s", code, errOut)
	}
	if strings.TrimSpace(out) != "confirmed "+r+" as Ops" {
		t.Errorf("identity confirm = %q, want no role in parentheses", out)
	}

	// Confirmed, a room is not pending any more, and `set` renames it.
	code, _, errOut = runCLI(t, "", "identity", "confirm", r, "--name", "Ops2", "--config", e.dir)
	if code != 1 || !strings.Contains(errOut, "is not pending") {
		t.Fatalf("second confirm: exit = %d, stderr = %q", code, errOut)
	}
	code, _, errOut = runCLI(t, "", "identity", "set", r, "--name", "Ops3", "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity set: exit = %d\n%s", code, errOut)
	}
}

func TestIdentityListAndShowShowRooms(t *testing.T) {
	e := startDaemon(t, "", "")
	r := roomOn(t, e, "-100")
	if code, _, errOut := runCLI(t, "", "identity", "confirm", r, "--name", "Ops", "--config", e.dir); code != 0 {
		t.Fatalf("confirm: %d\n%s", code, errOut)
	}

	code, out, errOut := runCLI(t, "", "identity", "list", "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity list: exit = %d\n%s", code, errOut)
	}
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, r) {
			line = l
		}
	}
	if f := strings.Fields(line); len(f) < 6 || f[1] != "room" || f[2] != "confirmed" || f[3] != "Ops" || f[4] != "-" || f[5] != "telegram:-100" {
		t.Errorf("room row = %q in\n%s", line, out)
	}

	code, out, errOut = runCLI(t, "", "identity", "show", r, "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity show: exit = %d\n%s", code, errOut)
	}
	if want := "Ops · room · - · confirmed"; !strings.Contains(out, want) {
		t.Errorf("identity show = %q, missing %q", out, want)
	}
}

// A room is disabled from either status and enabled back to pending; its one
// binding can't be unbound, and a pending user can't be disabled.
func TestIdentityDisableAndUnbindRoom(t *testing.T) {
	e := startDaemon(t, "", "")
	r := roomOn(t, e, "-100")

	code, out, errOut := runCLI(t, "", "identity", "disable", r, "--config", e.dir)
	if code != 0 || !strings.Contains(out, "disabled "+r) {
		t.Fatalf("disable: exit = %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = runCLI(t, "", "identity", "show", r, "--config", e.dir)
	if code != 0 || !strings.Contains(out, "· disabled") {
		t.Fatalf("show: exit = %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut = runCLI(t, "", "identity", "enable", r, "--config", e.dir); code != 0 {
		t.Fatalf("enable: exit = %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = runCLI(t, "", "identity", "show", r, "--config", e.dir)
	if code != 0 || !strings.Contains(out, "· pending") {
		t.Fatalf("show: exit = %d\n%s\n%s", code, out, errOut)
	}

	code, _, errOut = runCLI(t, "", "identity", "unbind", "telegram:-100", "--config", e.dir)
	if code != 1 || !strings.Contains(errOut, "a room keeps its one binding") {
		t.Fatalf("unbind: exit = %d, stderr = %q", code, errOut)
	}

	g := guestOn(t, e, "201")
	if code, _, _ = runCLI(t, "", "identity", "disable", g, "--config", e.dir); code != 1 {
		t.Fatalf("disable a pending user: exit = %d", code)
	}
}

// `identity show` prints an approved line for an approved room only.
func TestIdentityShowApprovedLine(t *testing.T) {
	e := startDaemon(t, "", "")
	show := func(id string) string {
		t.Helper()
		code, out, errOut := runCLI(t, "", "identity", "show", id, "--config", e.dir)
		if code != 0 {
			t.Fatalf("identity show %s: exit = %d\n%s", id, code, errOut)
		}
		return out
	}
	hasApproved := func(out string) bool {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "approved ") {
				return true
			}
		}
		return false
	}

	approved := roomOn(t, e, "-200")
	if code, _, errOut := runCLI(t, "", "identity", "confirm", approved, "--name", "Ops", "--config", e.dir); code != 0 {
		t.Fatalf("confirm: %d\n%s", code, errOut)
	}
	owner, err := e.daemon.Auth.Owner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out := show(approved); !hasApproved(out) || !strings.Contains(out, " by "+string(owner.ID)+"\n") {
		t.Errorf("approved room by the socket shows no approved line, or not the owner it arrives as:\n%s", out)
	}
	byAdmin := roomOn(t, e, "-400")
	if _, _, err := e.daemon.App.Store.ApproveRoom(context.Background(), "telegram", "-400", "adm1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if out := show(byAdmin); !strings.Contains(out, " by adm1\n") || !hasApproved(out) {
		t.Errorf("room approved by an admin shows no approver:\n%s", out)
	}
	if out := show(roomOn(t, e, "-100")); hasApproved(out) {
		t.Errorf("pending room shows an approved line:\n%s", out)
	}
	disabled := roomOn(t, e, "-300")
	if _, err := e.daemon.App.Store.DB().Exec(`UPDATE identities SET status = 'disabled' WHERE id = ?`, disabled); err != nil {
		t.Fatal(err)
	}
	if out := show(disabled); hasApproved(out) {
		t.Errorf("disabled room shows an approved line:\n%s", out)
	}
	user := guestOn(t, e, "104")
	if code, _, errOut := runCLI(t, "", "identity", "confirm", user, "--name", "Ada", "--config", e.dir); code != 0 {
		t.Fatalf("confirm user: %d\n%s", code, errOut)
	}
	if out := show(user); hasApproved(out) {
		t.Errorf("user shows an approved line:\n%s", out)
	}
}

func TestInviteCreateRoomNeedsAnApprovedChannel(t *testing.T) {
	e := startDaemon(t, "", "")
	code, _, errOut := runCLI(t, "", "invite", "create", "--room", "--channel", "nope", "--config", e.dir)
	if code != 1 || !strings.Contains(errOut, `unknown channel "nope"`) {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	code, _, errOut = runCLI(t, "", "invite", "create", "--group", "--channel", "nope", "--config", e.dir)
	if code != 2 || !strings.Contains(errOut, "unknown flag: --group") {
		t.Fatalf("--group: exit = %d, stderr = %q", code, errOut)
	}
}
