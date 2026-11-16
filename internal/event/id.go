// Package event holds the core data types every other package shares: ids,
// the inbound Envelope, Message and Attachment, and the trace Event.
//
// It is a leaf package: it imports nothing from holaclaw.
package event

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// ID is a ULID in its canonical 26-character text form. Trace ids, span ids,
// message ids and most row ids are IDs. Client-generated message ids double
// as idempotency keys.
type ID string

var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

// NewID returns a new monotonic ULID for the current time.
func NewID() ID {
	return NewIDAt(time.Now())
}

// NewIDAt returns a new ULID for the given time.
func NewIDAt(t time.Time) ID {
	entropyMu.Lock()
	defer entropyMu.Unlock()
	return ID(ulid.MustNew(ulid.Timestamp(t), entropy).String())
}

// Valid reports whether id parses as a ULID.
func (id ID) Valid() bool {
	_, err := ulid.ParseStrict(string(id))
	return err == nil
}

// Time returns the timestamp embedded in the id, or the zero time if invalid.
func (id ID) Time() time.Time {
	u, err := ulid.ParseStrict(string(id))
	if err != nil {
		return time.Time{}
	}
	return ulid.Time(u.Time())
}

func (id ID) String() string { return string(id) }
