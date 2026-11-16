package check

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // the WhatsApp session store, read with no CGO

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
)

// sec001Rule: auth.toml must be a regular file readable by no one but its
// owner (ARCHITECTURE.md §4).
func sec001Rule() Rule {
	return Rule{
		ID:      "SEC001",
		Summary: "auth.toml is group/world accessible or not a regular file",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.SecretsPath == "" {
				return nil
			}
			if err := secrets.CheckPerms(in.SecretsPath); err != nil {
				return []Finding{{
					ID:       "SEC001",
					Severity: SeverityError,
					File:     relFile(in.ConfigDir, in.SecretsPath),
					Message:  err.Error(),
					Fix:      "chmod 600 " + in.SecretsPath,
				}}
			}
			return nil
		},
	}
}

// sec002Rule: a provider configured for api-key auth needs a resolvable
// secret, either an `env:VAR` reference that is set or an auth.toml entry.
func sec002Rule() Rule {
	return Rule{
		ID:      "SEC002",
		Summary: "provider needs api-key auth but no api_key resolves",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Providers) {
				p := in.Config.Providers[name]
				if p.Auth != "api-key" {
					continue
				}
				if envResolves(p.APIKey) {
					continue
				}
				if secretHas(in.Secrets, secrets.Providers, name, "api_key") {
					continue
				}
				out = append(out, Finding{
					ID:       "SEC002",
					Severity: SeverityError,
					File:     config.FileConfig,
					Key:      "providers." + name + ".auth",
					Message:  fmt.Sprintf("provider %q has auth = \"api-key\" but no api_key is set", name),
					Fix:      fmt.Sprintf("run `holaclaw auth set providers.%s api_key <value>`", name),
				})
			}
			return out
		},
	}
}

// sec003Rule: every channel instance needs the bot credential its platform
// requires, in auth.toml.
func sec003Rule() Rule {
	return Rule{
		ID:      "SEC003",
		Summary: "channel instance has no bot token in auth.toml",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Channels) {
				ch := in.Config.Channels[name]
				var missing []string
				switch ch.Type {
				case "telegram":
					if !secretHas(in.Secrets, secrets.Channels, name, "bot_token") {
						missing = append(missing, "bot_token")
					}
				case "slack":
					if !secretHas(in.Secrets, secrets.Channels, name, "bot_token") {
						missing = append(missing, "bot_token")
					}
					if !secretHas(in.Secrets, secrets.Channels, name, "app_token") {
						missing = append(missing, "app_token")
					}
				case "googlechat":
					if !secretHas(in.Secrets, secrets.Channels, name, "service_account") {
						missing = append(missing, "service_account")
					}
				default:
					continue
				}
				for _, key := range missing {
					out = append(out, Finding{
						ID:       "SEC003",
						Severity: SeverityError,
						File:     config.FileChannels,
						Key:      "channels." + name,
						Message:  fmt.Sprintf("channel %q has no %s in auth.toml", name, key),
						Fix:      fmt.Sprintf("run `holaclaw auth set channels.%s %s <value>`", name, key),
					})
				}
			}
			return out
		},
	}
}

// sec004Rule: auth.toml entries that name a provider, channel or connector
// not in the config are stale — most often left behind by a rename or
// removal.
func sec004Rule() Rule {
	return Rule{
		ID:      "SEC004",
		Summary: "auth.toml has an entry for a provider/channel/connector not in the config",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Secrets == nil || in.Config == nil {
				return nil
			}
			var out []Finding
			for _, name := range in.Secrets.Names(secrets.Providers) {
				if _, ok := in.Config.Providers[name]; ok {
					continue
				}
				out = append(out, Finding{
					ID:       "SEC004",
					Severity: SeverityWarning,
					File:     relFile(in.ConfigDir, in.SecretsPath),
					Key:      "providers." + name,
					Message:  fmt.Sprintf("auth.toml has a secret for provider %q, which is not configured", name),
					Fix:      fmt.Sprintf("run `holaclaw auth rm providers.%s` if it is no longer needed", name),
				})
			}
			for _, name := range in.Secrets.Names(secrets.Channels) {
				if _, ok := in.Config.Channels[name]; ok {
					continue
				}
				out = append(out, Finding{
					ID:       "SEC004",
					Severity: SeverityWarning,
					File:     relFile(in.ConfigDir, in.SecretsPath),
					Key:      "channels." + name,
					Message:  fmt.Sprintf("auth.toml has a secret for channel %q, which is not configured", name),
					Fix:      fmt.Sprintf("run `holaclaw auth rm channels.%s` if it is no longer needed", name),
				})
			}
			for _, name := range in.Secrets.Names(secrets.Connectors) {
				if _, ok := in.Config.Connectors[name]; ok {
					continue
				}
				out = append(out, Finding{
					ID:       "SEC004",
					Severity: SeverityWarning,
					File:     relFile(in.ConfigDir, in.SecretsPath),
					Key:      "connectors." + name,
					Message:  fmt.Sprintf("auth.toml has a secret for connector %q, which is not configured", name),
					Fix:      fmt.Sprintf("run `holaclaw auth rm connectors.%s` if it is no longer needed", name),
				})
			}
			return out
		},
	}
}

// sec005Rule: nothing under the config directory may be world-writable — a
// local user could otherwise rewrite what the daemon trusts.
func sec005Rule() Rule {
	return Rule{
		ID:      "SEC005",
		Summary: "config directory or a config file is world-writable",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.ConfigDir == "" {
				return nil
			}
			return worldWritable(in.ConfigDir)
		},
	}
}

// sec006Rule: a WhatsApp instance has nothing in auth.toml; its session
// database is the credential, and it holds a device only once the instance
// is linked (docs/channels/whatsapp.md §3). The daemon creates the file empty
// when it starts, so the rule looks inside it, opening it read-only.
func sec006Rule() Rule {
	return Rule{
		ID:      "SEC006",
		Summary: "whatsapp channel instance is not linked",
		Run: func(ctx context.Context, in *Input) []Finding {
			if in.Config == nil || in.DataDir == "" {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Channels) {
				// A name that is not a file name never gets a session (the
				// daemon refuses it, internal/daemon/channels.go), and must
				// not lead the rule out of the data directory.
				if in.Config.Channels[name].Type != "whatsapp" ||
					name == "." || name == ".." || filepath.Base(name) != name {
					continue
				}
				linked, known := whatsappLinked(ctx, filepath.Join(in.DataDir, "whatsapp", name+".db"))
				if !known || linked {
					continue
				}
				// The CLI picks a channel named after its type by default.
				login := "holaclaw login whatsapp --name " + name
				if name == "whatsapp" {
					login = "holaclaw login whatsapp"
				}
				out = append(out, Finding{
					ID:       "SEC006",
					Severity: SeverityWarning,
					File:     config.FileChannels,
					Key:      "channels." + name,
					Message:  fmt.Sprintf("channel %q is not linked to a WhatsApp account", name),
					Fix:      "run `" + login + " --phone <number>`",
				})
			}
			return out
		},
	}
}

// sec007Rule: a key stored for a provider that does not authenticate is
// never sent — most often a key added with `auth set` after an init that
// wrote the provider without one.
func sec007Rule() Rule {
	return Rule{
		ID:      "SEC007",
		Summary: "provider has an api_key but no api-key auth",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Providers) {
				p := in.Config.Providers[name]
				if p.Auth != "" && p.Auth != "none" {
					continue
				}
				if p.APIKey == "" && !secretHas(in.Secrets, secrets.Providers, name, "api_key") {
					continue
				}
				out = append(out, Finding{
					ID:       "SEC007",
					Severity: SeverityWarning,
					File:     config.FileConfig,
					Key:      "providers." + name + ".auth",
					Message:  fmt.Sprintf("provider %q has an api_key but no auth = \"api-key\": the key is never sent", name),
					Fix:      fmt.Sprintf("set providers.%s.auth = \"api-key\", or run `holaclaw auth rm providers.%s api_key`", name, name),
				})
			}
			return out
		},
	}
}

// whatsappLinked reports whether the whatsmeow session database at path
// holds a device. known is false when the file cannot be read (another
// user's, say): the rule then says nothing rather than guess. The file is
// opened read-only and its devices are counted, never read.
func whatsappLinked(ctx context.Context, path string) (linked, known bool) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return false, true
	} else if err != nil {
		return false, false
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: sessionReadQuery(path)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return false, false
	}
	defer func() { _ = db.Close() }()
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'whatsmeow_device'`).Scan(&tables); err != nil {
		return false, false
	}
	if tables == 0 {
		return false, true // created but never migrated: no device yet
	}
	var devices int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM whatsmeow_device`).Scan(&devices); err != nil {
		return false, false
	}
	return devices > 0, true
}

// sessionReadQuery is how whatsappLinked opens the session database without
// creating or changing a file. Even read-only, SQLite creates the -wal and
// -shm files of a WAL database when they are missing, so:
//
//   - A WAL database with no -wal file was closed cleanly: every commit is
//     in the main file and no connection has it open (one in WAL mode always
//     keeps a -wal). immutable=1 reads that file with no locks and no side
//     files. It is never used while the -wal exists, so it cannot miss what
//     a running daemon wrote; a daemon starting in between writes to a new
//     -wal and leaves the main file alone until its first checkpoint.
//   - Any other database (the daemon running, a rollback journal, an empty
//     file) is read through the normal locks. readonly_shm=1 opens an
//     existing -shm read-only and fails rather than create a missing one.
func sessionReadQuery(path string) string {
	const ro = "mode=ro&readonly_shm=1&_pragma=busy_timeout(5000)"
	if !walHeader(path) {
		return ro
	}
	if _, err := os.Lstat(path + "-wal"); errors.Is(err, fs.ErrNotExist) {
		return "mode=ro&immutable=1"
	}
	return ro
}

// walHeader reports whether the SQLite file at path says it is in WAL mode:
// its read and write format versions (header bytes 18 and 19) are 2.
func walHeader(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var h [20]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return false
	}
	return h[18] == 2 && h[19] == 2
}

// envResolves reports whether v is an `env:VAR` reference whose variable is
// set and non-empty.
func envResolves(v string) bool {
	if v == "" {
		return false
	}
	if _, err := config.ResolveEnv(v); err != nil {
		return false
	}
	// A literal (non-"env:") value "resolves" too as far as ResolveEnv is
	// concerned, but that case is always flagged separately as CFG009; only
	// count it here if it actually looks like an env reference.
	return len(v) > 4 && v[:4] == "env:"
}

func secretHas(f *secrets.File, section secrets.Section, name, key string) bool {
	if f == nil {
		return false
	}
	_, ok := f.Get(section, name, key)
	return ok
}
