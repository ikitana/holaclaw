package main

import (
	"context"
	"strings"
	"testing"
)

// guest makes a pending identity the way an open DM, or an unknown room
// member, does — the same shortcut internal/api's own tests use.
func guestOn(t *testing.T, e *daemonEnv, alias string) string {
	t.Helper()
	i, err := e.daemon.Auth.EnsureGuest(context.Background(), "telegram", []string{alias}, "Guest "+alias)
	if err != nil {
		t.Fatalf("ensure guest: %v", err)
	}
	return string(i.ID)
}

func TestIdentityList(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "101")

	code, out, errOut := runCLI(t, "", "identity", "list", "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity list: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "owner") || !strings.Contains(out, g) || !strings.Contains(out, "telegram:101") {
		t.Errorf("identity list = %q", out)
	}

	code, out, errOut = runCLI(t, "", "identity", "list", "--pending", "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity list --pending: exit = %d\n%s", code, errOut)
	}
	if strings.Contains(out, "owner") {
		t.Errorf("--pending still listed the owner: %q", out)
	}
	if !strings.Contains(out, g) {
		t.Errorf("--pending missing the guest: %q", out)
	}
}

func TestIdentityListEmpty(t *testing.T) {
	// A fresh host has only the owner, so --pending finds nobody.
	e := startDaemon(t, "", "")
	code, out, errOut := runCLI(t, "", "identity", "list", "--pending", "--config", e.dir)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "no identities") {
		t.Errorf("identity list --pending = %q", out)
	}
}

func TestIdentityShow(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "102")

	code, out, errOut := runCLI(t, "", "identity", "show", "--config", e.dir, g)
	if code != 0 {
		t.Fatalf("identity show: exit = %d\n%s", code, errOut)
	}
	for _, want := range []string{"· user · guest · pending", "  id " + g, "telegram:102", `"Guest 102"`, "since "} {
		if !strings.Contains(out, want) {
			t.Errorf("identity show = %q, missing %q", out, want)
		}
	}
}

func TestIdentityShowUnknown(t *testing.T) {
	e := startDaemon(t, "", "")
	code, _, errOut := runCLI(t, "", "identity", "show", "--config", e.dir, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "holaclaw:") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestIdentityConfirm(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "103")

	code, out, errOut := runCLI(t, "", "identity", "confirm", "--name", "Ada", "--config", e.dir, g)
	if code != 0 {
		t.Fatalf("identity confirm: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "confirmed "+g+" as Ada (user)") {
		t.Errorf("identity confirm = %q", out)
	}

	// A second confirm is rejected without reaching the server: the identity
	// is no longer a guest.
	code, _, errOut = runCLI(t, "", "identity", "confirm", "--name", "Ada2", "--config", e.dir, g)
	if code != 1 {
		t.Fatalf("second confirm: exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "is not pending") {
		t.Errorf("second confirm stderr = %q", errOut)
	}

	// show still says "Ada", not "Ada2": the rejected confirm never patched.
	_, out, _ = runCLI(t, "", "identity", "show", "--config", e.dir, g)
	if !strings.Contains(out, "Ada · user · user · confirmed") || strings.Contains(out, "Ada2") {
		t.Errorf("identity show after rejected confirm = %q", out)
	}
}

func TestIdentityConfirmWithRole(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "104")

	code, out, errOut := runCLI(t, "", "identity", "confirm", g, "--name", "Bo", "--role", "admin", "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity confirm: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "confirmed "+g+" as Bo (admin)") {
		t.Errorf("identity confirm = %q", out)
	}
}

func TestIdentityConfirmNonGuestRejected(t *testing.T) {
	e := startDaemon(t, "", "")
	// The owner is not pending.
	code, out, errOut := runCLI(t, "", "identity", "list", "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity list: exit = %d\n%s", code, errOut)
	}
	owner := firstColumn(t, out, "owner")

	code, _, errOut = runCLI(t, "", "identity", "confirm", "--name", "Nope", "--config", e.dir, owner)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "is not pending") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestIdentityConfirmMissingName(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "105")

	code, _, errOut := runCLI(t, "", "identity", "confirm", "--config", e.dir, g)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "usage: holaclaw identity confirm") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestIdentitySet(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "106")
	if code, _, errOut := runCLI(t, "", "identity", "confirm", "--name", "Cal", "--config", e.dir, g); code != 0 {
		t.Fatalf("confirm: exit = %d\n%s", code, errOut)
	}

	code, out, errOut := runCLI(t, "", "identity", "set", g, "--name", "Callie", "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity set: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "updated "+g) {
		t.Errorf("identity set = %q", out)
	}

	_, out, _ = runCLI(t, "", "identity", "show", "--config", e.dir, g)
	if !strings.Contains(out, "Callie · user · user · confirmed") {
		t.Errorf("identity show after set = %q", out)
	}
}

func TestIdentitySetNoFlags(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "107")
	if code, _, errOut := runCLI(t, "", "identity", "confirm", "--name", "Dee", "--config", e.dir, g); code != 0 {
		t.Fatalf("confirm: exit = %d\n%s", code, errOut)
	}

	code, _, errOut := runCLI(t, "", "identity", "set", "--config", e.dir, g)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "usage: holaclaw identity set") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestIdentitySetOnGuestRefused(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "108")

	code, _, errOut := runCLI(t, "", "identity", "set", "--name", "Should Not", "--config", e.dir, g)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "is pending: use identity confirm") {
		t.Errorf("stderr = %q", errOut)
	}

	// The refusal never reached the server: still a guest, still unnamed.
	_, out, _ := runCLI(t, "", "identity", "show", "--config", e.dir, g)
	if !strings.Contains(out, "guest · pending") {
		t.Errorf("identity show after refused set = %q", out)
	}
}

func TestIdentityConfirmUnknownID(t *testing.T) {
	e := startDaemon(t, "", "")
	code, _, errOut := runCLI(t, "", "identity", "confirm", "--name", "X", "--config", e.dir, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "holaclaw:") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestIdentityOwnerRoleRejected(t *testing.T) {
	// The server refuses to change the owner's role (409); the CLI must
	// surface that, not swallow it.
	e := startDaemon(t, "", "")
	code, out, errOut := runCLI(t, "", "identity", "list", "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity list: exit = %d\n%s", code, errOut)
	}
	owner := firstColumn(t, out, "owner")

	code, _, errOut = runCLI(t, "", "identity", "set", "--role", "user", "--config", e.dir, owner)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "holaclaw:") {
		t.Errorf("stderr = %q", errOut)
	}
}

// firstColumn returns the ID (first tab-separated column) of the row whose
// line contains want.
func firstColumn(t *testing.T, table, want string) string {
	t.Helper()
	for _, line := range strings.Split(table, "\n") {
		if strings.Contains(line, want) {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				t.Fatalf("no fields in line %q", line)
			}
			return fields[0]
		}
	}
	t.Fatalf("no line with %q in:\n%s", want, table)
	return ""
}

func TestIdentityDelete(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "109")

	code, out, errOut := runCLI(t, "", "identity", "delete", "--config", e.dir, g)
	if code != 0 {
		t.Fatalf("identity delete: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "deleted "+g+" (guest): 1 aliases removed") || !strings.Contains(out, "memory rows forgotten") {
		t.Errorf("identity delete = %q", out)
	}

	code, _, errOut = runCLI(t, "", "identity", "show", "--config", e.dir, g)
	if code != 1 {
		t.Fatalf("show after delete: exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "holaclaw:") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestIdentityDeleteKeepMemory(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "110")

	code, out, errOut := runCLI(t, "", "identity", "delete", "--keep-memory", "--config", e.dir, g)
	if code != 0 {
		t.Fatalf("identity delete --keep-memory: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "memory kept") || strings.Contains(out, "memory rows forgotten") {
		t.Errorf("identity delete --keep-memory = %q", out)
	}
}

func TestIdentityDeleteOwnerRejected(t *testing.T) {
	e := startDaemon(t, "", "")
	code, out, errOut := runCLI(t, "", "identity", "list", "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity list: exit = %d\n%s", code, errOut)
	}
	owner := firstColumn(t, out, "owner")

	code, _, errOut = runCLI(t, "", "identity", "delete", "--config", e.dir, owner)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "holaclaw:") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestIdentityDeleteMissingID(t *testing.T) {
	e := startDaemon(t, "", "")
	code, _, errOut := runCLI(t, "", "identity", "delete", "--config", e.dir)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "usage: holaclaw identity delete") {
		t.Errorf("stderr = %q", errOut)
	}
}
