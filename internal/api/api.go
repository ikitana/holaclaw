// Package api is the Control API: the single client interface of holaclaw —
// HTTP/JSON commands under /v1 plus one resumable SSE stream per client window
// (docs/reference/api.md). Routing and wire models are generated from
// api/openapi.yaml into package gen; this package holds the Backend the
// handlers run against, the auth and scope middlewares, the event hub, the
// listeners and a small Go client for the CLI.
//
// Two invariants shape the code: a token is never revealed to be absent rather
// than wrong (every auth failure is one `unauthorized`), and credentials are
// write-only — no handler, response or log line ever carries a secret value,
// with one exception an admin has to ask for: the archive of a backup made
// with include_secrets (ARCHITECTURE.md §4).
package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/approval"
	"holaclaw/internal/auth"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/channel"
	"holaclaw/internal/config"
	"holaclaw/internal/connector"
	"holaclaw/internal/connector/oauth"
	"holaclaw/internal/event"
	"holaclaw/internal/memory"
	"holaclaw/internal/session"
	"holaclaw/internal/store"
)

// Backend is everything the handlers need. The daemon fills the fields it has;
// the optional ones (Memory, Tasks, Skills, Backups, Channels, ChannelLinker,
// ProviderLogin, Connectors) answer 501 `not_implemented` while their milestone is
// missing, so the surface of /v1 never changes shape between builds.
type Backend struct {
	Config *config.Config
	Store  store.Store
	Auth   *auth.Service
	// Bootstrap, when set, gates the API on an owner existing: until
	// `holaclaw init` creates one, every operation answers 503
	// `not_initialised` (docs/identity.md §3). Nil serves as initialised.
	Bootstrap *auth.Bootstrap
	Approvals *approval.Service
	Router    *session.Router
	Sessions  *session.Manager

	// Memory may be nil until the memory backend is wired (501).
	Memory  memory.Store
	Secrets *secrets.File

	// Hub fans events out to SSE clients. NewHandler creates one when it is
	// nil; the daemon adds it to the event fan-out after the store sink, so
	// every streamed event already has its stored id.
	Hub *Hub

	// Tasks is the task surface (docs/tasks.md); nil answers 501.
	Tasks TaskBackend
	// Skills is the skills explorer surface (docs/skills.md §7); nil answers
	// 501.
	Skills SkillBackend
	// Browser is the browser live view and takeover lock (docs/browser.md); nil answers
	// 501.
	Browser BrowserBackend
	// Backups is the managed backups of the daemon (ARCHITECTURE.md §4); nil answers 501.
	Backups BackupBackend
	// Restart stops the daemon and starts it again in this process, which is
	// how a staged restore is applied; nil answers 501 to a restore and to a
	// restart.
	Restart func()
	// Channels lists the configured channel instances; nil is an empty list.
	Channels func() []gen.ChannelStatus
	// ChannelLinker is the running adapter of a configured channel instance
	// as a channel.Linker, or nil when it cannot link interactively; a nil
	// func or a nil Linker answers 501.
	ChannelLinker func(name string) channel.Linker
	// ProviderLogin is the device-code flow (docs/models.md); nil answers 501.
	ProviderLogin ProviderLoginBackend
	// Connectors is the connector manager (docs/connectors.md §8); nil
	// answers 501.
	Connectors ConnectorBackend
	// BeforeDeleteSession runs before a session is deleted; an error aborts it.
	BeforeDeleteSession func(ctx context.Context, id event.ID) error

	Version string
	Started time.Time
	Logger  *slog.Logger
}

// TaskBackend mirrors the task operations of the spec. It deals in the wire
// types on purpose: the scheduler owns the semantics, the API only carries
// them (docs/tasks.md).
type TaskBackend interface {
	ListTasks(ctx context.Context) ([]gen.Task, error)
	GetTask(ctx context.Context, id string) (gen.Task, error)
	CreateTask(ctx context.Context, in gen.TaskCreate, by event.ID) (gen.Task, error)
	DeleteTask(ctx context.Context, id string) error
}

// SkillBackend mirrors the skill operations of the spec, in wire types like
// TaskBackend: the loader and writer own the semantics (docs/skills.md §7).
// A skill is named by agent and id; a skill's id is its name. Errors:
// skill.ErrNotFound, skill.ErrExists, skill.ErrBundled and skill.ErrBadName
// map onto the stable codes; who is the acting user id, or "admin" for a
// principal without one. GetSkill is the skill as it is now;
// GetSkillVersion is one of an agent skill's versions by number, the live
// one included. RevertSkill with a nil to is a no-op.
type SkillBackend interface {
	ListSkills(ctx context.Context, agent string) ([]gen.Skill, error)
	GetSkill(ctx context.Context, agent, id string) (gen.Skill, error)
	GetSkillVersion(ctx context.Context, agent, id string, version int) (gen.Skill, error)
	RemoveSkill(ctx context.Context, agent, id string, who event.ID) error
	RestoreSkill(ctx context.Context, agent, id string, who event.ID) (gen.Skill, error)
	RevertSkill(ctx context.Context, agent, id string, to *int, who event.ID) (gen.Skill, error)
	// InstallSkill fetches or opens, stages and scans in.Git or in.Upload
	// and, unless in.Preview, installs it under agents/<agent>/skills/
	// (docs/skills.md §7).
	InstallSkill(ctx context.Context, in gen.SkillInstall, who event.ID) (gen.SkillInstalled, error)
	// UploadSkill keeps the gzipped tar r of a folder the client read at
	// path, for an install to name by the answer's id.
	UploadSkill(ctx context.Context, r io.Reader, path string, who event.ID) (gen.SkillUpload, error)
}

// ProviderLoginBackend is the device-code login of one provider. Start begins
// a login, Status polls it; neither ever returns a credential.
type ProviderLoginBackend interface {
	Start(ctx context.Context, provider string) (gen.DeviceLogin, error)
	Status(ctx context.Context, provider string) (gen.DeviceLogin, error)
}

// ConnectorBackend is the part of *connector.Manager the handlers use. Its
// errors are the connector package's and map onto the stable codes; none
// carries a credential or server text.
type ConnectorBackend interface {
	Create(ctx context.Context, r connector.CreateRequest) (connector.Started, error)
	Code(ctx context.Context, id string, cb oauth.Callback) (connector.Connector, error)
	Reconnect(ctx context.Context, id string, r connector.ReconnectRequest) (connector.Started, error)
	List(ctx context.Context) ([]connector.Connector, error)
	Get(ctx context.Context, id string) (connector.Connector, error)
	Inspect(ctx context.Context, serverURL string) (connector.Inspection, error)
	Delete(ctx context.Context, id string) error
}

func (b *Backend) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

// Stable error codes. They are part of the contract: the spec lists them in
// the description of Error.code.
const (
	CodeBadRequest     = "bad_request"
	CodeUnauthorized   = "unauthorized"
	CodeForbidden      = "forbidden"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeTooLarge       = "too_large"
	CodeRateLimited    = "rate_limited"
	CodeInternal       = "internal"
	CodeNotImplemented = "not_implemented"
	CodeNotInitialised = "not_initialised"

	CodeIdentityDisabled = "identity_disabled"
)

// scopes is the required scope of every operation, keyed by operationId. It
// mirrors `x-scope` in api/openapi.yaml; TestScopesMatchSpec fails the build
// when the two drift apart, because scopes are part of the contract
// (docs/identity.md, ARCHITECTURE.md §7).
var scopes = map[string]auth.Scope{
	"createSession":        auth.ScopeClient,
	"listSessions":         auth.ScopeClient,
	"getSession":           auth.ScopeClient,
	"deleteSession":        auth.ScopeClient,
	"sendMessage":          auth.ScopeClient,
	"listMessages":         auth.ScopeClient,
	"abortSession":         auth.ScopeClient,
	"listTraces":           auth.ScopeClient,
	"putBlob":              auth.ScopeClient,
	"getBlob":              auth.ScopeClient,
	"streamEvents":         auth.ScopeClient,
	"listApprovals":        auth.ScopeAdmin,
	"resolveApproval":      auth.ScopeAdmin,
	"listIdentities":       auth.ScopeAdmin,
	"getIdentity":          auth.ScopeAdmin,
	"updateIdentity":       auth.ScopeAdmin,
	"assignIdentity":       auth.ScopeAdmin,
	"unbindBinding":        auth.ScopeAdmin,
	"createIdentity":       auth.ScopeAdmin,
	"disableIdentity":      auth.ScopeAdmin,
	"enableIdentity":       auth.ScopeAdmin,
	"deleteIdentity":       auth.ScopeAdmin,
	"createInvite":         auth.ScopeAdmin,
	"listTokens":           auth.ScopeAdmin,
	"createToken":          auth.ScopeAdmin,
	"revokeToken":          auth.ScopeAdmin,
	"updateToken":          auth.ScopeAdmin,
	"rotateToken":          auth.ScopeAdmin,
	"rotateOwnToken":       auth.ScopeClient,
	"getMe":                auth.ScopeClient,
	"listDevices":          auth.ScopeClient,
	"createDeviceLink":     auth.ScopeClient,
	"unbindDevice":         auth.ScopeClient,
	"pairDevice":           auth.ScopeClient,
	"listTasks":            auth.ScopeAdmin,
	"createTask":           auth.ScopeAdmin,
	"getTask":              auth.ScopeAdmin,
	"deleteTask":           auth.ScopeAdmin,
	"listChannels":         auth.ScopeClient,
	"loginChannel":         auth.ScopeAdmin,
	"getChannelLogin":      auth.ScopeAdmin,
	"logoutChannel":        auth.ScopeAdmin,
	"getStatus":            auth.ScopeClient,
	"restartDaemon":        auth.ScopeAdmin,
	"listAgents":           auth.ScopeClient,
	"listModels":           auth.ScopeClient,
	"putCredentials":       auth.ScopeAdmin,
	"loginProvider":        auth.ScopeAdmin,
	"getProviderLogin":     auth.ScopeAdmin,
	"getTrace":             auth.ScopeClient,
	"searchMemory":         auth.ScopeAdmin,
	"listMemoryPartitions": auth.ScopeAdmin,
	"writeMemory":          auth.ScopeAdmin,
	"forgetMemory":         auth.ScopeAdmin,
	"listSkills":           auth.ScopeClient,
	"getSkill":             auth.ScopeClient,
	"removeSkill":          auth.ScopeAdmin,
	"restoreSkill":         auth.ScopeAdmin,
	"revertSkill":          auth.ScopeAdmin,
	"installSkill":         auth.ScopeAdmin,
	"uploadSkill":          auth.ScopeAdmin,
	"listBrowserSessions":  auth.ScopeClient,
	"viewBrowser":          auth.ScopeClient,
	"streamBrowser":        auth.ScopeClient,
	"takeoverBrowser":      auth.ScopeAdmin,
	"releaseBrowser":       auth.ScopeAdmin,
	"getBrowserViewer":     auth.ScopeClient,
	"listBackups":          auth.ScopeAdmin,
	"createBackup":         auth.ScopeAdmin,
	"getBackup":            auth.ScopeAdmin,
	"getBackupContent":     auth.ScopeAdmin,
	"verifyBackup":         auth.ScopeAdmin,
	"deleteBackup":         auth.ScopeAdmin,
	"uploadBackup":         auth.ScopeAdmin,
	"restoreBackup":        auth.ScopeAdmin,
	"listConnectors":       auth.ScopeAdmin,
	"createConnector":      auth.ScopeAdmin,
	"getConnector":         auth.ScopeAdmin,
	"postConnectorCode":    auth.ScopeAdmin,
	"reconnectConnector":   auth.ScopeAdmin,
	"inspectConnector":     auth.ScopeAdmin,
	"deleteConnector":      auth.ScopeAdmin,
}

// route is one spec operation as the router sees it: method plus path
// template, with the /v1 prefix stripped.
type route struct {
	Method string
	Path   string
}

// routes maps a matched pattern back to its operationId, so the scope
// middleware enforces the spec rather than a hand-written switch. The drift
// test checks this table against the spec too.
var routes = map[route]string{
	{"POST", "/sessions"}:                          "createSession",
	{"GET", "/sessions"}:                           "listSessions",
	{"GET", "/sessions/{id}"}:                      "getSession",
	{"DELETE", "/sessions/{id}"}:                   "deleteSession",
	{"POST", "/sessions/{id}/messages"}:            "sendMessage",
	{"GET", "/sessions/{id}/messages"}:             "listMessages",
	{"POST", "/sessions/{id}/abort"}:               "abortSession",
	{"GET", "/sessions/{id}/traces"}:               "listTraces",
	{"POST", "/blobs"}:                             "putBlob",
	{"GET", "/blobs/{id}"}:                         "getBlob",
	{"GET", "/events"}:                             "streamEvents",
	{"GET", "/approvals"}:                          "listApprovals",
	{"POST", "/approvals/{id}"}:                    "resolveApproval",
	{"GET", "/identities"}:                         "listIdentities",
	{"GET", "/identities/{id}"}:                    "getIdentity",
	{"PATCH", "/identities/{id}"}:                  "updateIdentity",
	{"POST", "/identities/{id}/assign"}:            "assignIdentity",
	{"DELETE", "/bindings/{ref}"}:                  "unbindBinding",
	{"POST", "/identities"}:                        "createIdentity",
	{"POST", "/identities/{id}/disable"}:           "disableIdentity",
	{"POST", "/identities/{id}/enable"}:            "enableIdentity",
	{"DELETE", "/identities/{id}"}:                 "deleteIdentity",
	{"POST", "/invites"}:                           "createInvite",
	{"GET", "/tokens"}:                             "listTokens",
	{"POST", "/tokens"}:                            "createToken",
	{"DELETE", "/tokens/{id}"}:                     "revokeToken",
	{"PATCH", "/tokens/{id}"}:                      "updateToken",
	{"POST", "/tokens/{id}/rotate"}:                "rotateToken",
	{"POST", "/me/token/rotate"}:                   "rotateOwnToken",
	{"GET", "/me"}:                                 "getMe",
	{"GET", "/me/devices"}:                         "listDevices",
	{"POST", "/me/devices"}:                        "createDeviceLink",
	{"DELETE", "/me/devices/{binding}"}:            "unbindDevice",
	{"POST", "/pair"}:                              "pairDevice",
	{"GET", "/tasks"}:                              "listTasks",
	{"POST", "/tasks"}:                             "createTask",
	{"GET", "/tasks/{id}"}:                         "getTask",
	{"DELETE", "/tasks/{id}"}:                      "deleteTask",
	{"GET", "/channels"}:                           "listChannels",
	{"POST", "/channels/{name}/login"}:             "loginChannel",
	{"GET", "/channels/{name}/login"}:              "getChannelLogin",
	{"DELETE", "/channels/{name}/login"}:           "logoutChannel",
	{"GET", "/status"}:                             "getStatus",
	{"POST", "/restart"}:                           "restartDaemon",
	{"GET", "/agents"}:                             "listAgents",
	{"GET", "/models"}:                             "listModels",
	{"PUT", "/credentials/{provider}"}:             "putCredentials",
	{"POST", "/credentials/{provider}/login"}:      "loginProvider",
	{"GET", "/credentials/{provider}/login"}:       "getProviderLogin",
	{"GET", "/traces/{id}"}:                        "getTrace",
	{"GET", "/memory"}:                             "searchMemory",
	{"POST", "/memory"}:                            "writeMemory",
	{"GET", "/memory/partitions"}:                  "listMemoryPartitions",
	{"DELETE", "/memory/{id}"}:                     "forgetMemory",
	{"GET", "/skills"}:                             "listSkills",
	{"POST", "/skills"}:                            "installSkill",
	{"POST", "/skills/upload"}:                     "uploadSkill",
	{"GET", "/skills/{id}"}:                        "getSkill",
	{"DELETE", "/skills/{id}"}:                     "removeSkill",
	{"POST", "/skills/{id}/restore"}:               "restoreSkill",
	{"POST", "/skills/{id}/revert"}:                "revertSkill",
	{"GET", "/browser/sessions"}:                   "listBrowserSessions",
	{"POST", "/browser/sessions/{agent}/view"}:     "viewBrowser",
	{"GET", "/browser/sessions/{agent}/stream"}:    "streamBrowser",
	{"POST", "/browser/sessions/{agent}/takeover"}: "takeoverBrowser",
	{"POST", "/browser/sessions/{agent}/release"}:  "releaseBrowser",
	{"GET", "/browser/viewer"}:                     "getBrowserViewer",
	{"GET", "/backups"}:                            "listBackups",
	{"POST", "/backups"}:                           "createBackup",
	{"GET", "/backups/{id}"}:                       "getBackup",
	{"DELETE", "/backups/{id}"}:                    "deleteBackup",
	{"GET", "/backups/{id}/content"}:               "getBackupContent",
	{"POST", "/backups/{id}/verify"}:               "verifyBackup",
	{"POST", "/backups/upload"}:                    "uploadBackup",
	{"POST", "/backups/{id}/restore"}:              "restoreBackup",
	{"GET", "/connectors"}:                         "listConnectors",
	{"POST", "/connectors"}:                        "createConnector",
	{"POST", "/connectors/inspect"}:                "inspectConnector",
	{"GET", "/connectors/{id}"}:                    "getConnector",
	{"DELETE", "/connectors/{id}"}:                 "deleteConnector",
	{"POST", "/connectors/{id}/code"}:              "postConnectorCode",
	{"POST", "/connectors/{id}/reconnect"}:         "reconnectConnector",
}

// ptr is the pointer-to-value helper the generated optional fields need.
func ptr[T any](v T) *T { return &v }

// optString renders an empty string as an omitted field.
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// optID is optString for ids.
func optID(id event.ID) *string { return optString(string(id)) }

// ErrBadInput marks a backend rejection caused by the request itself. Backends
// wrap it; the message is shown to the client, so it must not carry internals.
var ErrBadInput = errors.New("bad input")
