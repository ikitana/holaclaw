package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"holaclaw/internal/app"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/channel/googlechat"
	"holaclaw/internal/channel/telegram"
	"holaclaw/internal/config"
)

// newAuthCmd is the auth.toml group. It works offline, straight on the file:
// the daemon re-reads its secrets per request, so a running daemon picks a
// change up without a restart (ARCHITECTURE.md §4). Values are never printed
// back.
func newAuthCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "manage the secrets in auth.toml"}
	set := &cobra.Command{
		Use:   "set <providers|channels>.<name> <key> [value]",
		Short: "store one secret",
		Long: "store one secret\n\n" +
			"with no value the secret is read from stdin: printf %s \"$KEY\" | holaclaw auth set providers.openrouter api_key\n" +
			"a multi-line secret (a service-account key) is read whole from stdin: holaclaw auth set channels.googlechat service_account < key.json",
		Args: cobra.RangeArgs(2, 3),
	}
	noVerify := set.Flags().Bool("no-verify", false, "store a channel bot token or service-account key without checking it with the platform")
	// Flags go before the entry: a value given as an argument is taken as
	// it is, even one that starts with a dash.
	set.Flags().SetInterspersed(false)
	set.RunE = func(cmd *cobra.Command, args []string) error { return cmdAuthSet(cmd.Context(), c, *noVerify, args) }
	cmd.AddCommand(set,
		&cobra.Command{
			Use: "list", Short: "list entry and key names, never a value", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error { return cmdAuthList(c) },
		},
		&cobra.Command{
			Use: "rm <providers|channels|connectors>.<name> [key...]", Short: "remove keys, or the whole entry",
			Args: cobra.MinimumNArgs(1),
			RunE: func(_ *cobra.Command, args []string) error { return cmdAuthRm(c, args) },
		})
	return cmd
}

// cmdAuthSet writes one secret. The value comes from the argument, or from
// stdin when it is left out — piping it is the safe way, because an argument
// is visible in the process list and the shell history:
//
//	printf %s "$KEY" | holaclaw auth set providers.openrouter api_key
func cmdAuthSet(ctx context.Context, c *cli, noVerify bool, args []string) error {
	dir := &c.configDir
	section, name, err := splitEntry(args[0])
	if err != nil {
		return err
	}
	key := args[1]

	value := ""
	switch {
	case len(args) == 3 && key == "service_account":
		// A service-account key on argv sits in the process list and the
		// shell history for as long as either keeps it — pipe the file
		// instead, the way the usage text above shows.
		return fmt.Errorf("auth set: refusing a service-account key given as an argument for %q — "+
			"it would leak into argv and the shell history; pipe the file instead: "+
			"holaclaw auth set %s %s < key.json", name, args[0], key)
	case len(args) == 3:
		value = args[2]
	case key == "service_account":
		// The key is multi-line JSON: a single ReadString('\n') would
		// truncate it after the first line.
		if value, err = readSecretAll(c); err != nil {
			return err
		}
	default:
		if value, err = readSecret(c); err != nil {
			return err
		}
	}
	if value == "" {
		return fmt.Errorf("auth set: the value is empty")
	}
	// Before the write, not after: a token or key the platform has already
	// refused must not reach auth.toml at all.
	if !noVerify && section == secrets.Channels {
		if err := verifyChannelSecret(ctx, c, *dir, name, key, value); err != nil {
			return err
		}
	}

	file, err := openSecrets(*dir)
	if err != nil {
		return err
	}
	if err := file.Set(section, name, map[string]string{key: value}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "set %s.%s %s in %s\n", section, name, key, file.Path())
	return nil
}

// telegramAPIBase is where a token is checked. A var so a test can point it at
// an httptest server; it is not configuration.
var telegramAPIBase = telegram.DefaultBaseURL

// googlechatAPIBase is where a service-account key is checked. A var so a
// test can point it at an httptest server; empty means Google's own token,
// Pub/Sub and Chat endpoints, which is what googlechat.VerifyOptions.BaseURL
// already defaults to.
var googlechatAPIBase = ""

// verifyChannelSecret checks a channel secret with its platform before it is
// stored, dispatching on the key's name. The channel type is re-checked by
// each verifier itself, the way verifyBotToken always has, so a key stored
// before the matching channels.toml entry exists is still just stored.
func verifyChannelSecret(ctx context.Context, c *cli, dir, name, key, value string) error {
	switch key {
	case "bot_token":
		return verifyBotToken(ctx, c, dir, name, value)
	case "service_account":
		return verifyServiceAccount(ctx, c, dir, name, value)
	default:
		return nil
	}
}

// verifyBotToken checks a bot token with the platform before it is stored.
// Without this the first sign that a token is wrong comes much later, when the
// daemon starts polling: `holaclaw check` only asks whether the key is present
// (SEC003), so a truncated paste passes every offline gate there is.
//
// Only a definitive answer refuses the write. An unreachable platform does
// not: a token has to be settable on a box that cannot talk to Telegram yet.
// The channel type decides the check, and an unreadable config simply means no
// check — this command's job is to write the secret, not to validate a tree it
// does not otherwise read.
func verifyBotToken(ctx context.Context, c *cli, dir, name, token string) error {
	if channelType(dir, name) != "telegram" {
		return nil
	}
	if !telegram.LooksLikeToken(token) {
		return fmt.Errorf("auth set: the value for %q does not look like a Telegram bot token — "+
			"BotFather issues \"<id>:<secret>\"; re-copy it, or pass --no-verify to store it anyway", name)
	}
	vctx, cancel := context.WithTimeout(ctx, telegram.DefaultVerifyTimeout)
	defer cancel()

	id, err := telegram.VerifyToken(vctx, telegram.VerifyOptions{Token: token, BaseURL: telegramAPIBase})
	switch {
	case err == nil:
		if id.Name != "" {
			_, _ = fmt.Fprintf(c.stdout, "verified @%s (%s)\n", id.Username, id.Name)
		} else {
			_, _ = fmt.Fprintf(c.stdout, "verified @%s\n", id.Username)
		}
		return nil
	case errors.Is(err, telegram.ErrAuth):
		return fmt.Errorf("auth set: Telegram rejected that token for %q — check it with BotFather, "+
			"or pass --no-verify to store it anyway", name)
	case errors.Is(err, telegram.ErrNetwork), errors.Is(err, telegram.ErrServer), errors.Is(err, context.DeadlineExceeded):
		_, _ = fmt.Fprintf(c.stderr, "warning: could not reach Telegram to verify the token (%v); storing it unchecked\n", err)
		return nil
	default:
		return fmt.Errorf("auth set: verifying the token for %q: %w", name, err)
	}
}

// verifyServiceAccount checks a Google Chat service-account key the way
// `auth set` promises in docs/channels/googlechat.md §3: it runs every step
// Verify runs (the key parses, Google issues a token for it, the `holaclaw`
// subscription in the key's project is readable, the Chat app is configured)
// and refuses to store the key on the first definitive failure. An
// unreachable Google does not refuse: the key has to be settable on a box
// that cannot reach Google yet, the same as a Telegram token.
func verifyServiceAccount(ctx context.Context, c *cli, dir, name, saJSON string) error {
	if channelType(dir, name) != "googlechat" {
		return nil
	}
	vctx, cancel := context.WithTimeout(ctx, googlechat.DefaultVerifyTimeout)
	defer cancel()

	res, err := googlechat.Verify(vctx, googlechat.VerifyOptions{Key: saJSON, BaseURL: googlechatAPIBase})
	switch {
	case err == nil:
		_, _ = fmt.Fprintf(c.stdout, "verified %s (project %s)\n", res.ClientEmail, res.Project)
		return nil
	case errors.Is(err, googlechat.ErrNetwork), errors.Is(err, googlechat.ErrServer), errors.Is(err, context.DeadlineExceeded):
		_, _ = fmt.Fprintf(c.stderr, "warning: could not reach Google to verify the service-account key (%v); storing it unchecked\n", err)
		return nil
	default:
		return googlechatVerifyErr(err, name, res)
	}
}

// googlechatVerifyErr maps a googlechat.Verify failure onto the message
// docs/channels/googlechat.md §3 names for it, pointing back at the setup
// step of the operator guide to redo. The network/server cases are handled by
// the caller before this is reached.
func googlechatVerifyErr(err error, name string, res googlechat.VerifyResult) error {
	var e *googlechat.Error
	errors.As(err, &e)
	switch {
	case errors.Is(err, googlechat.ErrBadKey):
		return fmt.Errorf("auth set: %q is not a service-account key (docs/guides/googlechat.md §2 step 3) — "+
			"download the JSON key from the console again, or pass --no-verify to store it anyway: %w", name, err)
	case errors.Is(err, googlechat.ErrAuth):
		return fmt.Errorf("auth set: Google rejected this key for %q: it was deleted or disabled "+
			"(docs/guides/googlechat.md §2 step 3) — issue a new key, or pass --no-verify to store it anyway", name)
	case errors.Is(err, googlechat.ErrNotFound) && e != nil && e.Op == googlechat.OpSubscriptionCheck:
		return fmt.Errorf("auth set: no subscription `holaclaw` in project `%s` for %q "+
			"(docs/guides/googlechat.md §2 step 5) — create it, or pass --no-verify to store the key anyway", res.Project, name)
	case errors.Is(err, googlechat.ErrForbidden) && e != nil && e.Op == googlechat.OpSubscriptionCheck:
		return fmt.Errorf("auth set: the service account %s cannot pull from subscription `holaclaw` in project `%s` for %q "+
			"(docs/guides/googlechat.md §2 step 5) — grant that account the Pub/Sub Subscriber role on the subscription "+
			"(a new grant can take a few minutes to apply), or pass --no-verify to store the key anyway",
			res.ClientEmail, res.Project, name)
	case errors.Is(err, googlechat.ErrNotConfigured):
		return fmt.Errorf("auth set: the Chat app is not configured for %q (docs/guides/googlechat.md §2 step 6) — "+
			"finish the console setup, or pass --no-verify to store the key anyway", name)
	default:
		return fmt.Errorf("auth set: verifying the service-account key for %q: %w", name, err)
	}
}

// channelType reports the configured type of a channel instance, or "" when
// the config cannot be read or names no such instance.
func channelType(dir, name string) string {
	cfg, err := config.Load(dir)
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.Channels[name].Type
}

// readSecret reads one line from stdin. There is no TTY echo control in the
// standard library, so an interactive value is echoed — pipe it instead.
func readSecret(c *cli) (string, error) {
	r := bufio.NewReader(c.stdin)
	line, err := r.ReadString('\n')
	if line == "" && err != nil {
		return "", fmt.Errorf("auth set: reading the value from stdin: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readSecretAll reads all of stdin, trimmed of surrounding whitespace. A
// one-line read (readSecret) would truncate a multi-line secret such as a
// service-account key after its first line.
func readSecretAll(c *cli) (string, error) {
	b, err := io.ReadAll(c.stdin)
	if err != nil {
		return "", fmt.Errorf("auth set: reading the value from stdin: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// cmdAuthList prints entry names and key names. Never a value: this command
// exists so an operator can see what is configured, not what it is.
func cmdAuthList(c *cli) error {
	file, err := openSecrets(c.configDir)
	if err != nil {
		return err
	}
	empty := true
	for _, section := range secrets.Sections() {
		for _, name := range file.Names(section) {
			empty = false
			_, _ = fmt.Fprintf(c.stdout, "%s.%s: %s\n", section, name, strings.Join(file.Keys(section, name), ", "))
		}
	}
	if empty {
		_, _ = fmt.Fprintf(c.stdout, "no secrets in %s\n", file.Path())
	}
	return nil
}

// cmdAuthRm removes named keys, or the whole entry when no key is given.
func cmdAuthRm(c *cli, args []string) error {
	section, name, err := splitEntry(args[0])
	if err != nil {
		return err
	}
	file, err := openSecrets(c.configDir)
	if err != nil {
		return err
	}
	keys := args[1:]
	if err := file.Delete(section, name, keys...); err != nil {
		return err
	}
	if len(keys) == 0 {
		_, _ = fmt.Fprintf(c.stdout, "removed %s.%s\n", section, name)
	} else {
		_, _ = fmt.Fprintf(c.stdout, "removed %s from %s.%s\n", strings.Join(keys, ", "), section, name)
	}
	return nil
}

// openSecrets opens auth.toml where server.secrets_dir puts it. It reads that
// one key itself: storing a key must not wait for the rest of the config to
// load, and a directory with no config.toml yet has the default.
func openSecrets(dir string) (*secrets.File, error) {
	s, err := serverDirs(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s.SecretsDir != "" && !filepath.IsAbs(s.SecretsDir) {
		return nil, fmt.Errorf("%s: server.secrets_dir %q is not an absolute path", config.FileConfig, s.SecretsDir)
	}
	return secrets.Open(app.SecretsPath(s.SecretsDir))
}

// splitEntry parses "providers.openrouter" into its section and entry name.
func splitEntry(s string) (secrets.Section, string, error) {
	sec, name, ok := strings.Cut(s, ".")
	if !ok || name == "" {
		return "", "", fmt.Errorf("auth: %q must be <providers|channels|connectors>.<name>", s)
	}
	if slices.Contains(secrets.Sections(), secrets.Section(sec)) {
		return secrets.Section(sec), name, nil
	}
	return "", "", fmt.Errorf("auth: unknown section %q, expected providers, channels or connectors", sec)
}
