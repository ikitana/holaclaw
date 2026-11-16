package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var _ ConnectorStore = (*SQLite)(nil)

const connectorColumns = `id, state, detail, server_name, server_version, session_id, client_id,
	cimd_refused, auth_meta, tools, scopes, last_check, failures, token_expiry, updated_at`

// PutConnector inserts or replaces the whole cache row. Empty JSON fields are
// stored as an empty object (auth metadata) or array (tools, scopes).
func (s *SQLite) PutConnector(ctx context.Context, c Connector) error {
	if c.ID == "" {
		return errors.New("store: put connector: empty id")
	}
	meta, tools := jsonOr(c.AuthMeta, "{}"), jsonOr(c.Tools, "[]")
	scopes := c.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return fmt.Errorf("store: put connector: %w", err)
	}
	cimd := 0
	if c.CIMDRefused {
		cimd = 1
	}
	_, err = s.exec(ctx, `INSERT OR REPLACE INTO connectors (`+connectorColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, string(c.State), c.Detail, c.ServerName, c.ServerVersion, c.SessionID, c.ClientID,
		cimd, meta, tools, string(scopesJSON), nullTime(c.LastCheck), c.Failures,
		nullTime(c.TokenExpiry), ms(c.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: put connector: %w", err)
	}
	return nil
}

func (s *SQLite) GetConnector(ctx context.Context, id string) (Connector, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+connectorColumns+` FROM connectors WHERE id = ?`, id)
	c, err := scanConnector(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Connector{}, ErrNotFound
	}
	if err != nil {
		return Connector{}, fmt.Errorf("store: get connector: %w", err)
	}
	return c, nil
}

func (s *SQLite) ListConnectors(ctx context.Context) ([]Connector, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+connectorColumns+` FROM connectors ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list connectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list connectors: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConnector removes the row; a missing row is ErrNotFound.
func (s *SQLite) DeleteConnector(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `DELETE FROM connectors WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete connector: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete connector: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanConnector(sc interface{ Scan(...any) error }) (Connector, error) {
	var c Connector
	var state, meta, tools, scopes string
	var cimd int
	var last, expiry sql.NullInt64
	var updated int64
	if err := sc.Scan(&c.ID, &state, &c.Detail, &c.ServerName, &c.ServerVersion, &c.SessionID,
		&c.ClientID, &cimd, &meta, &tools, &scopes, &last, &c.Failures, &expiry, &updated); err != nil {
		return Connector{}, err
	}
	if err := json.Unmarshal([]byte(scopes), &c.Scopes); err != nil {
		return Connector{}, fmt.Errorf("decode scopes: %w", err)
	}
	c.State, c.CIMDRefused = ConnectorState(state), cimd == 1
	c.AuthMeta, c.Tools = json.RawMessage(meta), json.RawMessage(tools)
	c.LastCheck, c.TokenExpiry = timePtr(last), timePtr(expiry)
	c.UpdatedAt = fromMS(updated)
	return c, nil
}

// jsonOr returns raw as stored text, or def when it is empty.
func jsonOr(raw json.RawMessage, def string) string {
	if len(raw) == 0 {
		return def
	}
	return string(raw)
}

// nullTime stores an optional time as unix ms, nil as NULL.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func timePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMS(v.Int64)
	return &t
}
