package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
)

// loginPoll is how often a login is polled while the user acts on the code.
// A var so a test can shorten it; it is not configuration.
var loginPoll = 3 * time.Second

// The login and logout commands have one subcommand per type, each with its
// own flags. Both flows run inside the daemon — the single owner of the
// rotating tokens and of the session — and the CLI only shows the code and
// waits. A channel type with a login adds a `login <type>` and a
// `logout <type>` here.

func newLoginCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "login", Short: "sign a provider in or link a channel to its account"}
	cmd.AddCommand(newLoginCodexCmd(c), newLoginWhatsAppCmd(c))
	return cmd
}

func newLogoutCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "logout", Short: "unlink a channel from its account"}
	cmd.AddCommand(newLogoutWhatsAppCmd(c))
	return cmd
}

func newLoginCodexCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "codex",
		Short: "sign a codex provider in with a device code",
		Args:  nameNotArg,
	}
	cn := c.connFlags(cmd)
	// The daemon lists no providers by type, so there is no single one to
	// find: the default is the name the config.toml examples use.
	name := cmd.Flags().String("name", "codex", "the codex provider")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		client, err := cn.dial(ctx)
		if err != nil {
			return err
		}
		return providerLogin(ctx, c, client, *name)
	}
	return cmd
}

func newLoginWhatsAppCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "whatsapp",
		Short: "link a whatsapp channel with a pairing code",
		Long: "link a whatsapp channel with a pairing code\n\n" +
			"without --phone, a code still waiting for the phone is shown again",
		Args: nameNotArg,
	}
	cn := c.connFlags(cmd)
	name := cmd.Flags().String("name", "whatsapp", "the whatsapp channel")
	phone := cmd.Flags().String("phone", "", "the number to link, in international format")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		client, err := cn.dial(ctx)
		if err != nil {
			return err
		}
		ch, err := channelOfType(ctx, client, "whatsapp", "login", *name)
		if err != nil {
			return interrupted(ctx, err)
		}
		return channelLogin(ctx, c, client, cmd, ch, *phone)
	}
	return cmd
}

func newLogoutWhatsAppCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "whatsapp",
		Short: "unlink a whatsapp channel from its account",
		Args:  nameNotArg,
	}
	cn := c.connFlags(cmd)
	name := cmd.Flags().String("name", "whatsapp", "the whatsapp channel")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		client, err := cn.dial(ctx)
		if err != nil {
			return err
		}
		ch, err := channelOfType(ctx, client, "whatsapp", "logout", *name)
		if err != nil {
			return err
		}
		return channelLogout(ctx, c, client, ch)
	}
	return cmd
}

// channelOfType is the channel of type typ that `holaclaw <verb> <typ>
// --name <name>` acts on: the one named, which defaults to typ itself. The
// daemon decides, so a remote --addr works the same.
func channelOfType(ctx context.Context, client *api.Client, typ, verb, name string) (string, error) {
	channels, err := client.ListChannels(ctx)
	if err != nil {
		return "", err
	}
	var names []string
	for _, ch := range channels {
		if ch.Type == typ {
			names = append(names, ch.Name)
		}
		if ch.Name == name && ch.Type != typ {
			return "", fmt.Errorf("%q is a %s channel, not a %s one; pass --name <channel>", name, printable(ch.Type), typ)
		}
	}
	if !slices.Contains(names, name) {
		return "", missingChannel(typ, verb, name, names)
	}
	return name, nil
}

// missingChannel says which channels of typ the daemon has instead of name
// and, for a login, how to add it: the daemon reads its config only when it
// starts, so a channel just written to channels.toml is unknown until a
// restart.
func missingChannel(typ, verb, name string, names []string) error {
	have := "no " + typ + " channels"
	if len(names) > 0 {
		have = typ + " channels: " + printable(strings.Join(names, ", "))
	}
	msg := fmt.Sprintf("no %s channel named %q (%s); pass --name <channel>", typ, name, have)
	if verb != "login" {
		return errors.New(msg)
	}
	return fmt.Errorf("%s\nadd the instance to channels.toml and restart `holaclaw run`:\n\n"+
		"    [channels.%s]\n    type  = %q\n    agent = \"main\"", msg, name, typ)
}

// nameNotArg refuses a positional instance with the flag that names one, so
// `holaclaw login whatsapp sales` points at --name rather than at a subcommand
// that does not exist.
func nameNotArg(_ *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("unexpected argument %q; name the instance with --name %s", args[0], args[0])
}

// loginLine is the command that links channel name of type typ; the name is
// left out when it is the type's own, as the default channel usually is.
func loginLine(typ, name string) string {
	if name == typ {
		return "holaclaw login " + typ
	}
	return "holaclaw login " + typ + " --name " + name
}

// providerLogin runs a device-code login for a subscription provider (Codex).
func providerLogin(ctx context.Context, c *cli, client *api.Client, provider string) error {
	st, err := client.StartProviderLogin(ctx, provider)
	if err != nil {
		var ce *api.ClientError
		if errors.As(err, &ce) && ce.Status == http.StatusNotFound {
			return fmt.Errorf("no codex provider named %s on this daemon; pass --name <provider>\n"+
				"add it to config.toml and restart `holaclaw run`:\n\n"+
				"    [providers.%s]\n    type = \"codex\"\n    auth = \"subscription\"", provider, provider)
		}
		return interrupted(ctx, err)
	}
	if st.UserCode != nil && st.VerificationUrl != nil {
		_, _ = fmt.Fprintf(c.stdout, "Open %s and enter the code: %s\n", printable(*st.VerificationUrl), printable(*st.UserCode))
	}
	// What the provider says the sign-in needs, in its own words.
	if note := detailOr(st, ""); st.State == gen.DeviceLoginStatePending && note != "" {
		_, _ = fmt.Fprintln(c.stderr, "note: "+note)
	}
	_, _ = fmt.Fprintln(c.stderr, "waiting for the login to complete (Ctrl-C stops waiting; the daemon keeps polling)…")
	tick := time.NewTicker(loginPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return interrupted(ctx, ctx.Err())
		case <-tick.C:
		}
		st, err = client.ProviderLogin(ctx, provider)
		if err != nil {
			return interrupted(ctx, err)
		}
		switch st.State {
		case gen.DeviceLoginStateDone:
			_, _ = fmt.Fprintf(c.stdout, "%s: logged in\n", provider)
			return nil
		case gen.DeviceLoginStateFailed, gen.DeviceLoginStateNone:
			return fmt.Errorf("%s: %s", provider, detailOr(st, "login failed"))
		}
	}
}

// channelLogin links a channel with a pairing code (WhatsApp,
// docs/channels/whatsapp.md §3). A link still pending — an earlier run
// stopped with Ctrl-C, and the daemon kept the code — is resumed rather than
// started again, which would throw that code away; only a new one needs
// --phone.
func channelLogin(ctx context.Context, c *cli, client *api.Client, cmd *cobra.Command, name, phone string) error {
	st, err := client.ChannelLogin(ctx, name)
	if err != nil {
		return interrupted(ctx, channelError(name, err))
	}
	// A resumed code may be for another number than the one typed now, so
	// the instructions name a number only for a code requested here.
	onPhone := "On the phone"
	if st.State != gen.DeviceLoginStatePending {
		if phone == "" {
			return &usageError{cmd: cmd, err: fmt.Errorf("%s: --phone is required: %s --phone <number>", name, loginLine("whatsapp", name))}
		}
		if st, err = client.StartChannelLogin(ctx, name, phone); err != nil {
			return interrupted(ctx, channelError(name, err))
		}
		onPhone += " with " + phone
	}
	if st.State == gen.DeviceLoginStatePending && st.UserCode != nil {
		_, _ = fmt.Fprintf(c.stdout, "%s, open WhatsApp → Settings → Linked devices → Link a device\n"+
			"→ Link with phone number instead, and enter:\n\n    %s\n\n", onPhone, printable(*st.UserCode))
		expiry := ""
		if st.ExpiresAt != nil {
			expiry = "The code expires at " + st.ExpiresAt.Local().Format("15:04:05") + ". "
		}
		_, _ = fmt.Fprintf(c.stdout, "%sWhatsApp also shows a notification on the phone that opens this screen.\n", expiry)
		_, _ = fmt.Fprintln(c.stderr, "waiting for the phone… (Ctrl-C stops waiting; the daemon keeps the code alive until it expires)")
	}
	tick := time.NewTicker(loginPoll)
	defer tick.Stop()
	for {
		switch st.State {
		case gen.DeviceLoginStateDone:
			_, _ = fmt.Fprintf(c.stdout, "%s: %s\n", name, linkedAs(st))
			return nil
		case gen.DeviceLoginStateFailed, gen.DeviceLoginStateNone:
			return fmt.Errorf("%s: %s", name, detailOr(st, "the link failed; run the command again"))
		}
		select {
		case <-ctx.Done():
			return interrupted(ctx, ctx.Err())
		case <-tick.C:
		}
		if st, err = client.ChannelLogin(ctx, name); err != nil {
			return interrupted(ctx, channelError(name, err))
		}
	}
}

// channelError says what the daemon refused in its own words: its message
// is written for the user and already names the channel type ("whatsapp:
// already linked as +34…; run `holaclaw logout whatsapp wa` first"), and the
// status code adds nothing.
func channelError(name string, err error) error {
	var ce *api.ClientError
	if !errors.As(err, &ce) {
		return err
	}
	switch ce.Status {
	case http.StatusNotFound:
		return fmt.Errorf("no channel named %q on this daemon: add it to channels.toml and restart `holaclaw run`", name)
	case http.StatusNotImplemented, http.StatusBadRequest, http.StatusConflict, http.StatusBadGateway:
		return errors.New(printable(ce.Message))
	}
	return err
}

// interrupted is err, or once ctx is done (Ctrl-C) a quiet exit with the
// shell's status for SIGINT: the user stopped the wait and needs no
// "context canceled".
func interrupted(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return exitError(130)
	}
	return err
}

// printable drops the terminal escapes (CSI sequences and the OSC, DCS,
// SOS, PM and APC strings whole, in their ESC and 8-bit forms), the control
// characters (C0, DEL, C1), the line and paragraph separators and the
// format runes (Cf: bidi controls, zero-width space, soft hyphen, BOM, tags)
// of a string the daemon sent, so it cannot drive the terminal it is
// printed on, reorder what it shows or make two ids look alike. ZWJ and
// ZWNJ are kept so emoji sequences and Persian or Indic text render.
func printable(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == 0x1b && strings.HasPrefix(s[i+n:], "["):
			i = skipCSI(s, i+n+1)
		case r == 0x9b:
			i = skipCSI(s, i+n)
		case r == 0x1b && i+n < len(s) && strings.IndexByte("]PX^_", s[i+n]) >= 0:
			i = skipString(s, i+n+1)
		case r == 0x9d, r == 0x90, r == 0x98, r == 0x9e, r == 0x9f:
			i = skipString(s, i+n)
		default:
			if !hidden(r) {
				b.WriteRune(r)
			}
			i += n
		}
	}
	return b.String()
}

// skipCSI is the index after the CSI sequence whose parameters start at i:
// past its final byte (0x40–0x7E), or at the first byte that cannot be
// part of it.
func skipCSI(s string, i int) int {
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 0x40 && c <= 0x7e:
			return i + 1
		case c < 0x20 || c > 0x7e:
			return i
		}
	}
	return i
}

// skipString is the index after the control string (OSC, DCS, SOS, PM,
// APC) whose text starts at i: past its BEL, ESC \ or 8-bit ST, past a CAN
// or SUB that cancels it, at any other ESC, which starts a new sequence, or
// the end of s.
func skipString(s string, i int) int {
	for ; i < len(s); i++ {
		switch {
		case s[i] == 0x07, s[i] == 0x18, s[i] == 0x1a:
			return i + 1
		case strings.HasPrefix(s[i:], "\x1b\\"), strings.HasPrefix(s[i:], "\u009c"):
			return i + 2
		case s[i] == 0x1b:
			return i
		}
	}
	return i
}

// hidden is a rune printable drops on its own.
func hidden(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r <= 0x9f, r == 0x2028, r == 0x2029:
		return true
	}
	return unicode.Is(unicode.Cf, r) && r != 0x200c && r != 0x200d
}

// linkedAs is the line a finished link ends with. The daemon's detail names
// the account ("linked as +34600000000").
func linkedAs(st gen.DeviceLogin) string {
	d := detailOr(st, "")
	switch {
	case d == "":
		return "linked"
	case strings.HasPrefix(d, "linked"):
		return d
	}
	return "linked as " + d
}

func detailOr(st gen.DeviceLogin, fallback string) string {
	if st.Detail != nil && printable(*st.Detail) != "" {
		return printable(*st.Detail)
	}
	return fallback
}

// channelLogout unlinks a channel: the daemon removes the device from the
// phone's list and deletes the session.
func channelLogout(ctx context.Context, c *cli, client *api.Client, name string) error {
	if err := client.ChannelLogout(ctx, name); err != nil {
		var ce *api.ClientError
		if errors.As(err, &ce) && ce.Status == http.StatusNotImplemented {
			return fmt.Errorf("%s has no interactive login to log out of", name)
		}
		return channelError(name, err)
	}
	_, _ = fmt.Fprintf(c.stdout, "%s: unlinked\n", name)
	return nil
}
