package app

import (
	"os"
	"path/filepath"
	"testing"
)

// machine stands in for a machine: running as uid, with or without a system
// install in /etc/holaclaw.
func machine(t *testing.T, uid int, system bool) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "etc-holaclaw")
	if system {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldDir, oldEuid := systemConfigDir, euid
	systemConfigDir, euid = dir, func() int { return uid }
	t.Cleanup(func() { systemConfigDir, euid = oldDir, oldEuid })
}

func TestDefaultConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/home/u/.cfg")
	for _, tc := range []struct {
		name   string
		uid    int
		system bool
		want   string
	}{
		{"root", 0, false, "/etc/holaclaw"},
		{"a system install", 1000, true, "/etc/holaclaw"},
		{"own user", 1000, false, "/home/u/.cfg/holaclaw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine(t, tc.uid, tc.system)
			t.Setenv("HOLACLAW_CONFIG", "")
			if got := DefaultConfigDir(); got != tc.want {
				t.Errorf("default = %q, want %q", got, tc.want)
			}
			t.Setenv("HOLACLAW_CONFIG", "/srv/hb")
			if got := DefaultConfigDir(); got != "/srv/hb" {
				t.Errorf("with $HOLACLAW_CONFIG = %q, want /srv/hb", got)
			}
		})
	}
}

func TestDefaultConfigDirHome(t *testing.T) {
	machine(t, 1000, false)
	t.Setenv("HOLACLAW_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got := DefaultConfigDir(); got != "/home/u/.config/holaclaw" {
		t.Errorf("default = %q, want ~/.config/holaclaw", got)
	}
	// With no home to go by, the system default.
	t.Setenv("HOME", "")
	if got := DefaultConfigDir(); got != "/etc/holaclaw" {
		t.Errorf("with no home = %q, want /etc/holaclaw", got)
	}
}

func TestDefaultSocket(t *testing.T) {
	for _, tc := range []struct {
		name   string
		uid    int
		system bool
		xdg    string
		want   string
	}{
		{"root", 0, false, "/run/user/0", "/run/holaclaw/holaclaw.sock"},
		{"a system install", 1000, true, "/run/user/1000", "/run/holaclaw/holaclaw.sock"},
		{"own user", 1000, false, "/run/user/1000", "/run/user/1000/holaclaw.sock"},
		{"own user, no runtime dir", 1000, false, "", "/run/holaclaw/holaclaw.sock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine(t, tc.uid, tc.system)
			t.Setenv("XDG_RUNTIME_DIR", tc.xdg)
			if got := DefaultSocket(); got != tc.want {
				t.Errorf("DefaultSocket() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSecretsPath(t *testing.T) {
	for _, tc := range []struct{ configured, want string }{
		{"", "/etc/holaclaw-secrets/auth.toml"},
		{"/run/keys", "/run/keys/auth.toml"},
	} {
		if got := SecretsPath(tc.configured); got != filepath.FromSlash(tc.want) {
			t.Errorf("SecretsPath(%q) = %q, want %q", tc.configured, got, tc.want)
		}
	}
}
