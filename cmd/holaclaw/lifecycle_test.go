package main

import (
	"context"
	"strings"
	"testing"

	"holaclaw/internal/store/storetest"
)

// createdID returns the id `identity create` printed.
func createdID(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "id" {
			return f[1]
		}
	}
	t.Fatalf("no id in %q", out)
	return ""
}

func TestIdentityCreate(t *testing.T) {
	e := startDaemon(t, "", "")

	code, out, errOut := runCLI(t, "", "identity", "create", "--name", "Rosa", "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity create: exit = %d\n%s", code, errOut)
	}
	id := createdID(t, out)
	if !strings.Contains(out, "name  Rosa") || !strings.Contains(out, "role  user") {
		t.Errorf("identity create = %q", out)
	}

	_, out, _ = runCLI(t, "", "identity", "show", "--config", e.dir, id)
	if !strings.Contains(out, "Rosa · user · user · confirmed") {
		t.Errorf("identity show after create = %q", out)
	}
	_, out, _ = runCLI(t, "", "identity", "list", "--config", e.dir)
	if !strings.Contains(out, id) || !strings.Contains(out, "confirmed") {
		t.Errorf("identity list = %q", out)
	}

	code, out, errOut = runCLI(t, "", "identity", "create", "--name", "Ada", "--role", "admin", "--config", e.dir)
	if code != 0 || !strings.Contains(out, "role  admin") {
		t.Fatalf("create admin: exit = %d, out = %q\n%s", code, out, errOut)
	}
}

func TestIdentityCreateRefused(t *testing.T) {
	e := startDaemon(t, "", "")

	code, _, errOut := runCLI(t, "", "identity", "create", "--config", e.dir)
	if code != 2 || !strings.Contains(errOut, "usage: holaclaw identity create") {
		t.Fatalf("no name: exit = %d, stderr = %q", code, errOut)
	}
	// The server decides what a role may be.
	for _, role := range []string{"guest", "owner", "root"} {
		code, _, errOut = runCLI(t, "", "identity", "create", "--name", "X", "--role", role, "--config", e.dir)
		if code != 1 || !strings.Contains(errOut, "holaclaw:") {
			t.Errorf("role %s: exit = %d, stderr = %q", role, code, errOut)
		}
	}
	code, _, errOut = runCLI(t, "", "identity", "create", "--name", "X", "--ttl", "1h", "--config", e.dir)
	if code != 2 || !strings.Contains(errOut, "--ttl needs --invite") {
		t.Fatalf("--ttl alone: exit = %d, stderr = %q", code, errOut)
	}
}

// --invite prints the first invite of the new identity; an invite the server
// refuses leaves the identity created and says so.
func TestIdentityCreateWithInvite(t *testing.T) {
	e := startDaemon(t, "", "")

	code, out, errOut := runCLI(t, "", "identity", "create", "--name", "Carla", "--invite", "holaclaw", "--ttl", "24h", "--config", e.dir)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	for _, want := range []string{"name  Carla", "code     ", "channel  holaclaw"} {
		if !strings.Contains(out, want) {
			t.Errorf("out = %q, want %q", out, want)
		}
	}

	code, out, errOut = runCLI(t, "", "identity", "create", "--name", "Bo", "--invite", "nope", "--config", e.dir)
	if code != 1 || !strings.Contains(errOut, "Bo was created, but its invite was not") || !strings.Contains(errOut, `unknown channel "nope"`) {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if id := createdID(t, out); id == "" {
		t.Fatal("the identity was not reported")
	}
}

func TestIdentityUnbind(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "109")

	code, out, errOut := runCLI(t, "", "identity", "unbind", "--config", e.dir, "telegram:109")
	if code != 0 {
		t.Fatalf("unbind: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "unbound telegram:109 from "+g) ||
		!strings.Contains(out, "pending identity deleted, 0 memories forgotten") {
		t.Errorf("unbind = %q", out)
	}
	// The pending identity had no other binding, so it went with it.
	code, _, _ = runCLI(t, "", "identity", "show", "--config", e.dir, g)
	if code != 1 {
		t.Fatalf("show after the last unbind: exit = %d, want 1", code)
	}

	// Gone now, so a second unbind finds nothing.
	code, _, errOut = runCLI(t, "", "identity", "unbind", "--config", e.dir, "telegram:109")
	if code != 1 || !strings.Contains(errOut, "holaclaw:") {
		t.Fatalf("second unbind: exit = %d, stderr = %q", code, errOut)
	}
}

func TestIdentityUnbindKeepsAConfirmedIdentity(t *testing.T) {
	e := startDaemon(t, "", "")
	ctx := context.Background()
	owner, err := e.daemon.Auth.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A colon splits only once: the alias is lid:1.
	storetest.Bind(t, e.daemon.App.Store, "wa", []string{"lid:1"}, owner.ID, "")

	_, out, _ := runCLI(t, "", "identity", "show", "--config", e.dir, string(owner.ID))
	if !strings.Contains(out, "wa:lid:1") {
		t.Fatalf("show lists the binding as its full ref: %q", out)
	}
	code, out, errOut := runCLI(t, "", "identity", "unbind", "--config", e.dir, "wa:lid:1")
	if code != 0 {
		t.Fatalf("unbind: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "unbound wa:lid:1 from "+string(owner.ID)) || strings.Contains(out, "deleted") {
		t.Errorf("unbind = %q", out)
	}
	// The owner's CLI token still works with no bindings left.
	code, out, errOut = runCLI(t, "", "identity", "show", "--config", e.dir, string(owner.ID))
	if code != 0 || !strings.Contains(out, "· user · owner · confirmed") || strings.Contains(out, "wa:lid:1") {
		t.Fatalf("show: exit = %d, out = %q\n%s", code, out, errOut)
	}
}

func TestIdentityUnbindBadRef(t *testing.T) {
	e := startDaemon(t, "", "")
	for _, ref := range []string{"telegram", ":109", "telegram:"} {
		code, _, errOut := runCLI(t, "", "identity", "unbind", "--config", e.dir, ref)
		if code != 2 || !strings.Contains(errOut, "usage: holaclaw identity unbind") {
			t.Errorf("unbind %q: exit = %d, stderr = %q", ref, code, errOut)
		}
	}
}

func TestIdentityAssign(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "110")
	_, out, _ := runCLI(t, "", "identity", "list", "--config", e.dir)
	owner := firstColumn(t, out, "owner")

	code, out, errOut := runCLI(t, "", "identity", "assign", g, "--to", owner, "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity assign: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "assigned "+g+" to "+owner) || !strings.Contains(out, "1 bindings") {
		t.Errorf("identity assign = %q", out)
	}
	_, out, _ = runCLI(t, "", "identity", "show", "--config", e.dir, owner)
	if !strings.Contains(out, "telegram:110") {
		t.Errorf("owner after assign = %q", out)
	}
}

func TestIdentityAssignRefused(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "111")
	other := guestOn(t, e, "112")

	code, _, errOut := runCLI(t, "", "identity", "assign", "--config", e.dir, g, "--to", g)
	if code != 1 || !strings.Contains(errOut, "itself") {
		t.Fatalf("assign to itself: exit = %d, stderr %q", code, errOut)
	}
	code, _, errOut = runCLI(t, "", "identity", "assign", "--config", e.dir, g, "--to", other)
	if code != 1 || !strings.Contains(errOut, "pending") {
		t.Fatalf("assign to a pending identity: exit = %d, stderr %q", code, errOut)
	}
	// --to is required, and the pair is no longer positional.
	code, _, errOut = runCLI(t, "", "identity", "assign", "--config", e.dir, g)
	if code != 2 || !strings.Contains(errOut, "usage: holaclaw identity assign") {
		t.Fatalf("no --to: exit = %d, stderr %q", code, errOut)
	}
	code, _, errOut = runCLI(t, "", "identity", "assign", "--config", e.dir, g, other)
	if code != 2 || !strings.Contains(errOut, "usage: holaclaw identity assign") {
		t.Fatalf("two positionals: exit = %d, stderr %q", code, errOut)
	}
}

func TestIdentityDisableEnable(t *testing.T) {
	e := startDaemon(t, "", "")
	_, out, _ := runCLI(t, "", "identity", "create", "--name", "Rosa", "--config", e.dir)
	id := createdID(t, out)

	code, out, errOut := runCLI(t, "", "identity", "disable", "--config", e.dir, id)
	if code != 0 || !strings.Contains(out, "disabled "+id) {
		t.Fatalf("disable: exit = %d, out = %q\n%s", code, out, errOut)
	}
	_, out, _ = runCLI(t, "", "identity", "show", "--config", e.dir, id)
	if !strings.Contains(out, "Rosa · user · user · disabled") {
		t.Errorf("show after disable = %q", out)
	}
	_, out, _ = runCLI(t, "", "identity", "list", "--config", e.dir)
	if !strings.Contains(out, "disabled") {
		t.Errorf("list after disable = %q", out)
	}

	code, out, errOut = runCLI(t, "", "identity", "enable", "--config", e.dir, id)
	if code != 0 || !strings.Contains(out, "enabled "+id) {
		t.Fatalf("enable: exit = %d, out = %q\n%s", code, out, errOut)
	}
	_, out, _ = runCLI(t, "", "identity", "show", "--config", e.dir, id)
	if !strings.Contains(out, "Rosa · user · user · confirmed") {
		t.Errorf("show after enable = %q", out)
	}
}

func TestIdentityDisableRefused(t *testing.T) {
	e := startDaemon(t, "", "")
	g := guestOn(t, e, "113")
	_, out, _ := runCLI(t, "", "identity", "list", "--config", e.dir)
	owner := firstColumn(t, out, "owner")

	for _, id := range []string{owner, g, "01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		code, _, errOut := runCLI(t, "", "identity", "disable", "--config", e.dir, id)
		if code != 1 || !strings.Contains(errOut, "holaclaw:") {
			t.Errorf("disable %s: exit = %d, stderr = %q", id, code, errOut)
		}
	}
	code, _, errOut := runCLI(t, "", "identity", "disable", "--config", e.dir)
	if code != 2 || !strings.Contains(errOut, "usage: holaclaw identity disable") {
		t.Errorf("no id: exit = %d, stderr = %q", code, errOut)
	}
}
