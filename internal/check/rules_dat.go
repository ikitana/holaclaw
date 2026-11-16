package check

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// dbFileName mirrors internal/store's SQLite file name.
const dbFileName = "holaclaw.db"

// dat001Rule: the data directory must exist, or be creatable, and be
// writable — everything the store, blobs and traces need lives there.
func dat001Rule() Rule {
	return Rule{
		ID:      "DAT001",
		Summary: "data dir is not writable and cannot be created",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.DataDir == "" {
				return nil
			}
			if err := checkWritable(in.DataDir); err != nil {
				return []Finding{{
					ID:       "DAT001",
					Severity: SeverityError,
					File:     in.DataDir,
					Message:  err.Error(),
					Fix:      "create " + in.DataDir + " and make it writable by the daemon's user, or set server.data_dir",
				}}
			}
			return nil
		},
	}
}

// dat002Rule: a database file readable by group or other on disk can leak
// every message, memory and token it holds to any local user.
func dat002Rule() Rule {
	return Rule{
		ID:      "DAT002",
		Summary: "the database file is group- or world-readable",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.DataDir == "" {
				return nil
			}
			dbPath := filepath.Join(in.DataDir, dbFileName)
			info, err := os.Stat(dbPath)
			if err != nil {
				return nil // no database yet: nothing to leak
			}
			if info.Mode().Perm()&0o077 == 0 {
				return nil
			}
			return []Finding{{
				ID:       "DAT002",
				Severity: SeverityWarning,
				File:     dbPath,
				Message:  fmt.Sprintf("database file mode %s allows group/other access", info.Mode().Perm()),
				Fix:      "chmod 600 " + dbPath,
			}}
		},
	}
}

// checkWritable creates dir if missing and proves it is writable by writing
// and removing a probe file — a plain stat is not enough, since a read-only
// filesystem can still list a directory.
func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".holaclaw-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
