package channel_test

import (
	"context"
	"testing"

	"holaclaw/internal/channel"
	"holaclaw/internal/store"
)

func TestSQLState(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	a := channel.SQLState{DB: st.DB(), Channel: "tg-a"}
	b := channel.SQLState{DB: st.DB(), Channel: "tg-b"}
	if _, ok, err := a.Get(ctx, "offset"); err != nil || ok {
		t.Fatalf("empty get: %v %v", ok, err)
	}
	for _, v := range []string{"1", "2"} {
		if err := a.Set(ctx, "offset", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok, _ := a.Get(ctx, "offset"); !ok || v != "2" {
		t.Fatalf("got %q %v", v, ok)
	}
	if _, ok, _ := b.Get(ctx, "offset"); ok {
		t.Fatal("state leaked across channel instances")
	}
}
