package check

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
)

func TestSEC001(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		dir := t.TempDir()
		_, path := openSecrets(t, dir) // Open leaves nothing on disk until a write
		if err := os.WriteFile(path, []byte("version = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fs := sec001Rule().Run(context.Background(), &Input{ConfigDir: dir, SecretsPath: path})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("world readable", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "auth.toml")
		if err := os.WriteFile(path, []byte("version = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		fs := sec001Rule().Run(context.Background(), &Input{ConfigDir: dir, SecretsPath: path})
		if len(fs) != 1 || fs[0].ID != "SEC001" || fs[0].Severity != SeverityError {
			t.Fatalf("findings = %+v", fs)
		}
	})
}

func TestSEC002(t *testing.T) {
	base := func() *config.Config {
		cfg := minimalConfig()
		p := cfg.Providers["ollama"]
		p.Auth = "api-key"
		cfg.Providers["ollama"] = p
		return cfg
	}
	t.Run("missing", func(t *testing.T) {
		fs := sec002Rule().Run(context.Background(), &Input{Config: base()})
		if len(fs) != 1 || fs[0].ID != "SEC002" || fs[0].Severity != SeverityError {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("env resolves", func(t *testing.T) {
		t.Setenv("TEST_SEC002_KEY", "sekret-value")
		cfg := base()
		p := cfg.Providers["ollama"]
		p.APIKey = "env:TEST_SEC002_KEY"
		cfg.Providers["ollama"] = p
		fs := sec002Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("auth.toml entry", func(t *testing.T) {
		dir := t.TempDir()
		f, _ := openSecrets(t, dir)
		if err := f.Set(secrets.Providers, "ollama", map[string]string{"api_key": "x"}); err != nil {
			t.Fatal(err)
		}
		fs := sec002Rule().Run(context.Background(), &Input{Config: base(), Secrets: f})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}

func TestSEC007(t *testing.T) {
	cases := []struct {
		name, auth, apiKey string
		stored             bool
		want               int
	}{
		{"no auth, no key", "none", "", false, 0},
		{"no auth, stored key", "none", "", true, 1},
		{"auth unset, stored key", "", "", true, 1},
		{"no auth, env key", "none", "env:TEST_SEC007_KEY", false, 1},
		{"api-key auth, stored key", "api-key", "", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := minimalConfig()
			p := cfg.Providers["ollama"]
			p.Auth, p.APIKey = c.auth, c.apiKey
			cfg.Providers["ollama"] = p
			f, _ := openSecrets(t, t.TempDir())
			if c.stored {
				if err := f.Set(secrets.Providers, "ollama", map[string]string{"api_key": "x"}); err != nil {
					t.Fatal(err)
				}
			}
			fs := sec007Rule().Run(context.Background(), &Input{Config: cfg, Secrets: f})
			if len(fs) != c.want {
				t.Fatalf("findings = %+v, want %d", fs, c.want)
			}
			if c.want == 1 && (fs[0].ID != "SEC007" || fs[0].Severity != SeverityWarning || fs[0].Key != "providers.ollama.auth") {
				t.Errorf("finding = %+v", fs[0])
			}
		})
	}
}

func TestSEC003(t *testing.T) {
	base := func() *config.Config {
		cfg := minimalConfig()
		cfg.Channels["tg-main"] = config.Channel{Name: "tg-main", Type: "telegram", Agent: "main"}
		return cfg
	}
	t.Run("missing telegram token", func(t *testing.T) {
		fs := sec003Rule().Run(context.Background(), &Input{Config: base()})
		if len(fs) != 1 || fs[0].ID != "SEC003" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("telegram ok", func(t *testing.T) {
		dir := t.TempDir()
		f, _ := openSecrets(t, dir)
		if err := f.Set(secrets.Channels, "tg-main", map[string]string{"bot_token": "x"}); err != nil {
			t.Fatal(err)
		}
		fs := sec003Rule().Run(context.Background(), &Input{Config: base(), Secrets: f})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("slack needs both", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Channels["sl-main"] = config.Channel{Name: "sl-main", Type: "slack", Agent: "main"}
		dir := t.TempDir()
		f, _ := openSecrets(t, dir)
		if err := f.Set(secrets.Channels, "sl-main", map[string]string{"bot_token": "x"}); err != nil {
			t.Fatal(err)
		}
		fs := sec003Rule().Run(context.Background(), &Input{Config: cfg, Secrets: f})
		if len(fs) != 1 || fs[0].Key != "channels.sl-main" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("missing googlechat service account", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Channels["gc-main"] = config.Channel{Name: "gc-main", Type: "googlechat", Agent: "main"}
		fs := sec003Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "SEC003" || fs[0].Key != "channels.gc-main" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("googlechat ok", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Channels["gc-main"] = config.Channel{Name: "gc-main", Type: "googlechat", Agent: "main"}
		dir := t.TempDir()
		f, _ := openSecrets(t, dir)
		if err := f.Set(secrets.Channels, "gc-main", map[string]string{"service_account": "x"}); err != nil {
			t.Fatal(err)
		}
		fs := sec003Rule().Run(context.Background(), &Input{Config: cfg, Secrets: f})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}

func TestSEC004(t *testing.T) {
	dir := t.TempDir()
	f, path := openSecrets(t, dir)
	if err := f.Set(secrets.Providers, "ghost", map[string]string{"api_key": "x"}); err != nil {
		t.Fatal(err)
	}
	fs := sec004Rule().Run(context.Background(), &Input{Config: minimalConfig(), Secrets: f, SecretsPath: path, ConfigDir: dir})
	if len(fs) != 1 || fs[0].ID != "SEC004" || fs[0].Severity != SeverityWarning || fs[0].Key != "providers.ghost" {
		t.Fatalf("findings = %+v", fs)
	}

	t.Run("no stale entries", func(t *testing.T) {
		dir2 := t.TempDir()
		f2, path2 := openSecrets(t, dir2)
		if err := f2.Set(secrets.Providers, "ollama", map[string]string{"api_key": "x"}); err != nil {
			t.Fatal(err)
		}
		fs := sec004Rule().Run(context.Background(), &Input{Config: minimalConfig(), Secrets: f2, SecretsPath: path2, ConfigDir: dir2})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}

func TestSEC005(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		dir := t.TempDir()
		fs := sec005Rule().Run(context.Background(), &Input{ConfigDir: dir})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("world writable file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(p, []byte("x"), 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o666); err != nil { // WriteFile's mode is masked by umask
			t.Fatal(err)
		}
		fs := sec005Rule().Run(context.Background(), &Input{ConfigDir: dir})
		if len(fs) != 1 || fs[0].ID != "SEC005" || fs[0].File != "config.toml" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("world writable dir", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "cfg")
		if err := os.Mkdir(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil { // Mkdir's mode is masked by umask
			t.Fatal(err)
		}
		fs := sec005Rule().Run(context.Background(), &Input{ConfigDir: dir})
		if len(fs) != 1 || fs[0].ID != "SEC005" {
			t.Fatalf("findings = %+v", fs)
		}
	})
}

func TestSEC006(t *testing.T) {
	// session writes a session database under dataDir with the device
	// table whatsmeow creates, holding devices rows.
	session := func(t *testing.T, dataDir string, devices int) {
		t.Helper()
		p := filepath.Join(dataDir, "whatsapp", "wa.db")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec(`CREATE TABLE whatsmeow_device (jid TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		for i := range devices {
			if _, err := db.Exec(`INSERT INTO whatsmeow_device (jid) VALUES (?)`, fmt.Sprintf("3460000000%d@s.whatsapp.net", i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := func() *config.Config {
		c := minimalConfig()
		c.Channels["wa"] = config.Channel{Name: "wa", Type: "whatsapp", Agent: "main"}
		c.Channels["tg"] = config.Channel{Name: "tg", Type: "telegram", Agent: "main"}
		return c
	}
	unlinked := func(t *testing.T, fs []Finding) {
		t.Helper()
		if len(fs) != 1 || fs[0].ID != "SEC006" || fs[0].Severity != SeverityWarning || fs[0].Key != "channels.wa" ||
			fs[0].Fix != "run `holaclaw login whatsapp --name wa --phone <number>`" {
			t.Fatalf("findings = %+v", fs)
		}
	}

	t.Run("no session file", func(t *testing.T) {
		unlinked(t, sec006Rule().Run(context.Background(), &Input{Config: cfg(), DataDir: t.TempDir()}))
	})
	t.Run("a channel named whatsapp needs no name in the fix", func(t *testing.T) {
		c := minimalConfig()
		c.Channels["whatsapp"] = config.Channel{Name: "whatsapp", Type: "whatsapp", Agent: "main"}
		fs := sec006Rule().Run(context.Background(), &Input{Config: c, DataDir: t.TempDir()})
		if len(fs) != 1 || fs[0].Fix != "run `holaclaw login whatsapp --phone <number>`" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("empty file the daemon created", func(t *testing.T) {
		dir := t.TempDir()
		if err := writeFileErr(dir, "whatsapp/wa.db", ""); err != nil {
			t.Fatal(err)
		}
		unlinked(t, sec006Rule().Run(context.Background(), &Input{Config: cfg(), DataDir: dir}))
	})
	t.Run("store with no device", func(t *testing.T) {
		dir := t.TempDir()
		session(t, dir, 0)
		unlinked(t, sec006Rule().Run(context.Background(), &Input{Config: cfg(), DataDir: dir}))
	})
	t.Run("linked", func(t *testing.T) {
		dir := t.TempDir()
		session(t, dir, 1)
		if fs := sec006Rule().Run(context.Background(), &Input{Config: cfg(), DataDir: dir}); len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("unreadable file is not a guess", func(t *testing.T) {
		dir := t.TempDir()
		if err := writeFileErr(dir, "whatsapp/wa.db", "not a database, and long enough to have a header to read"); err != nil {
			t.Fatal(err)
		}
		if fs := sec006Rule().Run(context.Background(), &Input{Config: cfg(), DataDir: dir}); len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("a name that is not a file name is skipped", func(t *testing.T) {
		dir := t.TempDir()
		c := cfg()
		delete(c.Channels, "wa")
		for _, n := range []string{"../wa", "sub/wa", "..", "."} {
			c.Channels[n] = config.Channel{Name: n, Type: "whatsapp", Agent: "main"}
		}
		if fs := sec006Rule().Run(context.Background(), &Input{Config: c, DataDir: dir}); len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("no config", func(t *testing.T) {
		if fs := sec006Rule().Run(context.Background(), &Input{DataDir: t.TempDir()}); len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}

// TestSEC006WAL: the daemon keeps the session in WAL mode. The rule reads it
// while the daemon runs, after it closed, and from a rollback-journal file
// of an older version, and never creates or removes a file doing so.
func TestSEC006WAL(t *testing.T) {
	ctx := context.Background()
	// names lists the session directory.
	names := func(t *testing.T, dir string) []string {
		t.Helper()
		es, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range es {
			out = append(out, e.Name())
		}
		return out
	}
	// daemon opens the session as the daemon does, WAL and all.
	daemon := func(t *testing.T, path string) *sql.DB {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		// keep in sync with sessionDSN in internal/channel/whatsapp/store.go
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		db.SetMaxOpenConns(1) // one connection, so it stays open between calls
		if _, err := db.ExecContext(ctx, `CREATE TABLE whatsmeow_device (jid TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		return db
	}
	link := func(t *testing.T, db *sql.DB) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO whatsmeow_device (jid) VALUES ('34600000000@s.whatsapp.net')`); err != nil {
			t.Fatal(err)
		}
	}
	// sizes maps each file in dir to its size, so a check can tell a
	// same-named file was not rewritten underneath it.
	sizes := func(t *testing.T, dir string) map[string]int64 {
		t.Helper()
		es, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, e := range es {
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			out[e.Name()] = info.Size()
		}
		return out
	}
	check := func(t *testing.T, path string, wantLinked, wantKnown bool) {
		t.Helper()
		before, beforeSizes := names(t, filepath.Dir(path)), sizes(t, filepath.Dir(path))
		linked, known := whatsappLinked(ctx, path)
		if linked != wantLinked || known != wantKnown {
			t.Fatalf("linked, known = %v, %v, want %v, %v", linked, known, wantLinked, wantKnown)
		}
		if after := names(t, filepath.Dir(path)); !slices.Equal(before, after) {
			t.Fatalf("files %v became %v", before, after)
		}
		if after := sizes(t, filepath.Dir(path)); !maps.Equal(beforeSizes, after) {
			t.Fatalf("file sizes %v became %v", beforeSizes, after)
		}
	}

	t.Run("daemon running", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "whatsapp", "wa.db")
		db := daemon(t, path)
		if got := names(t, filepath.Dir(path)); !slices.Equal(got, []string{"wa.db", "wa.db-shm", "wa.db-wal"}) {
			t.Fatalf("files = %v, want the database and its WAL", got)
		}
		check(t, path, false, true)
		link(t, db) // in the -wal only, not yet checkpointed
		check(t, path, true, true)
	})
	t.Run("crashed daemon", func(t *testing.T) {
		// The daemon connection stays open (as after a crash, nobody ran
		// clean shutdown) while a copy of the three files is checked:
		// nobody holds the copy, so the rule must read it as a closed,
		// checkpointed database would, and touch none of its files.
		src := filepath.Join(t.TempDir(), "wa.db")
		db := daemon(t, src)
		link(t, db)
		path := filepath.Join(t.TempDir(), "whatsapp", "wa.db")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"", "-wal", "-shm"} {
			b, err := os.ReadFile(src + s)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+s, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		check(t, path, true, true)
	})
	t.Run("daemon closed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "whatsapp", "wa.db")
		db := daemon(t, path)
		link(t, db)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if got := names(t, filepath.Dir(path)); !slices.Equal(got, []string{"wa.db"}) {
			t.Fatalf("files = %v, want the database alone", got)
		}
		check(t, path, true, true)
	})
	t.Run("rollback journal", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "whatsapp", "wa.db")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, `CREATE TABLE whatsmeow_device (jid TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		link(t, db)
		check(t, path, true, true)
	})
	t.Run("a -wal without its -shm is not a guess", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "wa.db")
		db := daemon(t, src)
		link(t, db)
		path := filepath.Join(t.TempDir(), "whatsapp", "wa.db")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"", "-wal"} {
			b, err := os.ReadFile(src + s)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+s, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		check(t, path, false, false)
	})
	t.Run("no file is created", func(t *testing.T) {
		dir := t.TempDir()
		if fs := sec006Rule().Run(ctx, &Input{Config: func() *config.Config {
			c := minimalConfig()
			c.Channels["wa"] = config.Channel{Name: "wa", Type: "whatsapp", Agent: "main"}
			return c
		}(), DataDir: dir}); len(fs) != 1 {
			t.Fatalf("findings = %v, want one", fs)
		}
		if got := names(t, dir); len(got) != 0 {
			t.Fatalf("files = %v, want none", got)
		}
	})
}
