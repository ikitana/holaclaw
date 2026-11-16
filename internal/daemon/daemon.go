// Package daemon is the shell around the agent library: it assembles the
// store, policy gate, approvals, memory, sessions and the Control API into one
// running service (ARCHITECTURE.md §2). It adds no behaviour of its own.
package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/api"
	"holaclaw/internal/app"
	"holaclaw/internal/approval"
	"holaclaw/internal/auth"
	"holaclaw/internal/backup"
	"holaclaw/internal/channel"
	"holaclaw/internal/config"
	"holaclaw/internal/connector"
	"holaclaw/internal/event"
	"holaclaw/internal/media"
	"holaclaw/internal/memory"
	"holaclaw/internal/session"
	"holaclaw/internal/skill"
	"holaclaw/internal/store"
	"holaclaw/internal/task"
	"holaclaw/internal/tool"
	"holaclaw/internal/tool/tools"
)

// Version is stamped by the build.
var Version = "dev"

// initPoll is how often an uninitialised daemon looks for an owner that
// `holaclaw init` wrote to the database directly.
const initPoll = 2 * time.Second

// Options configure a daemon run.
type Options struct {
	ConfigDir string
	Logger    *slog.Logger
	// OnListen is called once per bound listener, for tests and the CLI banner.
	OnListen func(kind string, addr net.Addr)
	// Debounce overrides the session debounce (tests use a negative value).
	Debounce time.Duration
	// ChannelBaseURL overrides the platform API base URL per channel instance.
	// Tests only: it is not a config key.
	ChannelBaseURL map[string]string
	// Restart stops this daemon so that its caller starts a new one in its
	// place; Serve sets it. Without it the API cannot restore a backup.
	Restart func()
}

// Daemon is one assembled service.
type Daemon struct {
	App       *app.App
	Auth      *auth.Service
	Bootstrap *auth.Bootstrap
	Approvals *approval.Service
	Sessions  *session.Manager
	Router    *session.Router
	Hub       *api.Hub
	Backend   *api.Backend

	log     *slog.Logger
	opts    Options
	mu      sync.Mutex
	runners map[string]session.Turner // per agent
	media   map[string]*media.Normaliser
	// deliverers lets channel adapters (M3) and tasks (M4) take over delivery
	// for their channel instances; API and CLI conversations need none: the
	// `reply` event on the SSE stream is their delivery.
	deliverers map[string]Deliverer
	channels   *channels
	tasks      *task.Service
	backups    *backup.Manager
	connectors *connector.Manager
	// browser is the live view and the sidecar client, nil without a
	// [browser] table (docs/browser.md); see browser.go.
	browser *browserWiring
}

// Deliverer sends a final reply to a conversation of one channel instance.
type Deliverer func(ctx context.Context, sess store.Session, traceID event.ID, reply event.Message) error

// New assembles the daemon. Call Run to serve.
func New(ctx context.Context, opts Options) (*Daemon, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	// A restore staged by the previous run goes in now, while nothing has the
	// store open (ARCHITECTURE.md §4). One that cannot be finished stops the
	// start: opening the store over a half-replaced database would be worse.
	restored, wasRestored, err := applyPendingRestore(opts.ConfigDir)
	if err != nil {
		return nil, err
	}
	a, err := app.Open(ctx, opts.ConfigDir)
	if err != nil {
		return nil, err
	}
	d := &Daemon{App: a, log: opts.Logger, opts: opts, runners: map[string]session.Turner{}, media: map[string]*media.Normaliser{}, deliverers: map[string]Deliverer{}}
	a.Logger = d.log
	// No install is under way before the API serves: whatever one left
	// behind (a crash mid-fetch or mid-stage, an unused upload) goes.
	if err := skill.CleanTmp(a.SkillTmp()); err != nil {
		d.log.Warn("skill install leftovers", "error", err)
	}

	// Event fan-out: the store first, so every streamed event has its stored id.
	d.Hub = api.NewHub(a.Store, d.log)
	// The channel layer is built last but must see approval events: late-bound.
	sink := event.Ordered(a.Store, d.Hub, event.SinkFunc(func(ctx context.Context, e event.Event) {
		if d.channels != nil {
			d.channels.Emit(ctx, e)
		}
	}), event.SinkFunc(d.onDisabled))
	a.Events = sink
	if wasRestored {
		d.log.Warn("started on restored data", "backup_id", restored.BackupID, "by", restored.By)
		sink.Emit(ctx, event.New(event.BackupRestored, "", "", "", map[string]any{
			"backup_id": restored.BackupID, "by": restored.By, "requested": restored.Requested,
		}))
	}
	a.Memory.Start(ctx)

	d.Auth = auth.New(a.Store)
	d.Auth.Events = sink
	d.Auth.Namespaces = d.aliasNamespaces
	// No owner is created here: a fresh database waits for `holaclaw init`,
	// and the API says so until then (docs/identity.md §3).
	d.Bootstrap = d.Auth.Bootstrap()
	if ok, err := d.Bootstrap.Initialised(ctx); err != nil {
		return nil, d.fail(err)
	} else if !ok {
		d.log.Warn("holaclaw is not initialised: run `holaclaw init`")
	}
	d.Approvals = approval.New(a.Store.DB(), sink, approval.WithGrantPattern(grantPattern))
	if _, err := d.Approvals.ExpireStale(ctx); err != nil {
		return nil, d.fail(err)
	}

	// A crash inside a turn leaves consumed input unanswered: requeue it so
	// the restart replays it.
	healed, err := a.Store.HealTurns(ctx)
	if err != nil {
		return nil, d.fail(err)
	}
	for _, id := range healed {
		if ok, err := a.Store.RequeueLastUser(ctx, id); err != nil {
			return nil, d.fail(err)
		} else if ok {
			d.log.Info("requeued input interrupted by a restart", "session", id)
		}
	}

	d.Sessions = session.New(session.Deps{
		Store: a.Store, Events: sink, Logger: d.log, Debounce: opts.Debounce,
		Runner:     d.runner,
		Prepare:    d.prepare,
		Deliver:    d.deliver,
		ProviderOf: d.providerOf,
	})
	d.Router = &session.Router{
		Resolve: d.resolve, AgentFor: d.agentFor,
		Store: a.Store, Manager: d.Sessions, Events: sink, Logger: d.log,
	}
	d.Backend = &api.Backend{
		Config: a.Config, Store: a.Store, Auth: d.Auth, Bootstrap: d.Bootstrap, Approvals: d.Approvals,
		Router: d.Router, Sessions: d.Sessions, Memory: a.Memory, Secrets: a.Secrets,
		Hub: d.Hub, Version: Version, Started: time.Now(), Logger: d.log,
	}
	d.Backend.ProviderLogin = providerLogin{d}
	d.Backend.Skills = a
	// Managed backups live beside the database, in a directory no backup
	// archives (ARCHITECTURE.md §4).
	d.backups, err = backup.NewManager(backup.ManagerOptions{
		Dir: filepath.Join(a.DataDir, "backups"), DataDir: a.DataDir, ConfigDir: a.Config.Dir,
		SecretsPath: app.SecretsPath(a.Config.Server.SecretsDir),
		DB:          a.Store.DB(), Version: Version, Events: sink, Logger: d.log,
		Every: a.Config.Backups.Every, Keep: a.Config.Backups.Keep,
	})
	if err != nil {
		return nil, d.fail(err)
	}
	d.Backend.Backups = d.backups
	d.Backend.Restart = opts.Restart
	d.Backend.BeforeDeleteSession = d.forgetSession
	if err := d.setupConnectors(ctx); err != nil {
		return nil, d.fail(err)
	}
	if err := d.setupChannels(); err != nil {
		return nil, d.fail(err)
	}
	if err := d.setupTasks(ctx); err != nil {
		return nil, d.fail(err)
	}
	d.setupBrowser(ctx)
	d.Auth.BeforeDeleteUser = d.beforeDeleteUser
	d.Auth.BeforeEnable = d.beforeEnable
	d.Auth.AfterEnable = d.afterEnable
	d.Auth.ForgetUserMemory = d.forgetUserMemory
	return d, nil
}

// applyPendingRestore finishes a restore the previous run staged, if any. A
// config that does not load is left for app.Open to report.
func applyPendingRestore(configDir string) (backup.Restore, bool, error) {
	cfg, err := config.Load(configDir)
	if err != nil {
		return backup.Restore{}, false, nil
	}
	return backup.ApplyPendingRestore(app.DataDir(configDir, cfg.Server.DataDir))
}

// Serve runs a daemon until ctx ends, and starts a new one in its place each
// time the running one asks to be restarted: that is how a restore is
// applied without the process, or whatever supervises it, noticing.
func Serve(ctx context.Context, opts Options) error {
	for {
		runCtx, cancel := context.WithCancel(ctx)
		var restart atomic.Bool
		o := opts
		o.Restart = func() {
			restart.Store(true)
			cancel()
		}
		d, err := New(runCtx, o)
		if err != nil {
			cancel()
			return err
		}
		err = d.Run(runCtx)
		cancel()
		if err != nil || !restart.Load() || ctx.Err() != nil {
			return err
		}
		d.log.Info("restarting")
	}
}

func (d *Daemon) fail(err error) error {
	if d.connectors != nil {
		_ = d.connectors.Close()
	}
	_ = d.App.Close()
	return err
}

// SetDeliverer registers the delivery function of a channel instance.
func (d *Daemon) SetDeliverer(channel string, fn Deliverer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deliverers[channel] = fn
}

// Socket returns the control socket path.
func (d *Daemon) Socket() string { return SocketPath(d.App) }

// SocketPath is server.socket, else app.DefaultSocket.
func SocketPath(a *app.App) string {
	if s := a.Config.Server.Socket; s != "" {
		return s
	}
	return app.DefaultSocket()
}

// Run serves until ctx ends, then shuts everything down in order.
func (d *Daemon) Run(ctx context.Context) error {
	if p := d.Socket(); len(p) > 100 {
		return d.fail(fmt.Errorf("socket path is %d bytes, the limit is about 100: set server.socket in config.toml to a shorter path (%s)", len(p), p))
	}
	// Serve can return on its own (a bind error): the loops below then end
	// too, and Run returns that error.
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if err := d.Sessions.Start(ctx); err != nil {
		return d.fail(err)
	}
	go func() {
		if err := d.tasks.Run(ctx); err != nil && ctx.Err() == nil {
			d.log.Error("task scheduler stopped", "err", err)
		}
	}()
	go d.housekeeping(ctx)
	checksDone := make(chan struct{})
	go func() {
		defer close(checksDone)
		d.connectorChecks(ctx)
	}()
	go d.automaticBackups(ctx)
	chDone := make(chan struct{})
	go func() {
		defer close(chDone)
		// No adapter starts before there is an owner to answer to.
		if d.Bootstrap.Wait(ctx, initPoll) == nil {
			d.channels.run(ctx)
		}
	}()
	srv := d.App.Config.Server
	if srv.Listen != "" && srv.AllowInsecureTransport && (srv.TLSCert == "" || srv.TLSKey == "") {
		d.log.Warn("server.allow_insecure_transport: the control API is served over plain http", "listen", srv.Listen)
	}
	err := api.Serve(ctx, d.Backend, api.ServeOptions{
		Socket: d.Socket(), Listen: srv.Listen, TLSCert: srv.TLSCert, TLSKey: srv.TLSKey,
		AllowInsecureTransport: srv.AllowInsecureTransport,
		OnListen:               d.opts.OnListen,
	})
	cancelRun()
	stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if serr := d.Sessions.Shutdown(stop); serr != nil && err == nil {
		err = serr
	}
	<-chDone
	// No check runs any more. A turn Shutdown gave up on may still be in a
	// connector tool call: Close interrupts it with mcp.ErrClosed.
	<-checksDone
	_ = d.connectors.Close()
	// A backup still running reads the database: it ends, as failed, before
	// the store closes.
	d.backups.Close()
	d.Hub.Close()
	if cerr := d.App.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

// grantPattern narrows an approved "session"/"always" choice into a grant:
// the exact command for exec, any arguments otherwise.
func grantPattern(a tool.Approval) string {
	if a.Tool != "exec" {
		return ""
	}
	cmd, err := tools.ExecCommand([]byte(a.Args))
	if err != nil {
		return "\x00" // matches nothing
	}
	return cmd
}

// runner returns the (cached) loop of the session's agent with the policy
// gate inside and compaction after each turn.
func (d *Daemon) runner(sess store.Session) (session.Turner, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.runners[sess.Agent]; ok {
		return t, nil
	}
	a := d.App
	exec, err := a.Executor(sess.Agent)
	if err != nil {
		return nil, err
	}
	if exec != nil {
		gc := tool.GateConfig{
			Policy: a.Policy(sess.Agent), Grants: d.Approvals, Approver: d.Approvals,
			Events: a.Events, ExecCommand: tools.ExecCommand,
		}
		// The guard role is optional; without it policy alone decides.
		if g, err := a.Guard(sess.Agent); err != nil {
			return nil, err
		} else if g != nil {
			gc.Guard = g
		}
		exec = tool.NewGate(exec, gc)
	}
	r, err := a.Runner(sess.Agent, exec, a.Events)
	if err != nil {
		return nil, err
	}
	summarize := &agent.ModelRef{Model: r.Model, Provider: r.Provider}
	if role := a.Config.Agents[sess.Agent].Models.Summarize; role != "" {
		if summarize, err = a.ModelRef(role); err != nil {
			return nil, err
		}
	}
	t := &compacting{
		Runner: r, d: d,
		compactor: &agent.Compactor{
			Store: a.Store, Summarize: *summarize, Events: a.Events,
			Blobs: func(ctx context.Context, text string) (string, error) {
				b, err := a.Store.PutBlob(ctx, strings.NewReader(text), "text/plain; charset=utf-8")
				return b.ID, err
			},
			OnSummary: d.ingestRecall,
		},
		log: d.log,
	}
	d.runners[sess.Agent] = t
	return t, nil
}

// compacting runs compaction right after a turn that asked for it. It runs
// inside the session actor, so it is serialized with the session's turns.
type compacting struct {
	*agent.Runner
	d         *Daemon
	compactor *agent.Compactor
	log       *slog.Logger
}

func (c *compacting) Turn(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
	stopTyping := c.d.channels.typing(ctx, in.Call.Origin)
	res, err := c.Runner.Turn(ctx, in)
	stopTyping()
	if err != nil || !res.NeedsCompaction || ctx.Err() != nil {
		return res, err
	}
	var specs = c.Tools
	def := c.Def
	def.Skills = in.Skills
	ci := agent.CompactInput{SessionID: in.SessionID, TraceID: in.TraceID, Def: def, Model: c.Model}
	if specs != nil {
		ci.Tools = agent.VisibleTools(specs.Specs(), in.Skills, in.Call)
	}
	if _, cerr := c.compactor.Compact(ctx, ci); cerr != nil {
		// Loud, but the reply already exists: never fail the turn for it.
		c.log.Warn("compaction failed", "session", in.SessionID, "err", cerr)
	}
	return res, nil
}

func (d *Daemon) ingestRecall(ctx context.Context, sess store.Session, summary string, tainted bool) {
	if sess.Kind == store.SessionTask {
		return
	}
	// A session whose identity was deleted belongs to nobody: a turn still queued
	// or running at the delete must not write recall of that conversation into a
	// partition nothing forgets (issue #48).
	if sess.UserID == "" {
		return
	}
	_, err := d.App.Memory.IngestRecall(ctx, memory.Provenance{
		Agent: sess.Agent, Partition: Partition(sess), UserID: sess.UserID,
		SessionID: sess.ID, Time: time.Now().UTC(),
	}, summary, tainted)
	if err != nil {
		d.log.Warn("recall ingestion failed", "session", sess.ID, "err", err)
	}
}

// Partition is the memory space of a conversation: one per DM user, one per
// room, never mixed (docs/memory.md §2). A room whose identity was just deleted
// reads and writes a space of its own, which no ULID can name, until the next
// message gives it a new identity.
func Partition(sess store.Session) string {
	switch {
	case sess.Kind == store.SessionTask:
		return "task/" + string(sess.ID)
	case sess.Origin.ConversationKind == event.ConversationRoom:
		if sess.UserID == "" {
			return "room/" + sess.Origin.Channel + "/" + sess.Origin.Conversation
		}
		return RoomPartition(sess.UserID)
	case sess.UserID != "":
		return UserPartition(sess.UserID)
	default:
		return "dm/" + sess.Origin.Channel + "/" + sess.Origin.Conversation
	}
}

// UserPartition is a person's own memory space, the one their DMs write.
func UserPartition(id event.ID) string { return "dm/" + string(id) }

// RoomPartition is a room identity's memory space.
func RoomPartition(id event.ID) string { return "room/" + string(id) }

// roleStrictness orders the policy classes from the widest to the narrowest.
// It is the comparison a coalesced turn needs, nothing else.
func roleStrictness(r store.Role) int {
	switch r {
	case store.RoleOwner:
		return 0
	case store.RoleAdmin:
		return 1
	case store.RoleUser:
		return 2
	default: // guest, and any role this build does not know
		return 3
	}
}

// actingIdentity picks the user whose policy class a turn runs under: the sender of
// the messages being answered, never the session row. A room's session is shared
// by everyone in the room and its row names the room, which holds no role
// (docs/sessions.md, docs/security.md).
//
// A turn that coalesced messages from several senders runs under the strictest
// of them, and under that sender's id. Messages without a user (task runs,
// rows written before this field existed) fall back to the session user; a user
// that no longer exists is a guest, and roleOf makes a room one too.
func actingIdentity(pending []event.Message, sessionUser event.ID, roleOf func(event.ID) (store.Role, bool)) (event.ID, store.Role) {
	ids := session.Senders(sessionUser, pending)
	if len(ids) == 0 {
		return "", store.RoleGuest
	}
	var (
		bestID   event.ID
		bestRole = store.RoleGuest
		best     = -1
	)
	for _, id := range ids {
		role := store.RoleGuest
		if r, ok := roleOf(id); ok {
			role = r
		}
		if s := roleStrictness(role); s > best {
			best, bestID, bestRole = s, id, role
		}
	}
	return bestID, bestRole
}

// prepare fills the per-turn input: volatile injections (time, recall), the
// trusted tool-call template and the image resolver.
func (d *Daemon) prepare(ctx context.Context, sess store.Session, pending []event.Message) (agent.TurnInput, error) {
	a := d.App
	if d.turnDisabled(ctx, sess, pending) {
		return agent.TurnInput{}, session.ErrIdentityDisabled
	}
	identityID, role := actingIdentity(pending, sess.UserID, func(id event.ID) (store.Role, bool) {
		i, err := a.Store.GetIdentity(ctx, id)
		if err != nil {
			return "", false
		}
		// A room never grants anything: it only gets here as the session user of
		// a message with no sender. A sender disabled since its message came in
		// is no more than a guest in a turn someone else keeps going.
		if i.Disabled() {
			return store.RoleGuest, true
		}
		return auth.RoleOf(i), true
	})
	runKind := tool.RunInteractive
	if sess.Kind == store.SessionTask {
		runKind = tool.RunTask
	}
	partition := Partition(sess)
	in := agent.TurnInput{
		Injections: []agent.Injection{{Text: "now: " + time.Now().Format("2006-01-02 15:04 MST")}},
		Call: tool.Call{
			Agent: sess.Agent, UserID: identityID, Role: string(role), Origin: sess.Origin,
			RunKind: runKind, Partition: partition,
			Workspace: filepath.Join(a.DataDir, "workspace", sess.Agent),
			// A task run answers no message, so it has nothing to react to.
			CanReact: runKind == tool.RunInteractive && d.channels.canReact(sess.Origin.Channel),
		},
		Images: func(ctx context.Context, at event.Attachment) ([]byte, error) {
			rc, _, err := a.Store.OpenBlob(ctx, at.BlobID)
			if err != nil {
				return nil, err
			}
			defer func() { _ = rc.Close() }()
			return io.ReadAll(io.LimitReader(rc, 8<<20))
		},
	}
	if runKind == tool.RunTask {
		in.Call.TaskID = sess.ID
		if d.tasks != nil {
			in.MaxSteps = d.tasks.MaxSteps(ctx, sess.ID)
		}
	}
	var traceID event.ID
	var query []string
	mentioned := false
	for _, m := range pending {
		traceID = m.TraceID
		query = append(query, m.Text)
		mentioned = mentioned || m.Mentioned
	}
	// A coalesced room turn runs under its strictest sender but its trace is the
	// last message's: every other sender goes on that trace too, so whoever reads
	// it to decide what the turn may show sees them all (channels.turnIsAdmin).
	if sess.Origin.ConversationKind == event.ConversationRoom {
		seen := map[event.ID]bool{}
		for _, m := range pending {
			if m.TraceID == traceID || m.UserID == "" || seen[m.UserID] {
				continue
			}
			seen[m.UserID] = true
			a.Events.Emit(ctx, event.New(event.RouterResolved, traceID, sess.ID, "", map[string]any{
				"user": string(m.UserID), "session": string(sess.ID), "coalesced": true,
			}))
		}
	}
	// Only reply = "always" starts a room turn that mentions nobody: the
	// model may pass on it instead of answering chatter.
	if sess.Origin.ConversationKind == event.ConversationRoom && !mentioned && len(pending) > 0 {
		in.Injections = append(in.Injections, agent.Injection{Text: agent.NoReplyNote})
	}
	in.OnText = func(delta string) { d.Hub.PublishToken(sess.ID, traceID, delta) }

	// The skill index of this run, from the same trusted fields every tool
	// call inherits; skill.load serves the cached set under this trace.
	sc := in.Call
	sc.TraceID, sc.SessionID = traceID, sess.ID
	in.Skills = app.SkillEntries(a.RunSkills(sc))

	// Media normalisation: transcripts and image descriptions are persisted on
	// the stored messages; what could not be processed becomes a notice the
	// model can relay. The session manager reads the history after Prepare.
	norm := d.normaliser(sess.Agent)
	for _, notice := range norm.Normalise(ctx, traceID, sess.ID, pending) {
		in.Injections = append(in.Injections, agent.Injection{Text: "notice: " + notice})
	}
	for _, doc := range norm.InlineDocuments(ctx, pending) {
		// An inlined document is untrusted content entering the turn: the taint
		// must reach the policy gate, not just the prompt (docs/security.md).
		in.Call.Tainted = true
		in.Injections = append(in.Injections, agent.Injection{Text: doc})
	}

	// Recall: deterministic, no model call; skipped for trivial prompts and
	// for task runs.
	text := strings.Join(query, "\n")
	if runKind == tool.RunInteractive && !memory.Trivial(text) {
		hits, err := a.Memory.Search(ctx, memory.Query{
			Agent: sess.Agent, Partition: partition, Text: text, TraceID: traceID, SessionID: sess.ID,
		})
		if err != nil {
			d.log.Warn("memory search failed", "session", sess.ID, "err", err)
		}
		for _, h := range hits {
			date := h.Item.Prov.Time.Format("2006-01-02")
			line := fmt.Sprintf("memory (%s, %s): %s", h.Item.Layer, date, oneLine(h.Item.Text))
			if h.Item.Tainted {
				// A recalled summary of untrusted content stays untrusted: it is
				// re-delimited and taints the turn, so recall cannot launder it
				// into trusted context (docs/memory.md, docs/security.md).
				in.Call.Tainted = true
				line = fmt.Sprintf("memory (%s, %s, untrusted): %s", h.Item.Layer, date,
					agent.WrapUntrusted(oneLine(h.Item.Text)))
			}
			in.Injections = append(in.Injections, agent.Injection{ID: h.Item.ID, Text: line})
			a.Events.Emit(ctx, event.New(event.MemoryInjected, traceID, sess.ID, "", map[string]any{
				"memory_id": h.Item.ID, "layer": h.Item.Layer, "score": h.Score,
			}))
		}
	}
	return in, nil
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return s
}

// deliver hands the reply to the conversation's channel. The target is the
// session origin, never anything the model produced.
func (d *Daemon) deliver(ctx context.Context, sess store.Session, traceID event.ID, reply event.Message) error {
	if sess.Kind == store.SessionTask {
		return d.finishTask(ctx, sess, traceID, reply)
	}
	d.mu.Lock()
	fn := d.deliverers[sess.Origin.Channel]
	d.mu.Unlock()
	if fn == nil {
		return nil // API / CLI: the `reply` event is the delivery
	}
	return fn(ctx, sess, traceID, reply)
}

func (d *Daemon) providerOf(sess store.Session) string {
	ag, ok := d.App.Config.Agents[sess.Agent]
	if !ok {
		return ""
	}
	return d.App.Config.Models[ag.Models.Chat].Provider
}

func (d *Daemon) agentFor(channel string) string {
	if c, ok := d.App.Config.Channels[channel]; ok && c.Agent != "" {
		return c.Agent
	}
	return session.DefaultAgent
}

// resolve maps a sender's aliases to an identity through its bindings. A
// channel's free aliases are linked to the identity found. A holaclaw client
// arrives with the alias of the binding its token or the unix socket resolved
// to, and the daemon itself issues those, so nothing is linked.
func (d *Daemon) resolve(ctx context.Context, origin event.Origin, sender event.Sender) (store.Resolution, error) {
	if origin.Channel == event.ChannelHolaclaw {
		i, err := d.Auth.Resolve(ctx, origin.Channel, sender.Aliases)
		return store.Resolution{Identity: i}, err
	}
	return d.Auth.ResolveAndLink(ctx, origin.Channel, sender)
}

// aliasNamespaces returns the alias namespaces a channel instance declares
// (channel.AliasNamespacer).
func (d *Daemon) aliasNamespaces(name string) []string {
	if d.channels == nil {
		return nil
	}
	if ns, ok := d.channels.byName[name].(channel.AliasNamespacer); ok {
		return ns.AliasNamespaces()
	}
	return nil
}
