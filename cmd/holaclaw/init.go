package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"holaclaw/deploy/example"
	"holaclaw/internal/app"
	"holaclaw/internal/auth"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/fsutil"
	"holaclaw/internal/store"
	"holaclaw/internal/termutil"
)

// errInitialised is the answer to a second `holaclaw init`.
var errInitialised = errors.New("holaclaw is already initialised: with the daemon running, create more tokens with `holaclaw token create`")

// initDataDirName is the data_dir of a config written by init: relative, so
// the data sits in the config directory and moves with it.
const initDataDirName = "data"

// initSecretsDirName is where its secrets go, inside the config directory
// too. secrets_dir is never relative, so the config names the full path.
const initSecretsDirName = "secrets"

// initModelName is the one model of a config written by init, and the main
// agent's chat role.
const initModelName = "default"

// initPreset is one --provider: the provider table init writes and the model
// it serves by default.
type initPreset struct {
	flag, desc string
	name, typ  string // [providers.<name>] and its type
	baseURL    string // "" = --base-url for openai-compat, none for codex
	auth       string // "api-key" or "subscription"
	sort       string
	model      string // "" = --model is required
	context    int
}

// initPresets are the --provider values, in the order the prompt lists them.
var initPresets = []initPreset{
	{flag: "openai", desc: "the OpenAI API, with an API key",
		name: "openai", typ: "openai-compatible", baseURL: "https://api.openai.com/v1", auth: "api-key",
		model: "gpt-6.1-sol", context: 128000},
	{flag: "openai-codex", desc: "a ChatGPT subscription, signed in with `holaclaw login codex`",
		name: "codex", typ: "codex", auth: "subscription",
		model: "gpt-6.1-sol", context: 128000},
	{flag: "openrouter", desc: "OpenRouter, with an API key",
		name: "openrouter", typ: "openai-compatible", baseURL: "https://openrouter.ai/api/v1", auth: "api-key", sort: "throughput",
		model: "deepseek/deepseek-v4.1-flash", context: 65536},
	{flag: "openai-compat", desc: "any other OpenAI-compatible endpoint: Ollama, llama-server, LM Studio, vLLM…",
		name: "openai-compat", typ: "openai-compatible", auth: "api-key",
		context: 65536},
}

// presetOf returns the preset of a --provider value.
func presetOf(flag string) (initPreset, bool) {
	for _, p := range initPresets {
		if p.flag == flag {
			return p, true
		}
	}
	return initPreset{}, false
}

// presetFlags is the list of --provider values, for messages.
func presetFlags() string {
	out := make([]string, len(initPresets))
	for i, p := range initPresets {
		out[i] = p.flag
	}
	return strings.Join(out, "|")
}

// initOptions are the flags of `init`.
type initOptions struct {
	provider, model, baseURL, tokenFile string
	nonInteractive                      bool
}

func newInitCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "write a config for one provider, and create the owner",
		Args:  cobra.NoArgs,
	}
	o := &initOptions{}
	cmd.Flags().StringVar(&o.provider, "provider", "", "the provider to write a config for: "+presetFlags())
	cmd.Flags().StringVar(&o.model, "model", "", "the model id, instead of the provider's default")
	cmd.Flags().StringVar(&o.baseURL, "base-url", "", "the endpoint of --provider openai-compat, e.g. http://localhost:11434/v1")
	cmd.Flags().BoolVar(&o.nonInteractive, "non-interactive", false, "never prompt, even at a terminal (without one on stdin init never prompts)")
	cmd.Flags().StringVar(&o.tokenFile, "token-file", "", "also create an init binding with one admin token, and write the token to this new file, mode 0600")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmdInit(cmd.Context(), c, *o) }
	return cmd
}

// stdinIsTerminal and echoOff stand for the terminal, so tests can replace
// them.
var (
	stdinIsTerminal = isTerminalReader
	echoOff         = terminalEchoOff
)

// initSetup is the provider config init writes: everything is asked for
// before anything is written.
type initSetup struct {
	preset         initPreset
	model, baseURL string
	key            string
	keyless        bool // openai-compat without a key: no auth
	server         config.Server
	replace        []byte // the empty config.toml it replaces; nil = none
	secrets        string // auth.toml
}

// cmdInit is the first-run command and the only way to create the owner
// (docs/identity.md §2). When the config directory has no config.toml, or
// only the empty one a container starts with, it writes a config for one
// provider there (config.toml and agents/main) and stores that provider's
// API key; any other config it keeps. It creates the owner with holaclaw:cli
// on the database directly, so it needs no daemon; a daemon running on the
// same database sees the owner on its next request. With --token-file it also
// creates an `init` binding with one admin token. Devices pair later, with a
// holaclaw invite.
//
// It is all or nothing. The key, the token and the config are written inside
// the bootstrap transaction, config.toml last, and a failure rolls the owner
// back and takes back the files it wrote, so init can simply be run again.
// Once an owner exists it writes nothing and fails. The token goes to
// --token-file, a new file of mode 0600, and never to stdout; prompts and
// everything else go to stderr.
func cmdInit(ctx context.Context, c *cli, o initOptions) error {
	cfgDir, err := fsutil.Abs(c.configDir)
	if err != nil {
		return err
	}
	tokenPath := ""
	if o.tokenFile != "" {
		if tokenPath, err = fsutil.Abs(o.tokenFile); err != nil {
			return err
		}
		if _, err := os.Lstat(tokenPath); err == nil {
			return fmt.Errorf("write the token: %s already exists", tokenPath)
		}
	}

	var server config.Server
	keep, empty := false, false
	body, err := os.ReadFile(filepath.Join(cfgDir, config.FileConfig))
	switch {
	case err == nil:
		if server, err = parseServerDirs(body); err != nil {
			return err
		}
		if empty, err = emptyConfig(cfgDir, body); err != nil {
			return err
		}
		keep = !empty
	case errors.Is(err, os.ErrNotExist):
		server = config.Server{DataDir: initDataDirName, SecretsDir: filepath.Join(cfgDir, initSecretsDirName)}
	default:
		return err
	}
	dataDir := app.DataDir(cfgDir, server.DataDir)
	if err := ownerExists(ctx, dataDir); err != nil {
		return err
	}
	var setup *initSetup
	if keep {
		if o.provider != "" || o.model != "" || o.baseURL != "" {
			_, _ = fmt.Fprintf(c.stderr, "warning: init keeps the config in %s; --provider, --model and --base-url are ignored\n", cfgDir)
		}
	} else {
		setup = &initSetup{server: server, secrets: app.SecretsPath(server.SecretsDir)}
		if empty {
			setup.replace = body
		}
		if err := askSetup(c, o, setup); err != nil {
			return err
		}
	}

	// Everything init creates, the database included, is recorded so a
	// failure can take it back.
	var created, fresh []string
	restore := func() {}
	undo := func() {
		restore()
		for _, p := range fresh {
			_ = os.RemoveAll(p)
		}
		removeCreated(created)
	}
	dirs, err := fsutil.MkdirAll(dataDir, 0o700)
	created = append(created, dirs...)
	if err != nil {
		undo()
		return err
	}
	fresh = fsutil.Missing(dataDir, "holaclaw.db", "holaclaw.db-wal", "holaclaw.db-shm", "blobs")
	st, err := store.Open(ctx, dataDir)
	if err != nil {
		undo()
		return err
	}

	svc := auth.New(st)
	svc.Events = st // no daemon here: identity.created goes straight to the event log
	boot, err := svc.CreateOwner(ctx, tokenPath != "", func(b auth.Bootstrapped) error {
		if setup != nil && setup.key != "" {
			if err := storeKey(setup, &created, &fresh); err != nil {
				return err
			}
		}
		if setup != nil {
			if err := writeProviderConfig(cfgDir, setup, &created, &restore); err != nil {
				return err
			}
			if _, err := app.Static(cfgDir); err != nil {
				return fmt.Errorf("the config init wrote does not load: %w", err)
			}
		}
		if tokenPath != "" {
			return writeToken(tokenPath, b.Secret, &created)
		}
		return nil
	})
	_ = st.Close()
	if err != nil {
		undo()
		if errors.Is(err, auth.ErrInitialised) {
			return errInitialised
		}
		return err
	}
	if setup != nil {
		reportSetup(c, cfgDir, setup)
	} else if _, err := app.Static(cfgDir); err != nil {
		_, _ = fmt.Fprintf(c.stderr, "warning: the config in %s is not ready for `holaclaw run`: %v\n", cfgDir, err)
	}
	_, _ = fmt.Fprintln(c.stderr, "holaclaw is initialised.")
	if tokenPath != "" {
		_, _ = fmt.Fprintf(c.stderr, "The owner's admin token is in %s; pass it as $HOLACLAW_TOKEN to reach the daemon over TCP.\n", tokenPath)
	}
	_, _ = fmt.Fprintf(c.stderr, "Unless a service or the container runs it, start the daemon with `holaclaw run --config %s`.\n", cfgDir)
	if setup != nil && setup.preset.auth == "subscription" {
		_, _ = fmt.Fprintln(c.stderr, "Once the daemon runs, sign in to ChatGPT with `holaclaw login codex`.")
	}
	_, _ = fmt.Fprintf(c.stderr, "Once it runs, pair a device in the apps with `holaclaw invite create --identity %s --channel holaclaw`.\n", boot.Owner.ID)
	return nil
}

// ownerExists fails with errInitialised when the database in dataDir already
// has an owner, so a second init asks nothing. CreateOwner still decides.
func ownerExists(ctx context.Context, dataDir string) error {
	if _, err := os.Stat(filepath.Join(dataDir, "holaclaw.db")); err != nil {
		return nil
	}
	st, err := store.Open(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if _, err := auth.New(st).Owner(ctx); err == nil {
		return errInitialised
	}
	return nil
}

// emptyConfig reports whether body, the config.toml of dir, is the empty
// config a container starts with: no provider, no model and no agents
// directory.
func emptyConfig(dir string, body []byte) (bool, error) {
	var v struct {
		Providers map[string]any `toml:"providers"`
		Models    map[string]any `toml:"models"`
	}
	if err := toml.Unmarshal(body, &v); err != nil {
		return false, fmt.Errorf("%s: %w", config.FileConfig, err)
	}
	if len(v.Providers) > 0 || len(v.Models) > 0 {
		return false, nil
	}
	_, err := os.Lstat(filepath.Join(dir, "agents"))
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return false, err
}

// askSetup fills s from the flags and, when init is interactive, from
// prompts. Without a terminal on stdin init is never interactive: a missing
// flag is an error, and stdin is the API key.
func askSetup(c *cli, o initOptions, s *initSetup) error {
	interactive := !o.nonInteractive && stdinIsTerminal(c.stdin)
	in := bufio.NewReader(c.stdin)
	ask := func(prompt string) (string, error) {
		_, _ = fmt.Fprint(c.stderr, prompt)
		line, err := in.ReadString('\n')
		if line == "" && err != nil {
			return "", fmt.Errorf("init: no answer: %w", err)
		}
		return strings.TrimSpace(line), nil
	}

	flag := o.provider
	if flag == "" && !interactive {
		return fmt.Errorf("init: pass --provider %s", presetFlags())
	}
	for flag == "" {
		_, _ = fmt.Fprintln(c.stderr, "Which provider?")
		for i, p := range initPresets {
			_, _ = fmt.Fprintf(c.stderr, "  %d) %-14s %s\n", i+1, p.flag, p.desc)
		}
		a, err := ask(fmt.Sprintf("Provider [1-%d]: ", len(initPresets)))
		if err != nil {
			return err
		}
		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(initPresets) {
			flag = initPresets[n-1].flag
		} else if _, ok := presetOf(a); ok {
			flag = a
		}
	}
	p, ok := presetOf(flag)
	if !ok {
		return fmt.Errorf("init: unknown provider %q: pass --provider %s", flag, presetFlags())
	}
	s.preset = p

	s.baseURL = p.baseURL
	if p.flag == "openai-compat" {
		s.baseURL = o.baseURL
		switch {
		case !interactive && s.baseURL == "":
			return errors.New("init: --provider openai-compat needs --base-url, e.g. http://localhost:11434/v1, and --model")
		case !interactive && o.model == "":
			return errors.New("init: --provider openai-compat needs --model, the model id the endpoint serves")
		}
		for !httpURL(s.baseURL) {
			if !interactive {
				return fmt.Errorf("init: --base-url %q is not an http or https URL", s.baseURL)
			}
			var err error
			if s.baseURL, err = ask("Base URL, e.g. http://localhost:11434/v1: "); err != nil {
				return err
			}
		}
	} else if o.baseURL != "" {
		return errors.New("init: --base-url is only for --provider openai-compat")
	}

	s.model = o.model
	if s.model == "" && !interactive {
		s.model = p.model
	}
	for s.model == "" {
		prompt := "Model id: "
		if p.model != "" {
			prompt = fmt.Sprintf("Model id [%s]: ", p.model)
		}
		a, err := ask(prompt)
		if err != nil {
			return err
		}
		s.model = cmpOr(a, p.model)
	}

	if p.auth != "api-key" {
		return nil
	}
	key, err := readKey(c, in, interactive, p)
	if err != nil {
		return err
	}
	s.key = key
	s.keyless = key == "" && p.flag == "openai-compat"
	return nil
}

// readKey reads the provider's API key: at a terminal from a prompt without
// echo, otherwise all of stdin. A --non-interactive at a terminal reads
// nothing.
func readKey(c *cli, in *bufio.Reader, interactive bool, p initPreset) (string, error) {
	if !interactive {
		if stdinIsTerminal(c.stdin) {
			return "", nil
		}
		b, err := io.ReadAll(in)
		if err != nil {
			return "", fmt.Errorf("init: reading the API key from stdin: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	prompt := fmt.Sprintf("API key for %s (not shown; empty to store it later): ", p.flag)
	if p.flag == "openai-compat" {
		prompt = "API key (not shown; empty for none): "
	}
	_, _ = fmt.Fprint(c.stderr, prompt)
	restore, err := echoOff(c.stdin)
	if err != nil {
		return "", fmt.Errorf("init: turning the terminal echo off: %w", err)
	}
	line, rerr := in.ReadString('\n')
	restore()
	_, _ = fmt.Fprintln(c.stderr)
	if line == "" && rerr != nil {
		return "", fmt.Errorf("init: no answer: %w", rerr)
	}
	return strings.TrimSpace(line), nil
}

// terminalEchoOff is termutil.EchoOff for stdin; a reader that is not a file
// has no echo to turn off.
func terminalEchoOff(r io.Reader) (func(), error) {
	f, ok := r.(interface{ Fd() uintptr })
	if !ok {
		return func() {}, nil
	}
	return termutil.EchoOff(int(f.Fd()))
}

// httpURL reports whether s is an absolute http or https URL.
func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func cmpOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// storeKey writes the API key to auth.toml, recording what it creates so a
// failed init can take it back.
func storeKey(s *initSetup, created, fresh *[]string) error {
	dir := filepath.Dir(s.secrets)
	dirs, err := fsutil.MkdirAll(dir, 0o700)
	*created = append(*created, dirs...)
	if err != nil {
		return err
	}
	*fresh = append(*fresh, fsutil.Missing(dir, filepath.Base(s.secrets), filepath.Base(s.secrets)+".lock")...)
	file, err := secrets.Open(s.secrets)
	if err != nil {
		return err
	}
	return file.Set(secrets.Providers, s.preset.name, map[string]string{"api_key": s.key})
}

// reportSetup says on stderr what init wrote, and what is left to do.
func reportSetup(c *cli, cfgDir string, s *initSetup) {
	_, _ = fmt.Fprintf(c.stderr, "Wrote a config for %s, model %s, to %s.\n", s.preset.flag, s.model, cfgDir)
	switch {
	case s.preset.auth != "api-key" || s.keyless:
	case s.key != "":
		_, _ = fmt.Fprintf(c.stderr, "Stored the API key in %s.\n", s.secrets)
	default:
		_, _ = fmt.Fprintf(c.stderr, "Store the API key with: printf %%s \"$KEY\" | holaclaw auth set providers.%s api_key\n", s.preset.name)
	}
}

// writeToken writes secret to a new file at path, mode 0600, and records it
// in created. An existing file is an error: init never overwrites a token.
func writeToken(path, secret string, created *[]string) error {
	if err := fsutil.CreateFile(path, []byte(secret+"\n"), 0o600); err != nil {
		return fmt.Errorf("write the token: %w", err)
	}
	*created = append(*created, path)
	return nil
}

// providerConfig renders the config.toml of s: its server directories, one
// provider and one model.
func providerConfig(s *initSetup) string {
	var b strings.Builder
	kv := func(k, v string) { _, _ = fmt.Fprintf(&b, "%-8s = %s\n", k, tomlString(v)) }
	_, _ = fmt.Fprintf(&b, "# holaclaw config for %s, written by `holaclaw init`. Validate with `holaclaw check`.\n", s.preset.flag)
	b.WriteString("version = 1\n")
	if s.server.DataDir != "" || s.server.SecretsDir != "" {
		b.WriteString("\n[server]\n")
		if s.server.DataDir != "" {
			_, _ = fmt.Fprintf(&b, "data_dir    = %s\n", tomlString(s.server.DataDir))
		}
		if s.server.SecretsDir != "" {
			_, _ = fmt.Fprintf(&b, "secrets_dir = %s\n", tomlString(s.server.SecretsDir))
		}
	}
	_, _ = fmt.Fprintf(&b, "\n[providers.%s]\n", s.preset.name)
	kv("type", s.preset.typ)
	if s.baseURL != "" {
		kv("base_url", s.baseURL)
	}
	if !s.keyless {
		kv("auth", s.preset.auth)
	}
	if s.preset.sort != "" {
		kv("sort", s.preset.sort)
	}
	_, _ = fmt.Fprintf(&b, "\n[models.%s]\n", initModelName)
	kv("provider", s.preset.name)
	kv("id", s.model)
	_, _ = fmt.Fprintf(&b, "%-8s = %d\n", "context", s.preset.context)
	_, _ = fmt.Fprintf(&b, "%-8s = true\n", "tools")
	return b.String()
}

// tomlString is s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			_, _ = fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// writeProviderConfig writes agents/main and then config.toml into dir.
// Files already there are kept, except the empty config.toml s replaces;
// restore is set to put that one back. Every file and directory it creates
// is appended to created, in order, so a failed init can take them back.
func writeProviderConfig(dir string, s *initSetup, created *[]string, restore *func()) error {
	agentDir := filepath.Join(dir, "agents", "main")
	dirs, err := fsutil.MkdirAll(agentDir, 0o700)
	*created = append(*created, dirs...)
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	for _, name := range []string{config.FileAgent, config.FilePrompt} {
		body, err := example.FS.ReadFile("agents/main/" + name)
		if err != nil {
			return fmt.Errorf("write config: %w", err)
		}
		if name == config.FileAgent {
			const chat = `chat = "local"`
			if !bytes.Contains(body, []byte(chat)) {
				return errors.New("write config: the example agent has no chat role to replace")
			}
			body = bytes.Replace(body, []byte(chat), []byte(`chat = "`+initModelName+`"`), 1)
		}
		if err := writeNew(filepath.Join(agentDir, name), body, created); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
	}

	// config.toml last: a container's entrypoint starts the daemon once it
	// has a provider.
	dst := filepath.Join(dir, config.FileConfig)
	body := []byte(providerConfig(s))
	if s.replace == nil {
		if err := writeNew(dst, body, created); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
		return nil
	}
	old := s.replace
	if err := fsutil.ReplaceFile(dst, body, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	*restore = func() { _ = fsutil.ReplaceFile(dst, old, 0o600) }
	return nil
}

// writeNew writes body to a new file at path, mode 0600, and records it in
// created. A file already there is kept as it is.
func writeNew(path string, body []byte, created *[]string) error {
	err := fsutil.CreateFile(path, body, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	*created = append(*created, path)
	return nil
}

// removeCreated takes back what a failed init created, newest first.
func removeCreated(created []string) {
	for i := len(created) - 1; i >= 0; i-- {
		_ = os.Remove(created[i])
	}
}

// serverDirs reads the two [server] directories of config.toml and nothing
// else.
func serverDirs(dir string) (config.Server, error) {
	body, err := os.ReadFile(filepath.Join(dir, config.FileConfig))
	if err != nil {
		return config.Server{}, err
	}
	return parseServerDirs(body)
}

// parseServerDirs is serverDirs on the bytes of a config.toml. A mistake
// elsewhere in the config is for `holaclaw check` and `holaclaw run` to
// report, and must not stop init creating the owner.
func parseServerDirs(body []byte) (config.Server, error) {
	var v struct {
		Server struct {
			DataDir    string `toml:"data_dir"`
			SecretsDir string `toml:"secrets_dir"`
		} `toml:"server"`
	}
	if err := toml.Unmarshal(body, &v); err != nil {
		return config.Server{}, fmt.Errorf("%s: %w", config.FileConfig, err)
	}
	return config.Server{DataDir: v.Server.DataDir, SecretsDir: v.Server.SecretsDir}, nil
}
