package store

import (
	"errors"
	"fmt"
)

var (
	// ErrTurnActive is returned by BeginTurn when the session is already
	// claimed by another turn.
	ErrTurnActive = errors.New("store: turn already active")
	// ErrTurnMismatch is returned by EndTurn when the token is not the one
	// currently held by the session.
	ErrTurnMismatch = errors.New("store: turn token mismatch")
	// ErrAliasLinked is returned when an alias being linked already
	// belongs to another identity (auth links inside its own transaction).
	ErrAliasLinked = errors.New("store: alias linked to another identity")
	// ErrRoomDisabled is returned when approving a room an admin switched off.
	ErrRoomDisabled = errors.New("store: room is disabled")
	// ErrBlobTooLarge is returned by PutBlob when the reader exceeds the
	// configured size cap.
	ErrBlobTooLarge = errors.New("store: blob too large")
)

// ErrSchemaNewer reports a database written by a newer binary; the migration
// runner is forward-only and refuses to touch it.
type ErrSchemaNewer struct {
	DBVersion   int // version recorded in the database
	KnownLatest int // newest migration this binary carries
}

func (e *ErrSchemaNewer) Error() string {
	return fmt.Sprintf("store: database schema version %d is newer than this binary knows (%d)", e.DBVersion, e.KnownLatest)
}
