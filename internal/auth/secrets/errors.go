package secrets

import (
	"errors"
	"fmt"
	"io/fs"
)

// Sentinels for the failure modes of auth.toml. They exist so callers can
// branch (`holaclaw check` reports a permission problem, a loader reports a
// malformed file); the concrete errors carry a key path and never a value.
var (
	// ErrMalformed reports TOML that does not parse.
	ErrMalformed = errors.New("secrets: malformed TOML")
	// ErrUnknownKey reports a top-level key other than version and the
	// sections.
	ErrUnknownKey = errors.New("secrets: unknown key")
	// ErrNotString reports a value that is not a string, or a section that is
	// not a table of tables.
	ErrNotString = errors.New("secrets: value is not a string")
	// ErrBadVersion reports a missing version, a non-integer version or a
	// version this binary does not know.
	ErrBadVersion = errors.New("secrets: unsupported version")
	// ErrBadName reports an empty or unrepresentable entry or key name.
	ErrBadName = errors.New("secrets: invalid name")
	// ErrBadMode reports a file that is group- or world-accessible.
	ErrBadMode = errors.New("secrets: file is readable or writable by others")
	// ErrNotRegular reports a symlink, directory or device where the secret
	// file should be.
	ErrNotRegular = errors.New("secrets: not a regular file")
)

// LoadError is a validation failure of the file's content. Key is the dotted
// path of the offending key, never its value: this error text is safe to log.
type LoadError struct {
	Path   string // file path
	Key    string // dotted key path, empty for file-level problems
	Detail string // position or type name — never a secret value
	Err    error  // one of the sentinels above
}

func (e *LoadError) Error() string {
	msg := e.Err.Error()
	if e.Key != "" {
		msg = fmt.Sprintf("%s: %s", msg, e.Key)
	}
	if e.Detail != "" {
		msg = fmt.Sprintf("%s (%s)", msg, e.Detail)
	}
	return fmt.Sprintf("%s: %s", msg, e.Path)
}

func (e *LoadError) Unwrap() error { return e.Err }

// PermError is what CheckPerms reports, and what Set refuses to write through.
type PermError struct {
	Path string
	Mode fs.FileMode
	Err  error // ErrBadMode or ErrNotRegular
}

func (e *PermError) Error() string {
	return fmt.Sprintf("%s: %s (mode %s)", e.Err, e.Path, e.Mode)
}

func (e *PermError) Unwrap() error { return e.Err }
