package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestConnectorRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	last := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	expiry := last.Add(time.Hour)
	updated := last.Add(time.Minute)
	in := Connector{
		ID: "github-admin", State: ConnectorConnected, Detail: "ok",
		ServerName: "github", ServerVersion: "1.2.3", SessionID: "sess-1", ClientID: "dcr-1",
		CIMDRefused: true,
		AuthMeta:    json.RawMessage(`{"authorization_servers":["https://auth.example"]}`),
		Tools:       json.RawMessage(`[{"name":"create_issue"}]`),
		Scopes:      []string{"repo", "issues:write"},
		LastCheck:   &last, Failures: 2, TokenExpiry: &expiry, UpdatedAt: updated,
	}
	if err := s.PutConnector(ctx, in); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetConnector(ctx, "github-admin")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, in)
	}
}

func TestConnectorDefaultsAndReplace(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	before := time.Now().Add(-time.Second)
	if err := s.PutConnector(ctx, Connector{ID: "homes", State: ConnectorPending}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetConnector(ctx, "homes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.AuthMeta) != "{}" || string(got.Tools) != "[]" || got.Scopes == nil || len(got.Scopes) != 0 {
		t.Fatalf("defaults = meta %s tools %s scopes %#v", got.AuthMeta, got.Tools, got.Scopes)
	}
	if got.LastCheck != nil || got.TokenExpiry != nil || got.CIMDRefused {
		t.Fatalf("optional fields set: %+v", got)
	}
	if got.UpdatedAt.Before(before) {
		t.Fatalf("UpdatedAt = %v, want about now", got.UpdatedAt)
	}

	// Put replaces the whole row.
	if err := s.PutConnector(ctx, Connector{ID: "homes", State: ConnectorDegraded, Failures: 3}); err != nil {
		t.Fatalf("put again: %v", err)
	}
	got, err = s.GetConnector(ctx, "homes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != ConnectorDegraded || got.Failures != 3 {
		t.Fatalf("after replace: %+v", got)
	}
}

func TestConnectorListAndDelete(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if list, err := s.ListConnectors(ctx); err != nil || len(list) != 0 {
		t.Fatalf("empty list = %v, %v", list, err)
	}
	for _, id := range []string{"zeta", "alpha", "mid"} {
		if err := s.PutConnector(ctx, Connector{ID: id, State: ConnectorConnected}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	list, err := s.ListConnectors(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var ids []string
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	if !reflect.DeepEqual(ids, []string{"alpha", "mid", "zeta"}) {
		t.Fatalf("ids = %v", ids)
	}

	if err := s.DeleteConnector(ctx, "mid"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetConnector(ctx, "mid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted = %v, want ErrNotFound", err)
	}
	if err := s.DeleteConnector(ctx, "mid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice = %v, want ErrNotFound", err)
	}
}

func TestConnectorPutRejects(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cases := map[string]Connector{
		"empty id":        {State: ConnectorPending},
		"unknown state":   {ID: "a", State: "deleted"},
		"bad auth meta":   {ID: "a", State: ConnectorPending, AuthMeta: json.RawMessage(`{nope`)},
		"bad tools":       {ID: "a", State: ConnectorPending, Tools: json.RawMessage(`[`)},
		"no state at all": {ID: "a"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := s.PutConnector(ctx, c); err == nil {
				t.Fatal("put succeeded")
			}
		})
	}
	if _, err := s.GetConnector(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a rejected put left a row: %v", err)
	}
}
