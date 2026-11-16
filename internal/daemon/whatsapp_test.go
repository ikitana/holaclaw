package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/channel"
	"holaclaw/internal/testmodel"
)

// TestWhatsAppChannelIsBuilt runs a daemon with an unlinked WhatsApp instance
// next to the Telegram one: the adapter opens its own session database under
// the data directory, never touches the network, and reports unlinked with
// the login hint (docs/channels/whatsapp.md §5.2, §5.3).
func TestWhatsAppChannelIsBuilt(t *testing.T) {
	model := testmodel.New(t)
	dir := t.TempDir()
	e := &env{
		t: t, dir: dir, dataDir: filepath.Join(dir, "data"),
		socket: shortSocket(t), model: model, ctx: context.Background(),
	}
	te := &tgEnv{env: e, fake: newFakeTG(t)}
	writeTGConfig(t, e, tgOpts{}, model.URL())
	f, err := os.OpenFile(filepath.Join(dir, "channels.toml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[channels.wa]\ntype  = \"whatsapp\"\nagent = \"main\"\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	te.launch()

	until(t, "wa to report unlinked", func() bool {
		list, err := e.client.ListChannels(e.ctx)
		if err != nil {
			t.Fatalf("list channels: %v", err)
		}
		for _, c := range list {
			if c.Name == "wa" && c.Type == "whatsapp" && c.State == gen.ChannelStatusState(channel.StateUnlinked) &&
				c.Detail != nil && strings.Contains(*c.Detail, "holaclaw login whatsapp --name wa --phone") {
				return true
			}
		}
		return false
	})

	path := filepath.Join(e.dataDir, "whatsapp", "wa.db")
	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", p, st.Mode().Perm(), want)
		}
	}
	ch := e.daemon.channels.byName["wa"]
	if _, ok := ch.(channel.Linker); !ok {
		t.Errorf("the whatsapp adapter (%T) is not a channel.Linker", ch)
	}
	if _, ok := ch.(channel.AliasNamespacer); !ok {
		t.Errorf("the whatsapp adapter (%T) declares no alias namespaces", ch)
	}

	// The login endpoints reach the running adapter. Only requests the
	// adapter refuses before it connects are made: no network.
	st, err := e.client.ChannelLogin(e.ctx, "wa")
	if err != nil || st.State != gen.DeviceLoginStateNone || st.UserCode != nil {
		t.Errorf("login state = %+v, %v; want none without a code", st, err)
	}
	wantErr := func(what string, err error, status int, msg string) {
		t.Helper()
		var ce *api.ClientError
		if !errors.As(err, &ce) || ce.Status != status || !strings.Contains(ce.Message, msg) {
			t.Errorf("%s: err = %v, want %d mentioning %q", what, err, status, msg)
		}
	}
	_, err = e.client.StartChannelLogin(e.ctx, "wa", "0600 000 000")
	wantErr("national number", err, http.StatusBadRequest, "international format")
	wantErr("logout unlinked", e.client.ChannelLogout(e.ctx, "wa"), http.StatusConflict, "not linked")
	_, err = e.client.ChannelLogin(e.ctx, "tg")
	wantErr("telegram", err, http.StatusNotImplemented, "no interactive login")
	_, err = e.client.ChannelLogin(e.ctx, "nope")
	wantErr("unknown", err, http.StatusNotFound, "not found")
}
