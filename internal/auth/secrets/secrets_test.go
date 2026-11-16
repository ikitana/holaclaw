package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "auth.toml")
}

func open(t *testing.T, path string) *File {
	t.Helper()
	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

func mustSet(t *testing.T, f *File, section Section, name string, kv map[string]string) {
	t.Helper()
	if err := f.Set(section, name, kv); err != nil {
		t.Fatalf("Set %s.%s: %v", section, name, err)
	}
}

func mustGet(t *testing.T, f *File, section Section, name, key string) string {
	t.Helper()
	v, ok := f.Get(section, name, key)
	if !ok {
		t.Fatalf("Get %s.%s.%s: missing", section, name, key)
	}
	return v
}

func TestOpenMissingFileIsEmpty(t *testing.T) {
	f := open(t, tempPath(t))
	if got := f.Names(Providers); len(got) != 0 {
		t.Fatalf("Names(providers) = %v, want empty", got)
	}
	if got := f.Names(Channels); len(got) != 0 {
		t.Fatalf("Names(channels) = %v, want empty", got)
	}
	if _, ok := f.Get(Providers, "openrouter", "api_key"); ok {
		t.Fatal("Get on empty set reported a value")
	}
	if _, err := os.Stat(f.Path()); !os.IsNotExist(err) {
		t.Fatal("Open created the file")
	}
}

func TestRoundTrip(t *testing.T) {
	path := tempPath(t)
	f := open(t, path)
	mustSet(t, f, Providers, "openrouter", map[string]string{"api_key": "sk-or-v1-abcdef"})
	mustSet(t, f, Providers, "codex", map[string]string{
		"access_token":  "access-aaaaaaaa",
		"refresh_token": "refresh-bbbbbbb",
		"expires_at":    "2026-09-18T10:00:00Z",
	})
	mustSet(t, f, Channels, "tg-main", map[string]string{"bot_token": "12345:telegram-token"})

	reopened := open(t, path)
	for _, f := range []*File{f, reopened} {
		if got := mustGet(t, f, Providers, "openrouter", "api_key"); got != "sk-or-v1-abcdef" {
			t.Fatalf("api_key = %q", got)
		}
		if got := mustGet(t, f, Channels, "tg-main", "bot_token"); got != "12345:telegram-token" {
			t.Fatalf("bot_token = %q", got)
		}
		if got := strings.Join(f.Names(Providers), ","); got != "codex,openrouter" {
			t.Fatalf("Names(providers) = %q", got)
		}
		if got := strings.Join(f.Keys(Providers, "codex"), ","); got != "access_token,expires_at,refresh_token" {
			t.Fatalf("Keys(codex) = %q", got)
		}
		if got := f.Keys(Providers, "nope"); len(got) != 0 {
			t.Fatalf("Keys of unknown entry = %v", got)
		}
	}
}

func TestSetChangesOnlyItsEntry(t *testing.T) {
	path := tempPath(t)
	f := open(t, path)
	mustSet(t, f, Providers, "openrouter", map[string]string{"api_key": "keep-this-one"})
	mustSet(t, f, Channels, "tg-main", map[string]string{"bot_token": "keep-this-too"})
	mustSet(t, f, Providers, "codex", map[string]string{
		"access_token":  "old-access-token",
		"refresh_token": "old-refresh-token",
	})

	// Rotate one key of one entry.
	mustSet(t, f, Providers, "codex", map[string]string{"access_token": "new-access-token"})

	after := open(t, path)
	if got := mustGet(t, after, Providers, "codex", "access_token"); got != "new-access-token" {
		t.Fatalf("access_token = %q", got)
	}
	if got := mustGet(t, after, Providers, "codex", "refresh_token"); got != "old-refresh-token" {
		t.Fatalf("untouched key of the same entry changed: %q", got)
	}
	if got := mustGet(t, after, Providers, "openrouter", "api_key"); got != "keep-this-one" {
		t.Fatalf("other provider changed: %q", got)
	}
	if got := mustGet(t, after, Channels, "tg-main", "bot_token"); got != "keep-this-too" {
		t.Fatalf("other section changed: %q", got)
	}
}

func TestSetReReadsBeforeWriting(t *testing.T) {
	path := tempPath(t)
	a := open(t, path)
	b := open(t, path) // opened before any write: its memory is stale on purpose

	mustSet(t, a, Providers, "openrouter", map[string]string{"api_key": "value-from-a"})
	mustSet(t, b, Channels, "tg-main", map[string]string{"bot_token": "value-from-b"})

	after := open(t, path)
	if got := mustGet(t, after, Providers, "openrouter", "api_key"); got != "value-from-a" {
		t.Fatalf("A's entry lost: %q", got)
	}
	if got := mustGet(t, after, Channels, "tg-main", "bot_token"); got != "value-from-b" {
		t.Fatalf("B's entry lost: %q", got)
	}
	// The writer's own copy is refreshed with what it re-read.
	if got := mustGet(t, b, Providers, "openrouter", "api_key"); got != "value-from-a" {
		t.Fatalf("B did not pick up A's entry: %q", got)
	}
	// A is still stale until it reloads.
	if _, ok := a.Get(Channels, "tg-main", "bot_token"); ok {
		t.Fatal("A saw B's entry without reloading")
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := mustGet(t, a, Channels, "tg-main", "bot_token"); got != "value-from-b" {
		t.Fatalf("Reload did not pick up B's entry: %q", got)
	}
}

func TestConcurrentWritersLoseNothing(t *testing.T) {
	path := tempPath(t)
	const handles, writers = 5, 50

	hs := make([]*File, handles)
	for i := range hs {
		hs[i] = open(t, path)
	}
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := hs[i%handles]
			name := fmt.Sprintf("p%02d", i)
			if err := h.Set(Providers, name, map[string]string{"api_key": "secret-" + name}); err != nil {
				t.Errorf("Set %s: %v", name, err)
			}
		}()
	}
	wg.Wait()

	after := open(t, path)
	if got := len(after.Names(Providers)); got != writers {
		t.Fatalf("entries after %d concurrent writes = %d", writers, got)
	}
	for i := range writers {
		name := fmt.Sprintf("p%02d", i)
		if got := mustGet(t, after, Providers, name, "api_key"); got != "secret-"+name {
			t.Fatalf("%s api_key = %q", name, got)
		}
	}
}

func TestWriteAlwaysEndsAt0600(t *testing.T) {
	path := tempPath(t)
	f := open(t, path)
	mustSet(t, f, Providers, "openrouter", map[string]string{"api_key": "first-value-x"})
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	mustSet(t, f, Providers, "openrouter", map[string]string{"api_key": "second-value-x"})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode after write = %o, want 600", got)
	}
	if err := CheckPerms(path); err != nil {
		t.Fatalf("CheckPerms after write: %v", err)
	}
}

func TestCheckPerms(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("version = 1\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
		return p
	}
	link := filepath.Join(dir, "link.toml")
	if err := os.Symlink(write("target.toml", 0o600), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	subdir := filepath.Join(dir, "adir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	tests := []struct {
		name string
		path string
		want error
	}{
		{"missing", filepath.Join(dir, "absent.toml"), nil},
		{"0600", write("ok.toml", 0o600), nil},
		{"0400", write("ro.toml", 0o400), nil},
		{"0640", write("group.toml", 0o640), ErrBadMode},
		{"0604", write("world.toml", 0o604), ErrBadMode},
		{"0620", write("groupwrite.toml", 0o620), ErrBadMode},
		{"symlink", link, ErrNotRegular},
		{"directory", subdir, ErrNotRegular},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPerms(tc.path)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("CheckPerms = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("CheckPerms = %v, want %v", err, tc.want)
			}
			var pe *PermError
			if !errors.As(err, &pe) {
				t.Fatalf("error %v is not a *PermError", err)
			}
		})
	}
}

func TestOpenToleratesBadPerms(t *testing.T) {
	path := tempPath(t)
	if err := os.WriteFile(path, []byte("version = 1\n[providers.a]\napi_key = \"value-here\"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := Open(path) // check reports perms; Open must still work
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := mustGet(t, f, Providers, "a", "api_key"); got != "value-here" {
		t.Fatalf("api_key = %q", got)
	}
	if err := CheckPerms(path); !errors.Is(err, ErrBadMode) {
		t.Fatalf("CheckPerms = %v, want ErrBadMode", err)
	}
}

func TestSetRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.toml")
	if err := os.WriteFile(target, []byte("version = 1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	path := filepath.Join(dir, "auth.toml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	f := open(t, path)
	err := f.Set(Providers, "openrouter", map[string]string{"api_key": "must-not-land"})
	if !errors.Is(err, ErrNotRegular) {
		t.Fatalf("Set through symlink = %v, want ErrNotRegular", err)
	}
	b, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read target: %v", readErr)
	}
	if strings.Contains(string(b), "must-not-land") {
		t.Fatal("refused write reached the symlink target")
	}
}

func TestNoTempFileLeftAfterFailedWrite(t *testing.T) {
	path := tempPath(t)
	f := open(t, path)
	mustSet(t, f, Providers, "openrouter", map[string]string{"api_key": "before-failure"})

	orig := renameFile
	renameFile = func(string, string) error { return errors.New("rename refused") }
	t.Cleanup(func() { renameFile = orig })

	err := f.Set(Providers, "codex", map[string]string{"access_token": "never-written"})
	if err == nil {
		t.Fatal("Set succeeded although rename failed")
	}
	renameFile = orig

	entries, readErr := os.ReadDir(filepath.Dir(path))
	if readErr != nil {
		t.Fatalf("readdir: %v", readErr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".auth-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	// The file on disk is untouched and the failed write is not in memory.
	after := open(t, path)
	if _, ok := after.Get(Providers, "codex", "access_token"); ok {
		t.Fatal("failed write reached the file")
	}
	if got := mustGet(t, after, Providers, "openrouter", "api_key"); got != "before-failure" {
		t.Fatalf("api_key = %q", got)
	}
}

func TestDelete(t *testing.T) {
	path := tempPath(t)
	f := open(t, path)
	mustSet(t, f, Providers, "codex", map[string]string{
		"access_token":  "access-value-1",
		"refresh_token": "refresh-value-1",
		"expires_at":    "2026-09-18T10:00:00Z",
	})
	mustSet(t, f, Providers, "openrouter", map[string]string{"api_key": "keep-me-here"})

	// One key.
	if err := f.Delete(Providers, "codex", "expires_at"); err != nil {
		t.Fatalf("Delete key: %v", err)
	}
	if _, ok := f.Get(Providers, "codex", "expires_at"); ok {
		t.Fatal("deleted key still present")
	}
	if got := strings.Join(f.Keys(Providers, "codex"), ","); got != "access_token,refresh_token" {
		t.Fatalf("Keys after delete = %q", got)
	}
	// Several keys at once, which empties and drops the entry.
	if err := f.Delete(Providers, "codex", "access_token", "refresh_token"); err != nil {
		t.Fatalf("Delete keys: %v", err)
	}
	if got := strings.Join(f.Names(Providers), ","); got != "openrouter" {
		t.Fatalf("Names = %q", got)
	}
	// Unknown key and unknown entry are no-ops.
	if err := f.Delete(Providers, "openrouter", "nope"); err != nil {
		t.Fatalf("Delete unknown key: %v", err)
	}
	if err := f.Delete(Channels, "absent"); err != nil {
		t.Fatalf("Delete unknown entry: %v", err)
	}
	// Whole entry.
	if err := f.Delete(Providers, "openrouter"); err != nil {
		t.Fatalf("Delete entry: %v", err)
	}
	after := open(t, path)
	if got := len(after.Names(Providers)); got != 0 {
		t.Fatalf("entries left = %d", got)
	}
	if err := f.Delete(Providers, ""); !errors.Is(err, ErrBadName) {
		t.Fatalf("Delete empty name = %v, want ErrBadName", err)
	}
}

func TestLookupSeesRotation(t *testing.T) {
	path := tempPath(t)
	f := open(t, path)
	mustSet(t, f, Providers, "codex", map[string]string{"access_token": "access-token-1"})

	token := f.Lookup(Providers, "codex", "access_token")
	if got := token(); got != "access-token-1" {
		t.Fatalf("token() = %q", got)
	}
	mustSet(t, f, Providers, "codex", map[string]string{"access_token": "access-token-2"})
	if got := token(); got != "access-token-2" {
		t.Fatalf("token() after rotation = %q", got)
	}
	// A rotation by another process is picked up on Reload.
	other := open(t, path)
	mustSet(t, other, Providers, "codex", map[string]string{"access_token": "access-token-3"})
	if err := f.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := token(); got != "access-token-3" {
		t.Fatalf("token() after external rotation = %q", got)
	}
	// A missing key reads as empty rather than panicking.
	if got := f.Lookup(Providers, "codex", "absent")(); got != "" {
		t.Fatalf("missing key = %q", got)
	}
}

func TestRedactor(t *testing.T) {
	path := tempPath(t)
	f := open(t, path)
	mustSet(t, f, Providers, "openrouter", map[string]string{"api_key": "sk-or-v1-longsecret"})
	redact := f.Redactor()

	line := "GET /v1/chat with key sk-or-v1-longsecret ok"
	if got := redact(line); strings.Contains(got, "sk-or-v1-longsecret") {
		t.Fatalf("redactor left the value: %q", got)
	} else if !strings.Contains(got, "[redacted]") {
		t.Fatalf("redactor output = %q", got)
	}

	// Values set after the redactor was built are redacted too.
	mustSet(t, f, Channels, "tg-main", map[string]string{"bot_token": "12345:telegram-bot-token"})
	if got := redact("bot 12345:telegram-bot-token connected"); strings.Contains(got, "telegram-bot-token") {
		t.Fatalf("redactor missed a later value: %q", got)
	}

	// Short values are left alone: they would match ordinary prose.
	mustSet(t, f, Providers, "tiny", map[string]string{"api_key": "abc"})
	if got := redact("abc is not a secret"); got != "abc is not a secret" {
		t.Fatalf("short value redacted: %q", got)
	}
	// A value containing another is redacted whole.
	mustSet(t, f, Providers, "nested", map[string]string{"api_key": "sk-or-v1-longsecret-extra"})
	if got := redact("key sk-or-v1-longsecret-extra end"); got != "key [redacted] end" {
		t.Fatalf("overlapping values = %q", got)
	}
}

func TestErrorsNeverLeakValues(t *testing.T) {
	const secret = "sk-or-v1-supersecretvalue"
	dir := t.TempDir()

	cases := []struct {
		name    string
		content string
		want    error
	}{
		{
			name:    "malformed",
			content: "version = 1\n[providers.a]\napi_key = \"" + secret + "\n",
			want:    ErrMalformed,
		},
		{
			name:    "unknown top-level key",
			content: "version = 1\nsomething = \"" + secret + "\"\n",
			want:    ErrUnknownKey,
		},
		{
			name:    "non-string value",
			content: "version = 1\n[providers.a]\napi_key = 12345678\n",
			want:    ErrNotString,
		},
		{
			name:    "section is not a table",
			content: "version = 1\nproviders = \"" + secret + "\"\n",
			want:    ErrNotString,
		},
		{
			name:    "entry is not a table",
			content: "version = 1\n[providers]\na = \"" + secret + "\"\n",
			want:    ErrNotString,
		},
		{
			name:    "missing version",
			content: "[providers.a]\napi_key = \"" + secret + "\"\n",
			want:    ErrBadVersion,
		},
		{
			name:    "wrong version",
			content: "version = 7\n[providers.a]\napi_key = \"" + secret + "\"\n",
			want:    ErrBadVersion,
		},
		{
			name:    "version not an integer",
			content: "version = \"one\"\n[providers.a]\napi_key = \"" + secret + "\"\n",
			want:    ErrBadVersion,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("auth%d.toml", i))
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := Open(path)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open = %v, want %v", err, tc.want)
			}
			var le *LoadError
			if !errors.As(err, &le) {
				t.Fatalf("error %v is not a *LoadError", err)
			}
			msg := err.Error()
			if strings.Contains(msg, secret) {
				t.Fatalf("error leaked the value: %q", msg)
			}
			if strings.Contains(msg, "12345678") {
				t.Fatalf("error leaked the value: %q", msg)
			}
			if tc.want != ErrMalformed && le.Key == "" {
				t.Fatalf("error does not name the key path: %q", msg)
			}
		})
	}
}

func TestSetRejectsBadNames(t *testing.T) {
	f := open(t, tempPath(t))
	if err := f.Set(Providers, "", map[string]string{"api_key": "v"}); !errors.Is(err, ErrBadName) {
		t.Fatalf("empty name = %v, want ErrBadName", err)
	}
	if err := f.Set(Providers, "a", map[string]string{"bad\nkey": "v"}); !errors.Is(err, ErrBadName) {
		t.Fatalf("control character in key = %v, want ErrBadName", err)
	}
	if err := f.Set("agents", "a", map[string]string{"k": "v"}); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown section = %v, want ErrUnknownKey", err)
	}
}

func TestWriteCreatesParentDirAndHeader(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "conf", "auth.toml")
	f := open(t, path)
	mustSet(t, f, Providers, "openrouter", map[string]string{"api_key": "value-goes-here"})

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent dir mode = %o, want 700", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(b)
	if !strings.HasPrefix(text, "# ") || !strings.Contains(text, "not preserved") {
		t.Fatalf("header comment missing:\n%s", text)
	}
	if !strings.Contains(text, fmt.Sprintf("version = %d", Version)) {
		t.Fatalf("version line missing:\n%s", text)
	}
	if !strings.Contains(text, "[providers.openrouter]") {
		t.Fatalf("section header missing:\n%s", text)
	}

	// Comments added by hand do not survive a rewrite; the entries do.
	if err := os.WriteFile(path, append([]byte("# hand written\n"), b...), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustSet(t, f, Channels, "tg-main", map[string]string{"bot_token": "token-value-1"})
	b2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(b2), "hand written") {
		t.Fatal("machine file preserved a comment")
	}
	if !strings.Contains(string(b2), "value-goes-here") {
		t.Fatal("rewrite dropped an existing entry")
	}
}

func TestValuesWithSpecialCharactersRoundTrip(t *testing.T) {
	path := tempPath(t)
	f := open(t, path)
	value := "quote\" backslash\\ tab\t newline\n end"
	mustSet(t, f, Providers, "odd.name", map[string]string{"api key": value})

	after := open(t, path)
	if got := mustGet(t, after, Providers, "odd.name", "api key"); got != value {
		t.Fatalf("value = %q, want %q", got, value)
	}
}

func TestConnectorsSection(t *testing.T) {
	path := tempPath(t)
	f := open(t, path)
	mustSet(t, f, Connectors, "github-admin", map[string]string{
		ConnectorAccessToken:  "gho-access-aaaa",
		ConnectorRefreshToken: "ghr-refresh-bbbb",
		ConnectorClientSecret: "client-secret-cc",
	})
	mustSet(t, f, Providers, "openrouter", map[string]string{"api_key": "sk-or-v1-abcdef"})

	reopened := open(t, path)
	if got := strings.Join(reopened.Keys(Connectors, "github-admin"), ","); got != "access_token,client_secret,refresh_token" {
		t.Fatalf("Keys(github-admin) = %q", got)
	}
	if got := mustGet(t, reopened, Connectors, "github-admin", ConnectorRefreshToken); got != "ghr-refresh-bbbb" {
		t.Fatalf("refresh_token = %q", got)
	}
	if got := reopened.Redactor()("token gho-access-aaaa leaked"); strings.Contains(got, "gho-access-aaaa") {
		t.Fatalf("connector token not redacted: %q", got)
	}

	// Sections are written in their declared order, connectors last.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(b)
	if p, c := strings.Index(text, "[providers.openrouter]"), strings.Index(text, "[connectors.github-admin]"); p < 0 || c < p {
		t.Fatalf("section order wrong:\n%s", text)
	}

	if err := f.Delete(Connectors, "github-admin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := open(t, path).Names(Connectors); len(got) != 0 {
		t.Fatalf("Names(connectors) after delete = %v", got)
	}
}

func TestSectionsIsACopy(t *testing.T) {
	s := Sections()
	s[0] = "agents"
	if got := Sections(); got[0] != Providers || len(got) != 3 || got[2] != Connectors {
		t.Fatalf("Sections() = %v after a caller changed its copy", got)
	}
	if err := open(t, tempPath(t)).Set("agents", "a", map[string]string{"k": "v"}); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Set on a section a caller invented = %v, want ErrUnknownKey", err)
	}
}

func TestConnectorsSectionValidated(t *testing.T) {
	path := tempPath(t)
	content := "version = 1\n[connectors.homes]\naccess_token = 42\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Open(path)
	var le *LoadError
	if !errors.Is(err, ErrNotString) || !errors.As(err, &le) || le.Key != "connectors.homes.access_token" {
		t.Fatalf("Open = %v, want ErrNotString on connectors.homes.access_token", err)
	}
}
