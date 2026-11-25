package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
)

// The admin group: tokens, identities, invites and approvals, all through the
// Control API. The CLI is just a client (ARCHITECTURE.md §2).

func newTokenCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "manage the API tokens"}

	create := &cobra.Command{
		Use:   "create (--identity <id> | --binding holaclaw:<alias>) --name <name>",
		Short: "create an API token; the secret is shown once",
		Args:  cobra.NoArgs,
	}
	cn := c.connFlags(create)
	name := create.Flags().String("name", "", "token name, also the name of a new binding")
	scope := create.Flags().String("scope", "", "client | admin (default client); admin needs the role owner or admin")
	identity := create.Flags().String("identity", "", "identity the token acts as, through a new holaclaw binding")
	binding := create.Flags().String("binding", "", "existing holaclaw:<alias> binding to add the token to")
	required(create, "name")
	create.MarkFlagsOneRequired("identity", "binding")
	create.MarkFlagsMutuallyExclusive("identity", "binding")
	create.RunE = func(cmd *cobra.Command, _ []string) error {
		in := gen.TokenCreate{Name: *name}
		if *scope != "" {
			in.Scope = ptrOf(gen.TokenCreateScope(*scope))
		}
		if *identity != "" {
			in.UserId = identity
		}
		if *binding != "" {
			in.Binding = binding
		}
		return cmdTokenCreate(cmd.Context(), c, cn, in)
	}

	list := &cobra.Command{Use: "list", Short: "list the active API tokens", Args: cobra.NoArgs}
	listConn := c.connFlags(list)
	var params gen.ListTokensParams
	params.Identity = list.Flags().String("identity", "", "only the tokens of this identity")
	params.Binding = list.Flags().String("binding", "", "only the tokens of this holaclaw:<alias> binding")
	params.All = list.Flags().Bool("all", false, "include revoked tokens")
	list.RunE = func(cmd *cobra.Command, _ []string) error { return cmdTokenList(cmd.Context(), c, listConn, params) }

	set := &cobra.Command{Use: "set <id> --scope client|admin", Short: "raise or lower a token's scope", Args: cobra.ExactArgs(1)}
	setConn := c.connFlags(set)
	setScope := set.Flags().String("scope", "", "client | admin; admin needs the role owner or admin")
	required(set, "scope")
	set.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdTokenSet(cmd.Context(), c, setConn, args[0], *setScope)
	}

	rotate := &cobra.Command{
		Use:   "rotate <id>",
		Short: "replace a token with a new secret on the same binding; the old one is revoked",
		Args:  cobra.ExactArgs(1),
	}
	rotateConn := c.connFlags(rotate)
	rotate.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdTokenRotate(cmd.Context(), c, rotateConn, args[0])
	}

	revoke := &cobra.Command{Use: "revoke <id>", Short: "revoke an API token; its binding stays", Args: cobra.ExactArgs(1)}
	revokeConn := c.connFlags(revoke)
	revoke.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdTokenRevoke(cmd.Context(), c, revokeConn, args[0])
	}

	cmd.AddCommand(create, list, set, rotate, revoke)
	return cmd
}

// cmdTokenCreate prints the secret once: the daemon never reveals it again.
func cmdTokenCreate(ctx context.Context, c *cli, cn *conn, in gen.TokenCreate) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	out, err := client.CreateToken(ctx, in)
	if err != nil {
		return err
	}
	printSecret(c, out)
	return nil
}

// printSecret prints a new token with its secret, which is shown once.
func printSecret(c *cli, out gen.TokenCreated) {
	_, _ = fmt.Fprintf(c.stdout, "id      %s\nname    %s\nscope   %s\n",
		out.Token.Id, out.Token.Name, out.Token.Scope)
	// The binding is how `identity unbind` cuts the client off, so it is
	// printed with its identity.
	_, _ = fmt.Fprintf(c.stdout, "binding %s\n", out.Token.Binding)
	if out.Token.UserId != nil && *out.Token.UserId != "" {
		_, _ = fmt.Fprintf(c.stdout, "user    %s\n", *out.Token.UserId)
	}
	_, _ = fmt.Fprintf(c.stdout, "secret  %s\n", out.Secret)
	_, _ = fmt.Fprintln(c.stdout, "\nThe secret is shown once. Store it now.")
}

func cmdTokenList(ctx context.Context, c *cli, cn *conn, params gen.ListTokensParams) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	tokens, err := client.ListTokens(ctx, params)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		_, _ = fmt.Fprintln(c.stdout, "no tokens")
		return nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tBINDING\tNAME\tSCOPE\tORIGIN\tLAST USED\tSTATE")
	for _, t := range tokens {
		state := "active"
		if t.RevokedAt != nil {
			state = "revoked"
		}
		used := "-"
		if t.LastUsedAt != nil {
			used = stamp(*t.LastUsedAt)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.Id, t.Binding, t.Name, t.Scope, tokenOrigin(t), used, state)
	}
	return w.Flush()
}

// tokenOrigin renders how a token was made: pair · invite <id>, token create ·
// <creator>, rotate · <replaced token>, or init.
func tokenOrigin(t gen.Token) string {
	origin := strings.ReplaceAll(string(t.Origin), "_", " ")
	switch {
	case t.Origin == gen.TokenOriginPair && t.OriginRef != nil:
		return origin + " · invite " + *t.OriginRef
	case t.Origin == gen.TokenOriginRotate && t.OriginRef != nil:
		return origin + " · " + *t.OriginRef
	case t.Origin == gen.TokenOriginTokenCreate && t.CreatedBy != nil:
		return origin + " · " + *t.CreatedBy
	}
	return origin
}

func cmdTokenSet(ctx context.Context, c *cli, cn *conn, id, scope string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	t, err := client.UpdateToken(ctx, id, gen.TokenUpdate{Scope: gen.TokenUpdateScope(scope)})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "%s scope %s\n", t.Id, t.Scope)
	return nil
}

// cmdTokenRotate prints the new secret once; the old token stops working.
func cmdTokenRotate(ctx context.Context, c *cli, cn *conn, id string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	out, err := client.RotateToken(ctx, id)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "revoked %s\n\n", id)
	printSecret(c, out)
	return nil
}

func cmdTokenRevoke(ctx context.Context, c *cli, cn *conn, id string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	if err := client.RevokeToken(ctx, id); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "revoked %s\n", id)
	return nil
}

// newIdentityCmd manages the identities and their bindings: create, confirm,
// assign, unbind, disable and delete (docs/identity.md §6).
func newIdentityCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "identity", Short: "manage the identities and their bindings"}

	list := &cobra.Command{Use: "list", Short: "list the identities and their bindings", Args: cobra.NoArgs}
	listConn := c.connFlags(list)
	pending := list.Flags().Bool("pending", false, "only pending identities")
	list.RunE = func(cmd *cobra.Command, _ []string) error {
		return cmdIdentityList(cmd.Context(), c, listConn, *pending)
	}

	show := &cobra.Command{Use: "show <id>", Short: "show one identity and its bindings", Args: cobra.ExactArgs(1)}
	showConn := c.connFlags(show)
	show.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdIdentityShow(cmd.Context(), c, showConn, args[0])
	}

	create := &cobra.Command{
		Use:   "create",
		Short: "create a confirmed identity with no bindings",
		Args:  cobra.NoArgs,
	}
	createConn := c.connFlags(create)
	createName := create.Flags().String("name", "", "what people call this person, e.g. Rafa (free text, not unique)")
	createRole := create.Flags().String("role", "", "role to grant: user | admin (default user)")
	createInvite := create.Flags().String("invite", "", "channel instance to print the first invite for")
	createTTL := create.Flags().Duration("ttl", 0, "how long the first invite stays valid (default 15m, at most 24h)")
	required(create, "name")
	create.RunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("ttl") && *createInvite == "" {
			return usagef(cmd, "--ttl needs --invite")
		}
		return cmdIdentityCreate(cmd.Context(), c, createConn, *createName, *createRole,
			inviteOptions{channel: *createInvite, ttl: *createTTL})
	}

	confirm := &cobra.Command{
		Use:   "confirm <id>",
		Short: "turn a pending identity into a named user",
		Args:  cobra.ExactArgs(1),
	}
	confirmConn := c.connFlags(confirm)
	confirmName := confirm.Flags().String("name", "", "what people call this person, e.g. Rafa (free text, not unique)")
	confirmRole := confirm.Flags().String("role", "", "role to grant: user | admin (default user)")
	required(confirm, "name")
	confirm.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdIdentityConfirm(cmd.Context(), c, confirmConn, args[0], *confirmName, *confirmRole)
	}

	assign := &cobra.Command{
		Use:   "assign <pending> --to <identity>",
		Short: "move a pending identity's bindings onto a confirmed one",
		Args:  cobra.ExactArgs(1),
	}
	assignConn := c.connFlags(assign)
	assignTo := assign.Flags().String("to", "", "the confirmed identity that receives the bindings")
	required(assign, "to")
	assign.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdIdentityAssign(cmd.Context(), c, assignConn, args[0], *assignTo)
	}

	set := &cobra.Command{
		Use:   "set <id>",
		Short: "rename an identity or change its role",
		Args:  cobra.ExactArgs(1),
	}
	setConn := c.connFlags(set)
	setName := set.Flags().String("name", "", "new name: what people call this person (free text, not unique)")
	setRole := set.Flags().String("role", "", "new role: user | admin")
	set.MarkFlagsOneRequired("name", "role")
	set.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdIdentitySet(cmd.Context(), c, setConn, args[0], *setName, *setRole)
	}

	unbind := &cobra.Command{
		Use:   "unbind <channel>:<alias>",
		Short: "remove one binding from the identity that holds it",
		Args:  cobra.ExactArgs(1),
	}
	unbindConn := c.connFlags(unbind)
	unbind.RunE = func(cmd *cobra.Command, args []string) error {
		channel, alias, ok := strings.Cut(args[0], ":")
		if !ok || channel == "" || alias == "" {
			return usage(cmd)
		}
		return cmdIdentityUnbind(cmd.Context(), c, unbindConn, channel, alias)
	}

	disable := &cobra.Command{
		Use:   "disable <id>",
		Short: "switch an identity or a room off: its messages are dropped and its tasks stop",
		Args:  cobra.ExactArgs(1),
	}
	disableConn := c.connFlags(disable)
	disable.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdIdentitySwitch(cmd.Context(), c, disableConn, args[0], false)
	}

	enable := &cobra.Command{Use: "enable <id>", Short: "switch a disabled identity back on; a room comes back pending", Args: cobra.ExactArgs(1)}
	enableConn := c.connFlags(enable)
	enable.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdIdentitySwitch(cmd.Context(), c, enableConn, args[0], true)
	}

	del := &cobra.Command{
		Use:   "delete <id>",
		Short: "delete an identity, forgetting its memory",
		Args:  cobra.ExactArgs(1),
	}
	delConn := c.connFlags(del)
	keepMemory := del.Flags().Bool("keep-memory", false, "keep the identity's own memory (notes and recall) instead of forgetting it")
	del.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdIdentityDelete(cmd.Context(), c, delConn, args[0], *keepMemory)
	}

	cmd.AddCommand(list, show, create, confirm, assign, set, unbind, disable, enable, del)
	return cmd
}

func cmdIdentityList(ctx context.Context, c *cli, cn *conn, pending bool) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	users, err := client.ListIdentities(ctx, pending)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		_, _ = fmt.Fprintln(c.stdout, "no identities")
		return nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tKIND\tSTATUS\tNAME\tROLE\tBINDINGS")
	for _, u := range users {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			u.Id, u.Kind, u.Status, u.Name, roleName(u.Role), identityBindings(u))
	}
	return w.Flush()
}

// cmdIdentityShow prints an identity and every binding as its full
// `<channel>:<alias>` ref, the form `identity unbind` takes (docs/identity.md §3).
func cmdIdentityShow(ctx context.Context, c *cli, cn *conn, id string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	u, err := client.GetIdentity(ctx, id)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "%s · %s · %s · %s\n", u.Name, u.Kind, roleName(u.Role), u.Status)
	_, _ = fmt.Fprintf(c.stdout, "  id %s\n", u.Id)
	stampLine(c, "approved", u.ApprovedAt, u.ApprovedBy)
	if u.Identities == nil {
		return nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	for _, b := range *u.Identities {
		label := ""
		if b.Display != nil && *b.Display != "" {
			label = fmt.Sprintf("%q", *b.Display)
		}
		tokens := ""
		switch {
		case b.Tokens == nil:
		case *b.Tokens == 1:
			tokens = "1 active token"
		default:
			tokens = fmt.Sprintf("%d active tokens", *b.Tokens)
		}
		_, _ = fmt.Fprintf(w, "  %s:%s\t%s\t%s\tsince %s\n", b.Channel, b.Alias, label, tokens, b.CreatedAt.UTC().Format("2006-01-02"))
	}
	return w.Flush()
}

// stampLine prints when something happened to an identity and by whom, when it
// did.
func stampLine(c *cli, what string, at *time.Time, by *string) {
	if at == nil {
		return
	}
	line := "  " + what + " " + stamp(*at)
	if by != nil && *by != "" {
		line += " by " + *by
	}
	_, _ = fmt.Fprintln(c.stdout, line)
}

// cmdIdentityCreate makes a confirmed identity with no bindings and, when
// inv names a channel, prints its first invite there. The server validates the
// name, the role and the invite.
func cmdIdentityCreate(ctx context.Context, c *cli, cn *conn, name, role string, inv inviteOptions) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	u, err := client.CreateIdentity(ctx, name, role)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "id    %s\nname  %s\nrole  %s\n", u.Id, u.Name, roleName(u.Role))
	if inv.channel == "" {
		return nil
	}
	_, _ = fmt.Fprintln(c.stdout)
	inv.identity = u.Id
	if err := createInvite(ctx, c, client, inv); err != nil {
		return fmt.Errorf("%s was created, but its invite was not (holaclaw invite create --identity %s): %w", u.Name, u.Id, err)
	}
	return nil
}

// cmdIdentityConfirm turns a guest into a named user. It checks the role itself before patching: the server treats
// any PATCH on a guest as a confirmation, so a second confirm — or one
// aimed at an identity that was never pending — must be rejected here first.
func cmdIdentityConfirm(ctx context.Context, c *cli, cn *conn, id, name, role string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	u, err := client.GetIdentity(ctx, id)
	if err != nil {
		return err
	}
	if u.Status != gen.UserStatusPending {
		return fmt.Errorf("%s is not pending", id)
	}
	in := gen.IdentityUpdate{Name: &name}
	if role != "" {
		r := gen.IdentityUpdateRole(role)
		in.Role = &r
	}
	out, err := client.UpdateIdentity(ctx, id, in)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "confirmed %s as %s%s\n", out.Id, out.Name, roleSuffix(out.Role))
	return nil
}

// cmdIdentitySet renames an identity or changes its role, but never on a
// still-pending guest: that would silently confirm it, which is `confirm`'s
// job.
func cmdIdentitySet(ctx context.Context, c *cli, cn *conn, id, name, role string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	u, err := client.GetIdentity(ctx, id)
	if err != nil {
		return err
	}
	if u.Status == gen.UserStatusPending {
		return fmt.Errorf("%s is pending: use identity confirm", id)
	}
	in := gen.IdentityUpdate{}
	if name != "" {
		in.Name = &name
	}
	if role != "" {
		r := gen.IdentityUpdateRole(role)
		in.Role = &r
	}
	if _, err := client.UpdateIdentity(ctx, id, in); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "updated %s\n", id)
	if role != string(gen.IdentityUpdateRoleAdmin) || (u.Role != nil && *u.Role == gen.UserRoleAdmin) {
		return nil
	}
	// A promotion raises no token: say which ones stay at client.
	tokens, err := client.ListTokens(ctx, gen.ListTokensParams{Identity: &id})
	if err != nil {
		return fmt.Errorf("%s was promoted, but its tokens could not be listed: %w", id, err)
	}
	var low []string
	for _, t := range tokens {
		if t.Scope == gen.TokenScopeClient {
			low = append(low, t.Id)
		}
	}
	if len(low) > 0 {
		noun := "tokens stay"
		if len(low) == 1 {
			noun = "token stays"
		}
		_, _ = fmt.Fprintf(c.stdout, "  %d %s at client scope; raise them with holaclaw token set <id> --scope admin:\n", len(low), noun)
		for _, t := range low {
			_, _ = fmt.Fprintf(c.stdout, "    %s\n", t)
		}
	}
	return nil
}

// cmdIdentityAssign moves a pending identity onto a confirmed one. The server
// checks both sides; a refusal comes back as its error.
func cmdIdentityAssign(ctx context.Context, c *cli, cn *conn, pending, to string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	a, err := client.AssignIdentity(ctx, pending, to)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "assigned %s to %s (%s): %d bindings, %d memory rows, %d sessions ended\n",
		pending, a.Identity.Id, a.Identity.Name, a.Bindings, a.Memory, a.Sessions)
	return nil
}

// cmdIdentityUnbind removes one binding from whichever identity holds it; the
// ref alone finds it. A confirmed identity keeps its memory and the next message
// from that account is a stranger's; a pending one left with none is deleted.
func cmdIdentityUnbind(ctx context.Context, c *cli, cn *conn, channel, alias string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	u, err := client.Unbind(ctx, channel, alias)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "unbound %s:%s from %s\n", channel, alias, u.Identity)
	if u.PendingDeleted {
		_, _ = fmt.Fprintf(c.stdout, "pending identity deleted, %d memories forgotten\n", u.MemoryForgotten)
	}
	return nil
}

// cmdIdentitySwitch disables an identity, or enables it back.
func cmdIdentitySwitch(ctx context.Context, c *cli, cn *conn, id string, enable bool) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	do, verb := client.DisableIdentity, "disabled"
	if enable {
		do, verb = client.EnableIdentity, "enabled"
	}
	u, err := do(ctx, id)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "%s %s (%s)\n", verb, u.Id, u.Name)
	return nil
}

// cmdIdentityDelete removes an identity: its aliases, tokens and invites go,
// its tasks are cancelled, and unless --keep-memory its own memory is
// forgotten. Like `backup rm` and `token revoke`, it does not ask for
// confirmation.
func cmdIdentityDelete(ctx context.Context, c *cli, cn *conn, id string, keepMemory bool) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	out, err := client.DeleteIdentity(ctx, id, keepMemory)
	if err != nil {
		return err
	}
	if out.MemoryKept {
		_, _ = fmt.Fprintf(c.stdout, "deleted %s%s: %d aliases removed, memory kept\n", out.Id, deletedRole(out.Role), len(out.Aliases))
	} else {
		_, _ = fmt.Fprintf(c.stdout, "deleted %s%s: %d aliases removed, %d memory rows forgotten\n",
			out.Id, deletedRole(out.Role), len(out.Aliases), out.MemoryForgotten)
	}
	return nil
}

// roleName renders an optional role for a table: a room has none.
func roleName(r *gen.UserRole) string {
	if r == nil {
		return "-"
	}
	return string(*r)
}

// roleSuffix renders " (role)" after a name, nothing when there is no role.
func roleSuffix(r *gen.UserRole) string {
	if r == nil {
		return ""
	}
	return " (" + string(*r) + ")"
}

// deletedRole is roleSuffix for a deleted identity.
func deletedRole(r *gen.IdentityDeletedRole) string {
	if r == nil {
		return ""
	}
	return " (" + string(*r) + ")"
}

// identityBindings renders an identity's bindings for the `list` table.
func identityBindings(u gen.User) string {
	if u.Identities == nil || len(*u.Identities) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(*u.Identities))
	for _, id := range *u.Identities {
		parts = append(parts, id.Channel+":"+id.Alias)
	}
	return strings.Join(parts, ",")
}

func newInviteCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "invite", Short: "invite people and rooms"}
	create := &cobra.Command{
		Use:   "create",
		Short: "create a one-shot expiring invite code for an identity or a room",
		Args:  cobra.NoArgs,
	}
	cn := c.connFlags(create)
	var o inviteOptions
	fs := create.Flags()
	fs.StringVar(&o.channel, "channel", "", "the one channel instance the code redeems on; holaclaw pairs a device")
	fs.StringVar(&o.identity, "identity", "",
		`identity id (from "holaclaw identity list") the redeemed account is bound to; it gets that identity's role`)
	fs.StringVar(&o.scope, "scope", "", "holaclaw only: token scope of the paired device, client | admin (default client)")
	fs.BoolVar(&o.room, "room", false,
		"approve a room instead of binding a person; telegram, googlechat and whatsapp only: "+
			"telegram's link adds the bot to a group (a room), Chat has no deep link, so send \"@app <code>\" in the space")
	fs.DurationVar(&o.ttl, "ttl", 0, "how long the code stays valid (default 15m, at most 1h, or 24h for an identity with no bindings)")
	required(create, "channel")
	create.RunE = func(cmd *cobra.Command, _ []string) error {
		switch {
		case o.room && (o.identity != "" || o.scope != ""):
			return usagef(cmd, "--room takes no --identity or --scope")
		case !o.room && o.identity == "":
			return usagef(cmd, "--identity is required, unless --room")
		case o.identity != "" && !event.ID(o.identity).Valid():
			return usagef(cmd, "--identity must be an identity id from `holaclaw identity list`")
		}
		client, err := cn.dial(cmd.Context())
		if err != nil {
			return err
		}
		return createInvite(cmd.Context(), c, client, o)
	}
	cmd.AddCommand(create)
	return cmd
}

// inviteOptions are the flags of `invite create`.
type inviteOptions struct {
	channel, identity, scope string
	room                     bool
	ttl                      time.Duration
}

// createInvite creates a one-shot expiring code and prints it with its link
// and a QR code of it, or with how to redeem it when the channel has no link.
// The server validates the rest.
func createInvite(ctx context.Context, c *cli, client *api.Client, o inviteOptions) error {
	in := gen.InviteCreate{Channel: o.channel}
	if o.identity != "" {
		in.UserId = &o.identity
	}
	if o.scope != "" {
		in.Scope = ptrOf(gen.InviteCreateScope(o.scope))
	}
	if o.room {
		in.Kind = ptrOf(gen.InviteCreateKindRoom)
	}
	if o.ttl > 0 {
		in.TtlSeconds = ptrOf(int(o.ttl.Seconds()))
	}
	inv, err := client.CreateInvite(ctx, in)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "code     %s\nchannel  %s\n", inv.Code, inv.Channel)
	if inv.Scope != nil {
		_, _ = fmt.Fprintf(c.stdout, "scope    %s\n", *inv.Scope)
	}
	_, _ = fmt.Fprintf(c.stdout, "expires  %s\n", stamp(inv.ExpiresAt))
	if inv.Link != nil && *inv.Link != "" {
		_, _ = fmt.Fprintf(c.stdout, "link     %s\n", *inv.Link)
		writeQR(c.stdout, *inv.Link)
	} else if inv.Instruction != nil {
		_, _ = fmt.Fprintf(c.stdout, "redeem   %s\n", *inv.Instruction)
	}
	return nil
}

func newApprovalsCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "approvals", Short: "list pending approvals", Args: cobra.NoArgs}
	cn := c.connFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmdApprovals(cmd.Context(), c, cn) }
	return cmd
}

func newApproveCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "approve <id>", Short: "approve a pending tool call", Args: cobra.ExactArgs(1)}
	cn := c.connFlags(cmd)
	session := cmd.Flags().Bool("session", false, "allow the same call for the rest of this session (not for a browser step)")
	always := cmd.Flags().Bool("always", false, "allow the same call from now on (not for a browser step)")
	cmd.MarkFlagsMutuallyExclusive("session", "always")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		choice := gen.ApprovalResolveChoiceOnce
		switch {
		case *session:
			choice = gen.ApprovalResolveChoiceSession
		case *always:
			choice = gen.ApprovalResolveChoiceAlways
		}
		return resolve(cmd.Context(), c, cn, args[0], true, &choice)
	}
	return cmd
}

func newDenyCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "deny <id>", Short: "deny a pending tool call", Args: cobra.ExactArgs(1)}
	cn := c.connFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return resolve(cmd.Context(), c, cn, args[0], false, nil)
	}
	return cmd
}

// cmdApprovals lists what is waiting for a human (docs/security.md).
func cmdApprovals(ctx context.Context, c *cli, cn *conn) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	pending, err := client.ListApprovals(ctx)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		_, _ = fmt.Fprintln(c.stdout, "no pending approvals")
		return nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tTOOL\tEXPIRES\tSUMMARY")
	for _, a := range pending {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			a.Id, deref(a.Tool), stamp(a.ExpiresAt), deref(a.Summary))
	}
	return w.Flush()
}

func resolve(ctx context.Context, c *cli, cn *conn, id string, approve bool, choice *gen.ApprovalResolveChoice) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	out, err := client.ResolveApproval(ctx, id, gen.ApprovalResolve{Approve: approve, Choice: choice})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "%s is %s\n", out.Id, out.Status)
	return nil
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05Z") }

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
