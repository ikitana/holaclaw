package check

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/config"
)

// browserConfig is minimalConfig with a [browser] table, which is what turns
// every BRW rule on.
func browserConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := minimalConfig()
	cfg.Browser = config.Browser{Enabled: true, Socket: "/nonexistent/holaclaw-browser.sock"}
	return cfg
}

// fakeBin writes an executable shell script named name under dir.
func fakeBin(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// onlyPath points PATH at a directory of our own, so the rules see exactly
// the binaries a test put there and nothing the dev shell provides.
func onlyPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	return dir
}

func TestBRW001(t *testing.T) {
	t.Run("no browser table", func(t *testing.T) {
		onlyPath(t)
		fs := brw001Rule().Run(context.Background(), &Input{Config: minimalConfig()})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("found on PATH", func(t *testing.T) {
		dir := onlyPath(t)
		fakeBin(t, dir, "chromium", `echo "Chromium 145.0.7632.1"`)
		fs := brw001Rule().Run(context.Background(), &Input{Config: browserConfig(t)})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("nothing on PATH", func(t *testing.T) {
		onlyPath(t)
		fs := brw001Rule().Run(context.Background(), &Input{Config: browserConfig(t)})
		if len(fs) != 1 || fs[0].ID != "BRW001" || fs[0].Severity != SeverityError {
			t.Fatalf("findings = %+v", fs)
		}
		if fs[0].Fix == "" {
			t.Fatal("an error with no fix")
		}
	})
	t.Run("too old", func(t *testing.T) {
		dir := onlyPath(t)
		fakeBin(t, dir, "chromium", `echo "Chromium 120.0.1.1"`)
		fs := brw001Rule().Run(context.Background(), &Input{Config: browserConfig(t)})
		if len(fs) != 1 || fs[0].ID != "BRW001" {
			t.Fatalf("findings = %+v", fs)
		}
		if !containsAll(fs[0].Message, "120.0.1.1", "136") {
			t.Fatalf("message = %q, want the version and the minimum major", fs[0].Message)
		}
	})
	t.Run("configured binary missing", func(t *testing.T) {
		onlyPath(t)
		cfg := browserConfig(t)
		cfg.Browser.Binary = filepath.Join(t.TempDir(), "nope")
		fs := brw001Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "BRW001" || fs[0].Key != "browser.binary" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("configured binary not executable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root ignores permission bits")
		}
		onlyPath(t)
		p := filepath.Join(t.TempDir(), "chromium")
		if err := os.WriteFile(p, []byte("not a program"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := browserConfig(t)
		cfg.Browser.Binary = p
		fs := brw001Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].Severity != SeverityError {
			t.Fatalf("findings = %+v", fs)
		}
	})
}

func TestBRW002(t *testing.T) {
	t.Run("xvfb present", func(t *testing.T) {
		dir := onlyPath(t)
		fakeBin(t, dir, "Xvfb", `exit 0`)
		fs := brw002Rule().Run(context.Background(), &Input{Config: browserConfig(t)})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("xvfb missing", func(t *testing.T) {
		onlyPath(t)
		fs := brw002Rule().Run(context.Background(), &Input{Config: browserConfig(t)})
		if len(fs) != 1 || fs[0].ID != "BRW002" || fs[0].Severity != SeverityError {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("headless needs no display", func(t *testing.T) {
		onlyPath(t)
		cfg := browserConfig(t)
		cfg.Browser.Headless = true
		fs := brw002Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("no browser table", func(t *testing.T) {
		onlyPath(t)
		fs := brw002Rule().Run(context.Background(), &Input{Config: minimalConfig()})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
}

func TestBRW003(t *testing.T) {
	// fakeProc writes the named files under a temporary /proc and points the
	// rule at it.
	fakeProc := func(t *testing.T, files map[string]string) {
		t.Helper()
		root := t.TempDir()
		for rel, content := range files {
			full := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		old := procRoot
		procRoot = root
		t.Cleanup(func() { procRoot = old })
	}

	t.Run("nothing to read", func(t *testing.T) {
		fakeProc(t, nil)
		fs := brw003Rule().Run(context.Background(), &Input{Config: browserConfig(t)})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("healthy host", func(t *testing.T) {
		fakeProc(t, map[string]string{
			"sys/kernel/unprivileged_userns_clone":             "1\n",
			"sys/user/max_user_namespaces":                     "15000\n",
			"sys/kernel/apparmor_restrict_unprivileged_userns": "0\n",
		})
		fs := brw003Rule().Run(context.Background(), &Input{Config: browserConfig(t)})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("userns disabled", func(t *testing.T) {
		fakeProc(t, map[string]string{"sys/kernel/unprivileged_userns_clone": "0\n"})
		fs := brw003Rule().Run(context.Background(), &Input{Config: browserConfig(t)})
		if len(fs) != 1 || fs[0].ID != "BRW003" || fs[0].Severity != SeverityError {
			t.Fatalf("findings = %+v", fs)
		}
		if !containsAll(fs[0].Fix, "sysctl", "unprivileged_userns_clone=1") {
			t.Fatalf("fix = %q", fs[0].Fix)
		}
	})
	t.Run("every switch wrong", func(t *testing.T) {
		fakeProc(t, map[string]string{
			"sys/kernel/unprivileged_userns_clone":             "0\n",
			"sys/user/max_user_namespaces":                     "0\n",
			"sys/kernel/apparmor_restrict_unprivileged_userns": "1\n",
		})
		fs := brw003Rule().Run(context.Background(), &Input{Config: browserConfig(t)})
		if len(fs) != 3 {
			t.Fatalf("findings = %+v, want three", fs)
		}
		for _, f := range fs {
			if f.Fix == "" {
				t.Fatalf("no fix on %+v", f)
			}
		}
	})
	t.Run("no browser table", func(t *testing.T) {
		fakeProc(t, map[string]string{"sys/user/max_user_namespaces": "0\n"})
		fs := brw003Rule().Run(context.Background(), &Input{Config: minimalConfig()})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
}

func TestBRW004(t *testing.T) {
	// socketPath stays well inside the ~107-byte unix socket limit.
	socketPath := func(t *testing.T) string {
		t.Helper()
		d, err := os.MkdirTemp("", "hb")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		return filepath.Join(d, "browser.sock")
	}

	t.Run("reachable", func(t *testing.T) {
		p := socketPath(t)
		ln, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		srv.Listener = ln
		srv.Start()
		defer srv.Close()

		cfg := browserConfig(t)
		cfg.Browser.Socket = p
		fs := brw004Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("not listening", func(t *testing.T) {
		cfg := browserConfig(t)
		cfg.Browser.Socket = socketPath(t)
		fs := brw004Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "BRW004" || fs[0].Severity != SeverityWarning {
			t.Fatalf("findings = %+v", fs)
		}
		if fs[0].Fix != "systemctl start holaclaw-browser" {
			t.Fatalf("fix = %q", fs[0].Fix)
		}
	})
	t.Run("offline skips the dial", func(t *testing.T) {
		cfg := browserConfig(t)
		cfg.Browser.Socket = socketPath(t)
		fs := brw004Rule().Run(context.Background(), &Input{Config: cfg, Offline: true})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("no socket configured", func(t *testing.T) {
		cfg := browserConfig(t)
		cfg.Browser.Socket = ""
		fs := brw004Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
}

func TestBRW005(t *testing.T) {
	withTools := func(t *testing.T, tools []string, allow []string) *Input {
		t.Helper()
		cfg := browserConfig(t)
		a := cfg.Agents["main"]
		a.Tools = tools
		a.Browser = config.AgentBrowser{Allow: allow}
		cfg.Agents["main"] = a
		return &Input{Config: cfg}
	}

	t.Run("no browser tool", func(t *testing.T) {
		fs := brw005Rule().Run(context.Background(), withTools(t, []string{"memory"}, nil))
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("tool without allow-list", func(t *testing.T) {
		fs := brw005Rule().Run(context.Background(), withTools(t, []string{"memory", "browser.do"}, nil))
		if len(fs) != 1 || fs[0].ID != "BRW005" || fs[0].Severity != SeverityWarning {
			t.Fatalf("findings = %+v", fs)
		}
		if fs[0].File != "agents/main/agent.toml" || fs[0].Key != "browser.allow" {
			t.Fatalf("finding points at %s: %s", fs[0].File, fs[0].Key)
		}
	})
	t.Run("group entry counts", func(t *testing.T) {
		fs := brw005Rule().Run(context.Background(), withTools(t, []string{"browser"}, nil))
		if len(fs) != 1 {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("allow-list set", func(t *testing.T) {
		fs := brw005Rule().Run(context.Background(), withTools(t, []string{"browser.do"}, []string{"example.com"}))
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("no browser table", func(t *testing.T) {
		cfg := minimalConfig()
		a := cfg.Agents["main"]
		a.Tools = []string{"browser.do"}
		cfg.Agents["main"] = a
		fs := brw005Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
}

// TestBrowserRulesSkipWithoutConfig proves every BRW rule survives the
// partial input a failed config load hands them.
func TestBrowserRulesSkipWithoutConfig(t *testing.T) {
	onlyPath(t)
	for _, r := range []Rule{brw001Rule(), brw002Rule(), brw003Rule(), brw004Rule(), brw005Rule()} {
		if fs := r.Run(context.Background(), &Input{}); len(fs) != 0 {
			t.Fatalf("%s on a nil config: %+v", r.ID, fs)
		}
	}
}

// containsAll reports whether s holds every substring.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestBRW006(t *testing.T) {
	with := func(t *testing.T, tools []string, decide string) *Input {
		t.Helper()
		cfg := browserConfig(t)
		a := cfg.Agents["main"]
		a.Tools = tools
		a.Models.Decide = decide
		cfg.Agents["main"] = a
		return &Input{Config: cfg}
	}
	t.Run("no browser tool", func(t *testing.T) {
		if fs := brw006Rule().Run(context.Background(), with(t, []string{"memory"}, "")); len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("tool without decide role", func(t *testing.T) {
		fs := brw006Rule().Run(context.Background(), with(t, []string{"browser.do"}, ""))
		if len(fs) != 1 || fs[0].ID != "BRW006" || fs[0].Severity != SeverityWarning || fs[0].Key != "models.decide" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("decide role set", func(t *testing.T) {
		if fs := brw006Rule().Run(context.Background(), with(t, []string{"browser.do"}, "jev")); len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("no browser table", func(t *testing.T) {
		cfg := minimalConfig()
		a := cfg.Agents["main"]
		a.Tools = []string{"browser.do"}
		cfg.Agents["main"] = a
		if fs := brw006Rule().Run(context.Background(), &Input{Config: cfg}); len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
}
