// Package storetest holds helpers for tests that need rows the production
// paths only create as a side effect of an inbound message.
package storetest

import (
	"context"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// Bind gives identity id a binding for each alias on channel, with display as
// the shown name. It fails the test when an alias is empty, already bound or
// the identity does not exist.
func Bind(t testing.TB, st *store.SQLite, channel string, aliases []string, id event.ID, display string) {
	t.Helper()
	for _, alias := range aliases {
		_, err := st.DB().ExecContext(context.Background(),
			`INSERT INTO bindings (channel, alias, identity_id, display, created_at) VALUES (?, ?, ?, ?, ?)`,
			channel, alias, string(id), display, time.Now().UnixMilli())
		if err != nil {
			t.Fatalf("storetest: bind %s/%s to %s: %v", channel, alias, id, err)
		}
	}
}
