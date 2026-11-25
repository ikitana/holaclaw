package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/connector"
	"holaclaw/internal/connector/oauth"
)

// maxClientSecret caps --client-secret-stdin.
const maxClientSecret = 4 << 10

// callbackRedirects are the addresses --listen tries, in order. Tests
// replace them with 127.0.0.1:0 ones.
var callbackRedirects = oauth.PinnedRedirects

// openURL opens a URL in the person's browser, best effort. Tests replace it.
var openURL = func(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, u)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// newConnectorsCmd is the connectors group (docs/connectors.md §8), through
// the API like skills.
func newConnectorsCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connectors",
		Short: "connect remote MCP servers to holaclaw",
		Args:  subcommands,
	}
	sub := func(use, short string, args cobra.PositionalArgs) (*cobra.Command, *conn) {
		s := &cobra.Command{Use: use, Short: short, Args: args}
		return s, c.connFlags(s)
	}

	create, createConn := sub("create <id> <url>", "connect a remote MCP server", cobra.ExactArgs(2))
	create.Long = "connect a remote MCP server\n\n" +
		"a server that needs a sign-in answers with an authorization URL. By default the command prints it\n" +
		"and returns: open it in any browser, sign in, and pass the address the browser lands on to\n" +
		"`holaclaw connectors code`. With --listen the command receives that redirect itself on 127.0.0.1,\n" +
		"ports 26500 to 26504, and waits until the connector is connected"
	var co connectOptions
	cf := create.Flags()
	auth := cf.String("auth", "", "none or oauth; what discovery finds when omitted")
	clientID := cf.String("client-id", "", "a client id registered with the authorization server")
	insecure := cf.Bool("allow-insecure-transport", false, "reach the server over plain http on any host, or https with a certificate that does not verify")
	co.flags(create, "every scope the server offers")
	create.RunE = func(cmd *cobra.Command, args []string) error {
		if *auth != "" && *auth != "none" && *auth != "oauth" {
			return usagef(cmd, "--auth must be none or oauth")
		}
		in := gen.ConnectorCreate{Id: args[0], Url: args[1], Scopes: co.scopeList(cmd)}
		if *auth != "" {
			a := gen.ConnectorCreateAuth(*auth)
			in.Auth = &a
		}
		if *clientID != "" {
			in.ClientId = clientID
		}
		if *insecure {
			in.AllowInsecureTransport = insecure
		}
		secret, err := co.secret(c, cmd)
		if err != nil {
			return err
		}
		in.ClientSecret = secret
		return connectorConnect(cmd.Context(), c, createConn, args[0], co.listen,
			func(ctx context.Context, client *api.Client, redirect string) (gen.ConnectorStarted, error) {
				in.RedirectUri = &redirect
				return client.CreateConnector(ctx, in)
			})
	}

	reconnect, reconnectConn := sub("reconnect <id>", "sign in to a connector again, optionally with other scopes", cobra.ExactArgs(1))
	reconnect.Long = "sign in to a connector again, optionally with other scopes\n\n" +
		"the old sign-in serves until the new one is done. The modes are those of create"
	var ro connectOptions
	ro.flags(reconnect, "the connector's scopes")
	reconnect.RunE = func(cmd *cobra.Command, args []string) error {
		in := gen.ConnectorReconnect{Scopes: ro.scopeList(cmd)}
		secret, err := ro.secret(c, cmd)
		if err != nil {
			return err
		}
		in.ClientSecret = secret
		return connectorConnect(cmd.Context(), c, reconnectConn, args[0], ro.listen,
			func(ctx context.Context, client *api.Client, redirect string) (gen.ConnectorStarted, error) {
				in.RedirectUri = &redirect
				return client.ReconnectConnector(ctx, args[0], in)
			})
	}

	code, codeConn := sub("code <id> <redirect-url>", "finish a sign-in with the address the browser landed on", cobra.ExactArgs(2))
	code.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdConnectorsCode(cmd.Context(), c, codeConn, args[0], args[1])
	}

	list, listConn := sub("list", "list the connectors and their state", cobra.NoArgs)
	list.RunE = func(cmd *cobra.Command, _ []string) error { return cmdConnectorsList(cmd.Context(), c, listConn) }

	show, showConn := sub("show <id>", "show one connector and its tools", cobra.ExactArgs(1))
	show.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdConnectorsShow(cmd.Context(), c, showConn, args[0])
	}

	inspect, inspectConn := sub("inspect <url>", "ask a remote MCP server what it is, without connecting it", cobra.ExactArgs(1))
	inspect.Long = "ask a remote MCP server what it is, without connecting it\n\n" +
		"an open server shows its name and tools; one that needs a sign-in shows its authorization\n" +
		"servers, the scopes it supports and the client identity a create with the default\n" +
		"redirect would use. Nothing is stored"
	inspect.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdConnectorsInspect(cmd.Context(), c, inspectConn, args[0])
	}

	del, delConn := sub("delete <id>", "delete a connector, its sign-in and its cached tools", cobra.ExactArgs(1))
	yes := del.Flags().Bool("yes", false, "do not ask for confirmation")
	del.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdConnectorsDelete(cmd.Context(), c, delConn, args[0], *yes)
	}

	cmd.AddCommand(create, reconnect, code, list, show, inspect, del)
	return cmd
}

// cmdConnectorsDelete removes a connector in any state. Like backup
// restore it asks for a typed yes unless told --yes, so with no answer on
// stdin it deletes nothing. An id the daemon does not list is asked about
// too: the delete removes an entry written by hand since the start and
// what a failed delete left (its sign-in, its cache row), and with nothing
// left it is the same 404.
func cmdConnectorsDelete(ctx context.Context, c *cli, cn *conn, id string, yes bool) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	if !yes {
		if err := confirmConnectorDelete(ctx, c, client, id); err != nil {
			return err
		}
	}
	if err := client.DeleteConnector(ctx, id); err != nil {
		return connectorError(id, err)
	}
	_, _ = fmt.Fprintf(c.stdout, "%s: deleted\n", printable(id))
	return nil
}

// confirmConnectorDelete asks for a typed yes, with a shorter question
// when the daemon does not list id; nil goes on to the delete.
func confirmConnectorDelete(ctx context.Context, c *cli, client *api.Client, id string) error {
	_, err := client.GetConnector(ctx, id)
	var ce *api.ClientError
	switch {
	case errors.As(err, &ce) && ce.Status == http.StatusNotFound:
		_, _ = fmt.Fprintf(c.stderr, "%s is not listed; whatever is left of it in connectors.toml, auth.toml and the cache is removed.\n",
			printable(id))
	case err != nil:
		return connectorError(id, err)
	default:
		_, _ = fmt.Fprintf(c.stderr, "Delete connector %s?\n"+
			"Its entry in connectors.toml, its sign-in in auth.toml and its cached tools are removed.\n"+
			"Agents that list it lose its tools, and `holaclaw check` reports them until the id is removed.\n",
			printable(id))
	}
	if !confirmed(c, "not deleted") {
		return exitError(1)
	}
	return nil
}

// maxToolDescriptionRunes is how many runes of a tool's description `show`
// and `inspect` print: its first line, cut.
const maxToolDescriptionRunes = 72

// cmdConnectorsList prints one line per connector. Every string the server
// gave is stripped of control characters.
func cmdConnectorsList(ctx context.Context, c *cli, cn *conn) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	cs, err := client.ListConnectors(ctx)
	if err != nil {
		return connectorError("", err)
	}
	if len(cs) == 0 {
		_, _ = fmt.Fprintln(c.stdout, "no connectors")
		return nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSTATE\tSERVER\tTOOLS\tSCOPES\tLAST CHECK\tDETAIL")
	for _, k := range cs {
		check := "-"
		if k.LastCheck != nil {
			check = stamp(*k.LastCheck)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", printable(k.Id), printable(k.State),
			orDash(printable(deref(k.ServerName))), k.ToolCount, orDash(printable(strings.Join(deref(k.GrantedScopes), " "))),
			check, orDash(printable(deref(k.Detail))))
	}
	return w.Flush()
}

// cmdConnectorsShow prints one connector as `key: value` lines, then one
// line per tool with its risk class and the first line of its description.
func cmdConnectorsShow(ctx context.Context, c *cli, cn *conn, id string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	d, err := client.GetConnector(ctx, id)
	if err != nil {
		return connectorError(id, err)
	}
	k := d.Connector
	line := keyLine(c.stdout)
	line("id", k.Id)
	line("url", k.Url)
	state := k.State
	if dt := deref(k.Detail); dt != "" {
		state += " (" + dt + ")"
	}
	line("state", state)
	line("server", strings.TrimSpace(deref(k.ServerName)+" "+deref(k.ServerVersion)))
	line("auth", deref(k.Auth))
	line("scopes requested", strings.Join(deref(k.Scopes), " "))
	line("scopes granted", strings.Join(deref(k.GrantedScopes), " "))
	trusted := "no"
	if d.Trusted {
		trusted = "yes"
	}
	line("trusted", trusted)
	if k.LastCheck != nil {
		line("last check", stamp(*k.LastCheck))
	}
	if am := d.AuthMetadata; am != nil {
		line("resource", deref(am.Resource))
		line("issuer", am.Issuer)
		line("authorization endpoint", deref(am.AuthorizationEndpoint))
		line("token endpoint", deref(am.TokenEndpoint))
		line("registration endpoint", deref(am.RegistrationEndpoint))
		line("scopes offered", strings.Join(deref(am.Scopes), " "))
		line("client identity", deref(am.IdentityKind))
	}
	// The granted client id, else the configured one: a pre-registered
	// connector still authorizing has no grant yet.
	clientID := deref(k.ClientId)
	if am := d.AuthMetadata; am != nil && deref(am.ClientId) != "" {
		clientID = deref(am.ClientId)
	}
	line("client id", clientID)
	return printConnectorTools(c, d.Tools)
}

// cmdConnectorsInspect prints what the server told the daemon without a
// token, as `key: value` lines, then the tools of an open server like show.
func cmdConnectorsInspect(ctx context.Context, c *cli, cn *conn, serverURL string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	in, err := client.InspectConnector(ctx, serverURL)
	if err != nil {
		return connectorError("", err)
	}
	line := keyLine(c.stdout)
	line("url", serverURL)
	if in.Open {
		line("open", "yes, no sign-in needed")
		line("server", strings.TrimSpace(deref(in.ServerName)+" "+deref(in.ServerVersion)))
		return printConnectorTools(c, in.Tools)
	}
	line("open", "no, it needs a sign-in")
	line("authorization servers", strings.Join(deref(in.AuthorizationServers), " "))
	if am := in.AuthMetadata; am != nil {
		line("issuer", am.Issuer)
		line("authorization endpoint", deref(am.AuthorizationEndpoint))
		line("token endpoint", deref(am.TokenEndpoint))
		line("registration endpoint", deref(am.RegistrationEndpoint))
	}
	line("scopes supported", strings.Join(deref(in.ScopesSupported), " "))
	if am := in.AuthMetadata; am != nil {
		line("scopes offered", strings.Join(deref(am.Scopes), " "))
	}
	identity := deref(in.Identity)
	if identity == "pre-registered" {
		identity += " (the server offers no other: create needs --client-id)"
	}
	line("client identity", identity)
	return nil
}

// keyLine prints `key: value`, the value stripped of control characters;
// an empty value prints nothing.
func keyLine(w io.Writer) func(key, v string) {
	return func(key, v string) {
		if v = printable(v); v != "" {
			_, _ = fmt.Fprintf(w, "%s: %s\n", key, v)
		}
	}
}

// printConnectorTools prints one line per tool with its risk class and the
// first line of its description, cut.
func printConnectorTools(c *cli, tools []gen.ConnectorTool) error {
	if len(tools) == 0 {
		_, _ = fmt.Fprintln(c.stdout, "\nno tools")
		return nil
	}
	_, _ = fmt.Fprintf(c.stdout, "\ntools (%d):\n", len(tools))
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	cut := false
	for _, t := range tools {
		risk := t.Risk
		if t.AlwaysAsk {
			risk += ", asks"
		}
		desc := truncate(firstLine(deref(t.Description)), maxToolDescriptionRunes)
		if t.DescriptionTruncated {
			desc += " [cut]"
			cut = true
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", printable(t.Name), printable(risk), desc)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// A tool without a description leaves the risk column's padding.
	for l := range strings.Lines(b.String()) {
		_, _ = fmt.Fprintln(c.stdout, strings.TrimRight(l, " \n"))
	}
	if cut {
		_, _ = fmt.Fprintln(c.stdout, "\n[cut]: the server's description was over 400 tokens; the agent sees it cut")
	}
	return nil
}

// firstLine is the first non-empty line of a tool description, printable.
// A tab is a space and CR, NEL and the line and paragraph separators break
// lines like LF, so printable does not glue the words they part.
func firstLine(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\t':
			return ' '
		case '\r', 0x85, 0x2028, 0x2029:
			return '\n'
		}
		return r
	}, s)
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(printable(l)); l != "" {
			return l
		}
	}
	return ""
}

// orDash is s, or "-" for an empty column.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// connectOptions are the flags create and reconnect share.
type connectOptions struct {
	scopes      []string
	listen      bool
	secretStdin bool
}

func (o *connectOptions) flags(cmd *cobra.Command, scopesDefault string) {
	fs := cmd.Flags()
	fs.StringSliceVar(&o.scopes, "scopes", nil, "scopes to ask for, comma separated; "+scopesDefault+" when omitted")
	fs.BoolVar(&o.listen, "listen", false, "receive the redirect on 127.0.0.1 and wait until connected")
	fs.BoolVar(&o.secretStdin, "client-secret-stdin", false, "read the secret of the pre-registered client from stdin")
}

// scopeList is --scopes, or nil when it was not given.
func (o *connectOptions) scopeList(cmd *cobra.Command) *[]string {
	if !cmd.Flags().Changed("scopes") {
		return nil
	}
	return &o.scopes
}

// secret reads --client-secret-stdin: the secret never lands in a shell
// history or in "ps".
func (o *connectOptions) secret(c *cli, cmd *cobra.Command) (*string, error) {
	if !o.secretStdin {
		return nil, nil
	}
	// A little more than the cap, so a secret of the cap and its newline
	// fits. Input that fills the read is refused before the trim: leading
	// space would otherwise push the end of the secret past it, and a cut
	// secret would be sent.
	const limit = maxClientSecret + 3
	b, err := io.ReadAll(io.LimitReader(c.stdin, limit))
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(b))
	switch {
	case len(b) == limit, len(s) > maxClientSecret:
		return nil, usagef(cmd, "--client-secret-stdin: the secret is over %d bytes", maxClientSecret)
	case s == "":
		return nil, usagef(cmd, "--client-secret-stdin: stdin is empty")
	}
	return &s, nil
}

// startFunc is the create or the reconnect, sent with the redirect the
// browser will be sent to.
type startFunc func(ctx context.Context, client *api.Client, redirect string) (gen.ConnectorStarted, error)

// connectorConnect runs a create or a reconnect in one of the two modes of
// docs/connectors.md §8.
func connectorConnect(ctx context.Context, c *cli, cn *conn, id string, listen bool, start startFunc) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	redirect := oauth.PinnedRedirects()[0]
	var cb *callbackServer
	if listen {
		if cb, err = listenCallback(); err != nil {
			return err
		}
		defer cb.close()
		redirect = cb.redirect
	}
	st, err := start(ctx, client, redirect)
	if err != nil {
		return interrupted(ctx, connectorError(id, err))
	}
	if st.AuthorizationUrl == nil {
		return connectorSettled(c, id, st.Connector)
	}
	authURL := printable(*st.AuthorizationUrl)
	// expires_at is the daemon's clock and the wait below runs on this one:
	// with --addr and a skewed clock the wait ends early or late.
	expires := time.Now().Add(connector.IntentTTL)
	if st.ExpiresAt != nil {
		expires = *st.ExpiresAt
	}
	if !listen {
		// The time is only ours: the server's code has a lifetime of its
		// own, counted from the sign-in, and the redirect does not carry it.
		_, _ = fmt.Fprintf(c.stdout, "Open this URL in a browser and sign in:\n\n    %s\n\n"+
			"The browser then goes to %s, which does not load. Copy the whole address\n"+
			"from the address bar and run, right after signing in:\n\n    holaclaw connectors code %s '<address>'\n\n"+
			"holaclaw takes the address until %s. The server may stop taking its code sooner.\n",
			authURL, redirect, id, expires.Local().Format("15:04:05"))
		return nil
	}

	// Only the redirect of this URL is taken.
	if err := cb.expect(*st.AuthorizationUrl); err != nil {
		return fmt.Errorf("%s: %w", id, err)
	}
	_, _ = fmt.Fprintf(c.stdout, "Open this URL in a browser and sign in:\n\n    %s\n\n", authURL)
	if openURL(authURL) == nil {
		_, _ = fmt.Fprintln(c.stderr, "opened the browser")
	}
	_, _ = fmt.Fprintf(c.stderr, "waiting for the browser on %s… (Ctrl-C stops waiting)\n", redirect)

	q, err := cb.wait(ctx, expires)
	if err != nil {
		return err
	}
	in, err := codeFromQuery(id, q)
	if err != nil {
		return err
	}
	return postCode(ctx, c, client, id, in)
}

// postCode finishes the sign-in. The daemon answers once the connector has
// settled, so its answer is the result.
func postCode(ctx context.Context, c *cli, client *api.Client, id string, in gen.ConnectorCode) error {
	got, err := client.PostConnectorCode(ctx, id, in)
	if err != nil {
		return interrupted(ctx, codeError(id, err))
	}
	return connectorSettled(c, id, got)
}

// codeError is connectorError for a code post. With no authorization open
// the daemon says why, and the way on is a new one.
func codeError(id string, err error) error {
	out := connectorError(id, err)
	var ce *api.ClientError
	if errors.As(err, &ce) && ce.Status == http.StatusConflict && strings.HasPrefix(ce.Message, connector.ErrNoIntent.Error()) {
		return fmt.Errorf("%w\ntry again with `holaclaw connectors reconnect %s`", out, id)
	}
	return out
}

// cmdConnectorsCode posts the code and state of the address the browser
// landed on.
func cmdConnectorsCode(ctx context.Context, c *cli, cn *conn, id, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.RawQuery == "" {
		return fmt.Errorf("%s: not the address the browser landed on: it should end in /callback?code=…&state=…", id)
	}
	in, err := codeFromQuery(id, u.Query())
	if err != nil {
		return err
	}
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	return postCode(ctx, c, client, id, in)
}

// codeFromQuery is the body of the code post. A redirect with error= is
// the authorization server's refusal: nothing is posted, and the open
// authorization expires on its own.
func codeFromQuery(id string, q url.Values) (gen.ConnectorCode, error) {
	if e := q.Get("error"); e != "" {
		msg := printable(e)
		if d := q.Get("error_description"); d != "" {
			msg += ": " + printable(d)
		}
		return gen.ConnectorCode{}, fmt.Errorf("%s: the authorization server refused: %s\n"+
			"try again with `holaclaw connectors reconnect %s`", id, msg, id)
	}
	code, state := q.Get("code"), q.Get("state")
	switch {
	case code == "":
		return gen.ConnectorCode{}, fmt.Errorf("%s: the address has no code: copy all of it from the address bar", id)
	case state == "":
		return gen.ConnectorCode{}, fmt.Errorf("%s: the address has no state: copy all of it from the address bar", id)
	}
	in := gen.ConnectorCode{Code: code, State: state}
	if iss := q.Get("iss"); iss != "" {
		in.Iss = &iss
	}
	return in, nil
}

// connectorSettled reports where a connector ended up: connected is
// success and names the server and the granted scopes, so a sign-in with
// the wrong account shows; anything else fails with why.
func connectorSettled(c *cli, id string, got gen.Connector) error {
	if got.State != "connected" {
		if d := deref(got.Detail); d != "" {
			return fmt.Errorf("%s: %s (%s)", id, printable(got.State), printable(d))
		}
		return fmt.Errorf("%s: %s", id, printable(got.State))
	}
	line := id + ": connected"
	if n := deref(got.ServerName); n != "" {
		line += " to " + printable(n)
	}
	if s := deref(got.GrantedScopes); len(s) > 0 {
		line += ", scopes " + printable(strings.Join(s, " "))
	}
	_, _ = fmt.Fprintln(c.stdout, line)
	return nil
}

// connectorError says what the daemon refused, in its words. id is ""
// for the list, which names no connector.
func connectorError(id string, err error) error {
	var ce *api.ClientError
	if !errors.As(err, &ce) {
		return err
	}
	prefix := id
	switch {
	case ce.Status == http.StatusNotFound && id != "":
		return fmt.Errorf("no connector named %q on this daemon", id)
	case ce.Status == http.StatusNotImplemented:
		return errors.New(printable(ce.Message))
	case id == "":
		prefix = "connectors"
	}
	return fmt.Errorf("%s: %s", prefix, printable(strings.TrimPrefix(ce.Message, "connector: ")))
}

// callbackServer is the loopback listener of --listen: it takes the first
// redirect to its callback path that carries the expected state, and hands
// its query over. Anything else is refused and leaves it waiting.
type callbackServer struct {
	redirect string
	srv      *http.Server
	ln       net.Listener
	got      chan url.Values

	mu    sync.Mutex
	state string // of the authorization URL; "" until expect
	taken bool   // a redirect with the state came; got holds or held it
}

// listenCallback binds the first free address of callbackRedirects: the
// pinned ports on 127.0.0.1. There is no port option: the client metadata
// document fixes them.
func listenCallback() (*callbackServer, error) {
	for _, r := range callbackRedirects() {
		u, err := url.Parse(r)
		if err != nil {
			continue
		}
		ln, err := net.Listen("tcp", u.Host)
		if err != nil {
			continue
		}
		cb := &callbackServer{redirect: "http://" + ln.Addr().String() + u.Path, ln: ln, got: make(chan url.Values, 1)}
		mux := http.NewServeMux()
		mux.HandleFunc("GET "+u.Path, cb.serve)
		cb.srv = &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       10 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		go func() { _ = cb.srv.Serve(ln) }()
		return cb, nil
	}
	return nil, errors.New("--listen: ports 26500 to 26504 on 127.0.0.1 are all in use; free one, or run without --listen")
}

// expect takes the state of the authorization URL the person opens.
func (cb *callbackServer) expect(authURL string) error {
	u, err := url.Parse(authURL)
	if err != nil || u.Query().Get("state") == "" {
		return errors.New("the authorization URL has no state")
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = u.Query().Get("state")
	return nil
}

func (cb *callbackServer) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if q.Get("code") == "" && q.Get("error") == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "This is not a sign-in redirect.\n")
		return
	}
	cb.mu.Lock()
	want, taken := cb.state, cb.taken
	ok := want != "" && subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(want)) == 1
	if ok {
		cb.taken = true
	}
	cb.mu.Unlock()
	switch {
	case !ok:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "This is not the sign-in holaclaw is waiting for.\n")
		return
	case taken:
		_, _ = io.WriteString(w, "holaclaw already got a sign-in. You can close this tab.\n")
		return
	case q.Get("error") != "":
		_, _ = io.WriteString(w, "The sign-in was refused. Go back to the terminal.\n")
	default:
		_, _ = io.WriteString(w, "holaclaw got the sign-in. You can close this tab and go back to the terminal.\n")
	}
	// The page is out before the query is handed over: the command may
	// return, and close the server, at once.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	cb.got <- q
}

// wait returns the redirect's query, or an error once the authorization
// expires or ctx ends.
func (cb *callbackServer) wait(ctx context.Context, expires time.Time) (url.Values, error) {
	t := time.NewTimer(time.Until(expires))
	defer t.Stop()
	select {
	case q := <-cb.got:
		return q, nil
	case <-t.C:
		return nil, errors.New("the authorization expired before the browser came back; run the command again")
	case <-ctx.Done():
		return nil, interrupted(ctx, ctx.Err())
	}
}

// close lets a page being written finish, for up to a second, then drops
// what is left. The listener is closed here too: Shutdown closes only those
// Serve has taken up, and the goroutine running it may not have started.
func (cb *callbackServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if cb.srv.Shutdown(ctx) != nil {
		_ = cb.srv.Close()
	}
	_ = cb.ln.Close()
}
