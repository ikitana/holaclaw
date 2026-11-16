package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// serve runs the daemon the way `holaclaw run` does — through Serve, so it can
// restart itself — on a config directory that already has an owner.
func (e *env) serve() (stop func()) {
	t := e.t
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Options{
			ConfigDir: e.dir, Logger: testLogger(t), Debounce: -1,
			OnListen: func(string, net.Addr) {},
		})
	}()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon.Serve: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not shut down")
		}
	}
	t.Cleanup(stop)
	e.await("the daemon to come up", func() bool {
		_, err := e.client.Status(context.Background())
		return err == nil
	})
	return stop
}

func (e *env) await(what string, ok func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tokenNames lists the API tokens by name; ok is false while the daemon is
// away.
func (e *env) tokenNames() (map[string]bool, bool) {
	tokens, err := e.client.ListTokens(context.Background(), gen.ListTokensParams{All: ptrOf(true)})
	if err != nil {
		return nil, false
	}
	names := map[string]bool{}
	for _, tok := range tokens {
		names[tok.Name] = true
	}
	return names, true
}

func (e *env) token(name string) {
	e.t.Helper()
	if _, err := e.client.CreateToken(context.Background(), gen.TokenCreate{Name: name, Scope: ptrOf(gen.TokenCreateScopeAdmin), UserId: ptrOf(e.ownerID())}); err != nil {
		e.t.Fatalf("create token %s: %v", name, err)
	}
}

// The whole restore, through the API: back up, change the state, restore.
// The daemon restarts by itself and comes back on the data of the backup,
// with its managed backups intact and the replaced database set aside.
func TestRestoreRestartsOntoTheBackup(t *testing.T) {
	e := start(t, opts{})
	e.stop() // the owner exists; from here on the daemon runs under Serve
	stop := e.serve()
	ctx := context.Background()

	e.token("before-the-backup")
	created, err := e.client.CreateBackup(ctx, gen.BackupCreate{})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	e.await("the backup to be ready", func() bool {
		b, err := e.client.GetBackup(ctx, created.Id)
		if err != nil || b.Status == gen.BackupStatusFailed {
			t.Fatalf("backup = %+v, %v", b, err)
		}
		return b.Status == gen.BackupStatusReady
	})
	e.token("after-the-backup")
	if names, _ := e.tokenNames(); !names["before-the-backup"] || !names["after-the-backup"] {
		t.Fatalf("tokens before the restore = %v", names)
	}

	if got, err := e.client.RestoreBackup(ctx, created.Id); err != nil || got.Id != created.Id {
		t.Fatalf("RestoreBackup = %+v, %v", got, err)
	}
	e.await("the daemon to come back on the restored data", func() bool {
		names, ok := e.tokenNames()
		return ok && names["before-the-backup"] && !names["after-the-backup"]
	})

	// The backup it restored from is still there, and still restorable.
	b, err := e.client.GetBackup(ctx, created.Id)
	if err != nil || b.Status != gen.BackupStatusReady {
		t.Fatalf("the backup after its restore = %+v, %v", b, err)
	}
	if v, err := e.client.VerifyBackup(ctx, created.Id); err != nil || !v.Ok {
		t.Fatalf("VerifyBackup = %+v, %v", v, err)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "holaclaw.db.pre-restore")); err != nil {
		t.Errorf("the replaced database was not kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "restore")); !os.IsNotExist(err) {
		t.Errorf("the staged restore is still there (%v)", err)
	}
	// A second restore works the same way: nothing was left half-done.
	e.token("after-the-first-restore")
	if _, err := e.client.RestoreBackup(ctx, created.Id); err != nil {
		t.Fatalf("second RestoreBackup: %v", err)
	}
	e.await("the second restore", func() bool {
		names, ok := e.tokenNames()
		return ok && names["before-the-backup"] && !names["after-the-first-restore"]
	})

	// The restored database says it was restored, and from what.
	stop()
	st, err := store.Open(ctx, e.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	events, err := st.ListEventsAfter(ctx, "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	restored := 0
	for _, ev := range events {
		if ev.Type == event.BackupRestored {
			restored++
			if attrs := string(ev.Attrs); !strings.Contains(attrs, created.Id) {
				t.Errorf("backup.restored does not name the backup: %s", attrs)
			}
		}
	}
	// Each restore brings back a database from before any restore, so only
	// the last one's event is in it.
	if restored != 1 {
		t.Errorf("%d backup.restored events, want 1", restored)
	}
}
