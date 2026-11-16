package check

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDAT001(t *testing.T) {
	t.Run("creatable", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		fs := dat001Rule().Run(context.Background(), &Input{DataDir: dir})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("data dir was not created: %v", err)
		}
	})
	t.Run("not writable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root ignores permission bits")
		}
		parent := t.TempDir()
		if err := os.Chmod(parent, 0o500); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(parent, 0o700) }()
		dir := filepath.Join(parent, "data")
		fs := dat001Rule().Run(context.Background(), &Input{DataDir: dir})
		if len(fs) != 1 || fs[0].ID != "DAT001" || fs[0].Severity != SeverityError {
			t.Fatalf("findings = %+v", fs)
		}
	})
}

func TestDAT002(t *testing.T) {
	t.Run("no db yet", func(t *testing.T) {
		fs := dat002Rule().Run(context.Background(), &Input{DataDir: t.TempDir()})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("db mode 600", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "holaclaw.db"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		fs := dat002Rule().Run(context.Background(), &Input{DataDir: dir})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("db world readable", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "holaclaw.db"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		fs := dat002Rule().Run(context.Background(), &Input{DataDir: dir})
		if len(fs) != 1 || fs[0].ID != "DAT002" || fs[0].Severity != SeverityWarning {
			t.Fatalf("findings = %+v", fs)
		}
	})
}
