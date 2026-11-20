package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"holaclaw/internal/api/gen"
)

// Client is the small Go client the CLI uses. It speaks the same /v1 as every
// other client: the CLI has no private path into the daemon (ARCHITECTURE.md §2).
type Client struct {
	http  *http.Client
	base  string
	token string
}

// ClientError is a non-2xx answer, carrying the stable code of the spec.
type ClientError struct {
	Status  int
	Code    string
	Message string
}

func (e *ClientError) Error() string {
	return fmt.Sprintf("api: %s (%d %s)", e.Message, e.Status, e.Code)
}

// Dial builds a client. addr is either a base URL (http:// or https://) or the
// path of the unix socket, where the token is not needed.
func Dial(addr, token string) (*Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("api: empty address")
	}
	c := &Client{token: token}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		u, err := url.Parse(addr)
		if err != nil {
			return nil, fmt.Errorf("api: bad base url: %w", err)
		}
		c.base = strings.TrimSuffix(u.String(), "/")
		c.http = &http.Client{}
	} else {
		socket := addr
		c.base = "http://unix"
		c.http = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
			},
		}}
	}
	if !strings.HasSuffix(c.base, BaseURL) {
		c.base += BaseURL
	}
	return c, nil
}

// --- operations ---------------------------------------------------------------

func (c *Client) Status(ctx context.Context) (gen.Status, error) {
	var out gen.Status
	err := c.do(ctx, http.MethodGet, "/status", nil, &out)
	return out, err
}

func (c *Client) CreateSession(ctx context.Context, in gen.SessionCreate) (gen.Session, error) {
	var out gen.Session
	err := c.do(ctx, http.MethodPost, "/sessions", in, &out)
	return out, err
}

func (c *Client) SendMessage(ctx context.Context, sessionID string, in gen.MessageSend) (gen.MessageAccepted, error) {
	var out gen.MessageAccepted
	err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/messages", in, &out)
	return out, err
}

func (c *Client) ListSessions(ctx context.Context) ([]gen.Session, error) {
	var out struct {
		Sessions []gen.Session `json:"sessions"`
	}
	err := c.do(ctx, http.MethodGet, "/sessions", nil, &out)
	return out.Sessions, err
}

func (c *Client) GetSession(ctx context.Context, sessionID string) (gen.Session, error) {
	var out gen.Session
	err := c.do(ctx, http.MethodGet, "/sessions/"+url.PathEscape(sessionID), nil, &out)
	return out, err
}

// ListMessages pages the transcript of a session. after and limit are optional
// (empty and 0 mean "from the start, server default page").
func (c *Client) ListMessages(ctx context.Context, sessionID, after string, limit int) ([]gen.Message, error) {
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/sessions/" + url.PathEscape(sessionID) + "/messages"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out struct {
		Messages []gen.Message `json:"messages"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Messages, err
}

// AbortSession cancels the running turn. Aborting an idle session is not an
// error.
func (c *Client) AbortSession(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/abort", nil, nil)
}

func (c *Client) ListApprovals(ctx context.Context) ([]gen.Approval, error) {
	var out struct {
		Approvals []gen.Approval `json:"approvals"`
	}
	err := c.do(ctx, http.MethodGet, "/approvals", nil, &out)
	return out.Approvals, err
}

func (c *Client) ResolveApproval(ctx context.Context, id string, in gen.ApprovalResolve) (gen.Approval, error) {
	var out gen.Approval
	err := c.do(ctx, http.MethodPost, "/approvals/"+url.PathEscape(id), in, &out)
	return out, err
}

// ListTraces summarises the traces of one session, newest first. It is what a
// client walks to find the trace it then reads whole with GetTrace.
func (c *Client) ListTraces(ctx context.Context, sessionID string) ([]gen.TraceSummary, error) {
	var out struct {
		Traces []gen.TraceSummary `json:"traces"`
	}
	err := c.do(ctx, http.MethodGet, "/sessions/"+url.PathEscape(sessionID)+"/traces", nil, &out)
	return out.Traces, err
}

func (c *Client) GetTrace(ctx context.Context, id string) (gen.Trace, error) {
	var out gen.Trace
	err := c.do(ctx, http.MethodGet, "/traces/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *Client) CreateToken(ctx context.Context, in gen.TokenCreate) (gen.TokenCreated, error) {
	var out gen.TokenCreated
	err := c.do(ctx, http.MethodPost, "/tokens", in, &out)
	return out, err
}

// ListTokens lists the active tokens, or every one with All, narrowed by
// params.
func (c *Client) ListTokens(ctx context.Context, params gen.ListTokensParams) ([]gen.Token, error) {
	q := url.Values{}
	if params.Identity != nil && *params.Identity != "" {
		q.Set("identity", *params.Identity)
	}
	if params.Binding != nil && *params.Binding != "" {
		q.Set("binding", *params.Binding)
	}
	if params.All != nil && *params.All {
		q.Set("all", "true")
	}
	path := "/tokens"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out struct {
		Tokens []gen.Token `json:"tokens"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Tokens, err
}

// UpdateToken sets a token's scope.
func (c *Client) UpdateToken(ctx context.Context, id string, in gen.TokenUpdate) (gen.Token, error) {
	var out gen.Token
	err := c.do(ctx, http.MethodPatch, "/tokens/"+url.PathEscape(id), in, &out)
	return out, err
}

// RotateToken replaces a token; the new secret is in the answer, once.
func (c *Client) RotateToken(ctx context.Context, id string) (gen.TokenCreated, error) {
	var out gen.TokenCreated
	err := c.do(ctx, http.MethodPost, "/tokens/"+url.PathEscape(id)+"/rotate", nil, &out)
	return out, err
}

func (c *Client) RevokeToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/tokens/"+url.PathEscape(id), nil, nil)
}

func (c *Client) ListChannels(ctx context.Context) ([]gen.ChannelStatus, error) {
	var out struct {
		Channels []gen.ChannelStatus `json:"channels"`
	}
	err := c.do(ctx, http.MethodGet, "/channels", nil, &out)
	return out.Channels, err
}

// ListAgents is the configured agent list, which is where a client that has
// to pick an agent starts.
func (c *Client) ListAgents(ctx context.Context) ([]gen.AgentInfo, error) {
	var out struct {
		Agents []gen.AgentInfo `json:"agents"`
	}
	err := c.do(ctx, http.MethodGet, "/agents", nil, &out)
	return out.Agents, err
}

// SearchMemory reads one partition of one agent; both are mandatory, the store
// is default-deny across partitions.
func (c *Client) SearchMemory(ctx context.Context, params gen.SearchMemoryParams) ([]gen.MemoryItem, error) {
	q := url.Values{"agent": {params.Agent}, "partition": {params.Partition}}
	if params.Q != nil && *params.Q != "" {
		q.Set("q", *params.Q)
	}
	if params.Layer != nil && *params.Layer != "" {
		q.Set("layer", string(*params.Layer))
	}
	if params.Limit != nil && *params.Limit > 0 {
		q.Set("limit", strconv.Itoa(*params.Limit))
	}
	var out struct {
		Items []gen.MemoryItem `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/memory?"+q.Encode(), nil, &out)
	return out.Items, err
}

// ListMemoryPartitions names the memory spaces one agent holds, so a client
// can offer them instead of asking somebody to spell one out.
func (c *Client) ListMemoryPartitions(ctx context.Context, agent string) ([]gen.MemoryPartition, error) {
	q := url.Values{"agent": {agent}}
	var out struct {
		Partitions []gen.MemoryPartition `json:"partitions"`
	}
	err := c.do(ctx, http.MethodGet, "/memory/partitions?"+q.Encode(), nil, &out)
	return out.Partitions, err
}

func (c *Client) WriteMemory(ctx context.Context, in gen.MemoryWrite) (gen.MemoryItem, error) {
	var out gen.MemoryItem
	err := c.do(ctx, http.MethodPost, "/memory", in, &out)
	return out, err
}

func (c *Client) ForgetMemory(ctx context.Context, id, agent, partition string) error {
	q := url.Values{"agent": {agent}, "partition": {partition}}
	return c.do(ctx, http.MethodDelete, "/memory/"+url.PathEscape(id)+"?"+q.Encode(), nil, nil)
}

// ListSkills is every skill of one agent, with its eligibility, and the ones
// in the trash.
func (c *Client) ListSkills(ctx context.Context, agent string) ([]gen.Skill, error) {
	q := url.Values{"agent": {agent}}
	var out struct {
		Skills []gen.Skill `json:"skills"`
	}
	err := c.do(ctx, http.MethodGet, "/skills?"+q.Encode(), nil, &out)
	return out.Skills, err
}

// InstallSkill previews or installs a skill from a source (docs/skills.md §7):
// 200 for a preview, 201 for the install.
func (c *Client) InstallSkill(ctx context.Context, in gen.SkillInstall) (gen.SkillInstalled, error) {
	var out gen.SkillInstalled
	err := c.do(ctx, http.MethodPost, "/skills", in, &out)
	return out, err
}

// UploadSkill sends the gzipped tar r of a skill folder read at path, for
// InstallSkill to name by the answer's id (docs/skills.md §7).
func (c *Client) UploadSkill(ctx context.Context, r io.Reader, path string) (gen.SkillUpload, error) {
	p := "/skills/upload"
	if path != "" {
		p += "?" + url.Values{"path": {path}}.Encode()
	}
	req, err := c.request(ctx, http.MethodPost, p, nil)
	if err != nil {
		return gen.SkillUpload{}, err
	}
	req.Body = io.NopCloser(r)
	req.Header.Set("Content-Type", "application/gzip")
	var out gen.SkillUpload
	err = c.send(req, http.MethodPost, p, &out)
	return out, err
}

// skillPath addresses one skill of an agent by name, with an optional tail
// ("/restore", "/revert").
func skillPath(name, agent, tail string) string {
	return "/skills/" + url.PathEscape(name) + tail + "?" + url.Values{"agent": {agent}}.Encode()
}

// GetSkill is one skill as it is now, with its body and, for an agent one,
// its live number and versions.
func (c *Client) GetSkill(ctx context.Context, name, agent string) (gen.Skill, error) {
	var out gen.Skill
	err := c.do(ctx, http.MethodGet, skillPath(name, agent, ""), nil, &out)
	return out, err
}

// GetSkillVersion is GetSkill for one of an agent skill's versions, the live
// number included: the body, raw file and learned block are that version's,
// the versions list is still the full one.
func (c *Client) GetSkillVersion(ctx context.Context, name, agent string, version int) (gen.Skill, error) {
	var out gen.Skill
	err := c.do(ctx, http.MethodGet, skillPath(name, agent, "")+"&version="+strconv.Itoa(version), nil, &out)
	return out, err
}

// RemoveSkill moves a live agent skill to the trash, or deletes one already
// there; a bundled skill is refused.
func (c *Client) RemoveSkill(ctx context.Context, name, agent string) error {
	return c.do(ctx, http.MethodDelete, skillPath(name, agent, ""), nil, nil)
}

// RestoreSkill moves an agent skill out of the trash.
func (c *Client) RestoreSkill(ctx context.Context, name, agent string) (gen.Skill, error) {
	var out gen.Skill
	err := c.do(ctx, http.MethodPost, skillPath(name, agent, "/restore"), nil, &out)
	return out, err
}

// RevertSkill copies version to of an agent skill into a new current one;
// the live number is a no-op.
func (c *Client) RevertSkill(ctx context.Context, name, agent string, to int) (gen.Skill, error) {
	var out gen.Skill
	err := c.do(ctx, http.MethodPost, skillPath(name, agent, "/revert"), gen.SkillRevert{To: &to}, &out)
	return out, err
}

func (c *Client) CreateInvite(ctx context.Context, in gen.InviteCreate) (gen.Invite, error) {
	var out gen.Invite
	err := c.do(ctx, http.MethodPost, "/invites", in, &out)
	return out, err
}

// PutCredentials writes provider secrets. Nothing comes back: credentials are
// write-only (ARCHITECTURE.md §4).
func (c *Client) PutCredentials(ctx context.Context, provider string, kv map[string]string) error {
	return c.do(ctx, http.MethodPut, "/credentials/"+url.PathEscape(provider), gen.CredentialsPut(kv), nil)
}

// ListIdentities lists identities with their bindings; pending keeps only the
// ones nobody has confirmed.
func (c *Client) ListIdentities(ctx context.Context, pending bool) ([]gen.User, error) {
	var out struct {
		Identities []gen.User `json:"identities"`
	}
	path := "/identities"
	if pending {
		path += "?pending=true"
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Identities, err
}

func (c *Client) GetIdentity(ctx context.Context, id string) (gen.User, error) {
	var out gen.User
	err := c.do(ctx, http.MethodGet, "/identities/"+url.PathEscape(id), nil, &out)
	return out, err
}

// UpdateIdentity confirms a pending identity, or renames one or changes its
// role.
func (c *Client) UpdateIdentity(ctx context.Context, id string, in gen.IdentityUpdate) (gen.User, error) {
	var out gen.User
	err := c.do(ctx, http.MethodPatch, "/identities/"+url.PathEscape(id), in, &out)
	return out, err
}

// CreateIdentity makes a confirmed identity with no bindings. An empty role
// is the server's default, user.
func (c *Client) CreateIdentity(ctx context.Context, name, role string) (gen.User, error) {
	in := gen.IdentityCreate{Name: name}
	if role != "" {
		in.Role = ptr(gen.IdentityCreateRole(role))
	}
	var out gen.User
	err := c.do(ctx, http.MethodPost, "/identities", in, &out)
	return out, err
}

// AssignIdentity moves a pending identity onto the confirmed identity to.
func (c *Client) AssignIdentity(ctx context.Context, id, to string) (gen.IdentityAssignment, error) {
	var out gen.IdentityAssignment
	err := c.do(ctx, http.MethodPost, "/identities/"+url.PathEscape(id)+"/assign", gen.IdentityAssign{To: to}, &out)
	return out, err
}

// DisableIdentity switches a confirmed identity off.
func (c *Client) DisableIdentity(ctx context.Context, id string) (gen.User, error) {
	var out gen.User
	err := c.do(ctx, http.MethodPost, "/identities/"+url.PathEscape(id)+"/disable", nil, &out)
	return out, err
}

// EnableIdentity switches a disabled identity back on.
func (c *Client) EnableIdentity(ctx context.Context, id string) (gen.User, error) {
	var out gen.User
	err := c.do(ctx, http.MethodPost, "/identities/"+url.PathEscape(id)+"/enable", nil, &out)
	return out, err
}

// Unbind removes the binding channel:alias from whichever identity holds it.
func (c *Client) Unbind(ctx context.Context, channel, alias string) (gen.BindingUnbound, error) {
	var out gen.BindingUnbound
	err := c.do(ctx, http.MethodDelete, "/bindings/"+url.PathEscape(channel+":"+alias), nil, &out)
	return out, err
}

// DeleteIdentity removes an identity, forgetting its own memory unless
// keepMemory.
func (c *Client) DeleteIdentity(ctx context.Context, id string, keepMemory bool) (gen.IdentityDeleted, error) {
	var out gen.IdentityDeleted
	path := "/identities/" + url.PathEscape(id)
	if keepMemory {
		path += "?keep_memory=true"
	}
	err := c.do(ctx, http.MethodDelete, path, nil, &out)
	return out, err
}

// Events opens the SSE stream. focus and lastID may be empty; lastID resumes
// where a previous stream stopped.
func (c *Client) Events(ctx context.Context, focus, lastID string) (*EventStream, error) {
	path := "/events"
	if focus != "" {
		path += "?focus=" + url.QueryEscape(focus)
	}
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("api: events: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, responseError(resp)
	}
	return &EventStream{body: resp.Body, r: bufio.NewReader(resp.Body), lastID: lastID}, nil
}

// EventStream iterates the events of one stream. Next blocks until the next
// event arrives; heartbeats are consumed silently.
type EventStream struct {
	body   io.ReadCloser
	r      *bufio.Reader
	lastID string
}

// LastID is the id to resume from after a disconnect.
func (s *EventStream) LastID() string { return s.lastID }

func (s *EventStream) Close() error { return s.body.Close() }

// Next returns the next event, or io.EOF when the stream ended.
func (s *EventStream) Next() (gen.Event, error) {
	var (
		id   string
		data []byte
	)
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			if len(line) == 0 {
				return gen.Event{}, err
			}
			return gen.Event{}, io.EOF
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if len(data) == 0 {
				continue // a heartbeat or an empty frame
			}
			var e gen.Event
			if err := json.Unmarshal(data, &e); err != nil {
				return gen.Event{}, fmt.Errorf("api: bad event payload: %w", err)
			}
			if id != "" {
				s.lastID = id
			}
			return e, nil
		case strings.HasPrefix(line, ":"):
			continue // comment: the heartbeat
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: ")...)
		}
	}
}

// --- plumbing -----------------------------------------------------------------

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("api: encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("api: request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	return c.send(req, method, path, out)
}

// send runs req and decodes a 2xx answer into out, or returns the error
// body as a ClientError.
func (c *Client) send(req *http.Request, method, path string, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("api: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return responseError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("api: decode response: %w", err)
	}
	return nil
}

// responseError renders an error body; a body that is not the error model
// still yields the status, never the raw payload.
func responseError(resp *http.Response) error {
	e := &ClientError{Status: resp.StatusCode, Code: CodeInternal, Message: http.StatusText(resp.StatusCode)}
	var body gen.Error
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxJSONBody)).Decode(&body); err == nil && body.Code != "" {
		e.Code, e.Message = body.Code, body.Message
	}
	return e
}

// ProviderModels lists the model ids a provider serves. A *ClientError with
// Status 404 is an unknown provider, 501 one that cannot list its models and
// 502 a list that could not be read.
func (c *Client) ProviderModels(ctx context.Context, provider string) ([]string, error) {
	var out struct {
		Models []string `json:"models"`
	}
	err := c.do(ctx, http.MethodGet, "/providers/"+url.PathEscape(provider)+"/models", nil, &out)
	return out.Models, err
}

// StartProviderLogin begins a device-code login (Codex) in the daemon.
func (c *Client) StartProviderLogin(ctx context.Context, provider string) (gen.DeviceLogin, error) {
	var out gen.DeviceLogin
	err := c.do(ctx, http.MethodPost, "/credentials/"+url.PathEscape(provider)+"/login", nil, &out)
	return out, err
}

// ProviderLogin reports the state of a device-code login.
func (c *Client) ProviderLogin(ctx context.Context, provider string) (gen.DeviceLogin, error) {
	var out gen.DeviceLogin
	err := c.do(ctx, http.MethodGet, "/credentials/"+url.PathEscape(provider)+"/login", nil, &out)
	return out, err
}

// StartChannelLogin begins a pairing-code login (WhatsApp) for the account of
// phone and returns once the code is there. A *ClientError with Status 404 is
// an unknown channel, 501 one that has no interactive login.
func (c *Client) StartChannelLogin(ctx context.Context, name, phone string) (gen.DeviceLogin, error) {
	var out gen.DeviceLogin
	err := c.do(ctx, http.MethodPost, "/channels/"+url.PathEscape(name)+"/login", gen.ChannelLoginStart{Phone: phone}, &out)
	return out, err
}

// ChannelLogin reports the state of a channel login.
func (c *Client) ChannelLogin(ctx context.Context, name string) (gen.DeviceLogin, error) {
	var out gen.DeviceLogin
	err := c.do(ctx, http.MethodGet, "/channels/"+url.PathEscape(name)+"/login", nil, &out)
	return out, err
}

// ChannelLogout unlinks a channel from its account.
func (c *Client) ChannelLogout(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/channels/"+url.PathEscape(name)+"/login", nil, nil)
}

// --- backups ------------------------------------------------------------------

// ListBackups returns the managed backups, newest first.
func (c *Client) ListBackups(ctx context.Context) ([]gen.Backup, error) {
	var out struct {
		Backups []gen.Backup `json:"backups"`
	}
	err := c.do(ctx, http.MethodGet, "/backups", nil, &out)
	return out.Backups, err
}

// CreateBackup requests a backup; the record comes back still running.
func (c *Client) CreateBackup(ctx context.Context, in gen.BackupCreate) (gen.Backup, error) {
	var out gen.Backup
	err := c.do(ctx, http.MethodPost, "/backups", in, &out)
	return out, err
}

func (c *Client) GetBackup(ctx context.Context, id string) (gen.Backup, error) {
	var out gen.Backup
	err := c.do(ctx, http.MethodGet, "/backups/"+url.PathEscape(id), nil, &out)
	return out, err
}

// DownloadBackup copies the archive of a ready backup into w and returns how
// many bytes it wrote. A stream that ends short of its Content-Length is an
// error: half an archive must never pass for a whole one.
func (c *Client) DownloadBackup(ctx context.Context, id string, w io.Writer) (int64, error) {
	path := "/backups/" + url.PathEscape(id) + "/content"
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("api: GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, responseError(resp)
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, fmt.Errorf("api: GET %s: %w", path, err)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return n, fmt.Errorf("api: GET %s: got %d of %d bytes", path, n, resp.ContentLength)
	}
	return n, nil
}

// VerifyBackup has the daemon re-check an archive against its manifest.
func (c *Client) VerifyBackup(ctx context.Context, id string) (gen.BackupVerification, error) {
	var out gen.BackupVerification
	err := c.do(ctx, http.MethodPost, "/backups/"+url.PathEscape(id)+"/verify", nil, &out)
	return out, err
}

// UploadBackup sends the archive r for the daemon to keep.
func (c *Client) UploadBackup(ctx context.Context, r io.Reader) (gen.Backup, error) {
	req, err := c.request(ctx, http.MethodPost, "/backups/upload", nil)
	if err != nil {
		return gen.Backup{}, err
	}
	req.Body = io.NopCloser(r)
	req.Header.Set("Content-Type", "application/gzip")
	var out gen.Backup
	err = c.send(req, http.MethodPost, "/backups/upload", &out)
	return out, err
}

// RestoreBackup has the daemon restart onto the data of a backup. A nil
// error means it is on its way down; it is back within seconds.
func (c *Client) RestoreBackup(ctx context.Context, id string) (gen.Backup, error) {
	var out gen.Backup
	err := c.do(ctx, http.MethodPost, "/backups/"+url.PathEscape(id)+"/restore", nil, &out)
	return out, err
}

func (c *Client) DeleteBackup(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/backups/"+url.PathEscape(id), nil, nil)
}

// --- connectors ---------------------------------------------------------------

// ListConnectors is every connector, by id.
func (c *Client) ListConnectors(ctx context.Context) ([]gen.Connector, error) {
	var out struct {
		Connectors []gen.Connector `json:"connectors"`
	}
	err := c.do(ctx, http.MethodGet, "/connectors", nil, &out)
	return out.Connectors, err
}

// GetConnector is one connector with its tools.
func (c *Client) GetConnector(ctx context.Context, id string) (gen.ConnectorDetail, error) {
	var out gen.ConnectorDetail
	err := c.do(ctx, http.MethodGet, "/connectors/"+url.PathEscape(id), nil, &out)
	return out, err
}

// CreateConnector opens a connector. An answer without AuthorizationUrl is
// an open server, connected already.
func (c *Client) CreateConnector(ctx context.Context, in gen.ConnectorCreate) (gen.ConnectorStarted, error) {
	var out gen.ConnectorStarted
	err := c.do(ctx, http.MethodPost, "/connectors", in, &out)
	return out, err
}

// ReconnectConnector opens a new authorization on a connector, answering
// like CreateConnector.
func (c *Client) ReconnectConnector(ctx context.Context, id string, in gen.ConnectorReconnect) (gen.ConnectorStarted, error) {
	var out gen.ConnectorStarted
	err := c.do(ctx, http.MethodPost, "/connectors/"+url.PathEscape(id)+"/reconnect", in, &out)
	return out, err
}

// InspectConnector asks the server at serverURL what it is, without
// signing in. Nothing is stored.
func (c *Client) InspectConnector(ctx context.Context, serverURL string) (gen.ConnectorInspection, error) {
	var out gen.ConnectorInspection
	err := c.do(ctx, http.MethodPost, "/connectors/inspect", gen.ConnectorInspect{Url: serverURL}, &out)
	return out, err
}

// PostConnectorCode finishes the open authorization of a connector with the
// redirect's code and state.
func (c *Client) PostConnectorCode(ctx context.Context, id string, in gen.ConnectorCode) (gen.Connector, error) {
	var out gen.Connector
	err := c.do(ctx, http.MethodPost, "/connectors/"+url.PathEscape(id)+"/code", in, &out)
	return out, err
}

// DeleteConnector removes a connector in any state.
func (c *Client) DeleteConnector(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/connectors/"+url.PathEscape(id), nil, nil)
}
