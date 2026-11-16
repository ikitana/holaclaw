package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/skill"
)

// recordingSkills is a SkillBackend that records its last call and answers
// err when set.
type recordingSkills struct {
	calls []string
	who   event.ID
	err   error
}

func (s *recordingSkills) record(format string, a ...any) {
	s.calls = append(s.calls, fmt.Sprintf(format, a...))
}

func (s *recordingSkills) last() string {
	if len(s.calls) == 0 {
		return ""
	}
	return s.calls[len(s.calls)-1]
}

func (s *recordingSkills) ListSkills(_ context.Context, agent string) ([]gen.Skill, error) {
	s.record("list %s", agent)
	off := gen.Skill{Name: "off", Level: gen.SkillLevelAgent}
	off.Hidden = &struct {
		Code    *string               `json:"code,omitempty"`
		Message *string               `json:"message,omitempty"`
		Reason  gen.SkillHiddenReason `json:"reason"`
		Subject *string               `json:"subject,omitempty"`
	}{Reason: gen.SkillHiddenReasonInvalid, Code: ptr("SKL101"), Message: ptr("no frontmatter")}
	return []gen.Skill{{Name: "plain", Level: gen.SkillLevelAgent, Eligible: true}, off}, s.err
}

func (s *recordingSkills) GetSkill(_ context.Context, agent, id string) (gen.Skill, error) {
	s.record("get %s %s", agent, id)
	out := gen.Skill{Name: id, Level: gen.SkillLevelAgent, Body: ptr("# " + id), Version: ptr(5)}
	out.Learned = &struct {
		By     string                 `json:"by"`
		ByName *string                `json:"by_name,omitempty"`
		From   string                 `json:"from"`
		On     time.Time              `json:"on"`
		Origin gen.SkillLearnedOrigin `json:"origin"`
	}{By: "U1", ByName: ptr("Angel"), From: "T1", On: time.Date(2026, 9, 12, 14, 3, 5, 0, time.UTC), Origin: gen.Correction}
	return out, s.err
}

func (s *recordingSkills) GetSkillVersion(_ context.Context, agent, id string, version int) (gen.Skill, error) {
	s.record("get %s %s @%d", agent, id, version)
	return gen.Skill{Name: id, Level: gen.SkillLevelAgent, Version: &version}, s.err
}

func (s *recordingSkills) RemoveSkill(_ context.Context, agent, id string, who event.ID) error {
	s.record("remove %s %s", agent, id)
	s.who = who
	return s.err
}

func (s *recordingSkills) RestoreSkill(_ context.Context, agent, id string, who event.ID) (gen.Skill, error) {
	s.record("restore %s %s", agent, id)
	s.who = who
	return gen.Skill{Name: id, Level: gen.SkillLevelAgent}, s.err
}

func (s *recordingSkills) RevertSkill(_ context.Context, agent, id string, to *int, who event.ID) (gen.Skill, error) {
	if to == nil {
		s.record("revert %s %s", agent, id)
	} else {
		s.record("revert %s %s %d", agent, id, *to)
	}
	s.who = who
	return gen.Skill{Name: id, Level: gen.SkillLevelAgent}, s.err
}

func skillsHarness(t *testing.T) (*harness, *recordingSkills) {
	t.Helper()
	h := newHarness(t)
	rec := &recordingSkills{}
	h.b.Skills = rec
	return h, rec
}

func TestSkillsHappyPaths(t *testing.T) {
	h, rec := skillsHarness(t)

	list := decode[struct {
		Skills []gen.Skill `json:"skills"`
	}](t, h.do(http.MethodGet, "/skills?agent=main", h.ownerToken, nil), http.StatusOK)
	if len(list.Skills) != 2 || rec.last() != "list main" {
		t.Fatalf("list = %+v, call %q", list, rec.last())
	}
	// Why a skill is hidden reaches the wire typed.
	if h := list.Skills[1].Hidden; list.Skills[0].Hidden != nil || h == nil || h.Reason != gen.SkillHiddenReasonInvalid ||
		h.Code == nil || *h.Code != "SKL101" || h.Message == nil || *h.Message != "no frontmatter" {
		t.Fatalf("hidden = %+v", list.Skills[1].Hidden)
	}

	sk := decode[gen.Skill](t, h.do(http.MethodGet, "/skills/deploy?agent=main", h.ownerToken, nil), http.StatusOK)
	if sk.Name != "deploy" || rec.last() != "get main deploy" || sk.Version == nil || *sk.Version != 5 {
		t.Fatalf("get = %+v, call %q", sk, rec.last())
	}
	// Who taught it reaches the wire: the identity and its display name.
	if l := sk.Learned; l == nil || l.By != "U1" || l.ByName == nil || *l.ByName != "Angel" {
		t.Fatalf("learned = %+v", sk.Learned)
	}
	// Any version number goes to the backend, the live one included.
	for _, v := range []int{4, 5} {
		sk = decode[gen.Skill](t, h.do(http.MethodGet, fmt.Sprintf("/skills/deploy?agent=main&version=%d", v), h.ownerToken, nil), http.StatusOK)
		if rec.last() != fmt.Sprintf("get main deploy @%d", v) || sk.Version == nil || *sk.Version != v {
			t.Fatalf("get with version %d: %q, %+v", v, rec.last(), sk.Version)
		}
	}
	calls := len(rec.calls)
	for _, v := range []string{"0", "-1", "v2"} {
		errorBody(t, h.do(http.MethodGet, "/skills/deploy?agent=main&version="+v, h.ownerToken, nil), http.StatusBadRequest, CodeBadRequest)
	}
	if len(rec.calls) != calls {
		t.Fatalf("a bad version reached the backend: %v", rec.calls[calls:])
	}

	decode[struct{}](t, h.do(http.MethodDelete, "/skills/deploy?agent=main", h.adminToken, nil), http.StatusNoContent)
	if rec.last() != "remove main deploy" || rec.who != h.admin.ID {
		t.Fatalf("remove: %q who %q", rec.last(), rec.who)
	}

	decode[gen.Skill](t, h.do(http.MethodPost, "/skills/deploy/restore?agent=main", h.adminToken, nil), http.StatusOK)
	if rec.last() != "restore main deploy" {
		t.Fatalf("restore: %q", rec.last())
	}

	decode[gen.Skill](t, h.do(http.MethodPost, "/skills/deploy/revert?agent=main", h.adminToken, nil), http.StatusOK)
	if rec.last() != "revert main deploy" {
		t.Fatalf("revert without body: %q", rec.last())
	}
	to := 3
	decode[gen.Skill](t, h.do(http.MethodPost, "/skills/deploy/revert?agent=main", h.adminToken, gen.SkillRevert{To: &to}), http.StatusOK)
	if rec.last() != "revert main deploy 3" {
		t.Fatalf("revert to: %q", rec.last())
	}
}

func TestSkillsNotImplemented(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/skills?agent=main"},
		{http.MethodGet, "/skills/x?agent=main"},
		{http.MethodDelete, "/skills/x?agent=main"},
		{http.MethodPost, "/skills/x/restore?agent=main"},
		{http.MethodPost, "/skills/x/revert?agent=main"},
		{http.MethodPost, "/skills/upload"},
	} {
		errorBody(t, h.do(c.method, c.path, h.adminToken, nil), http.StatusNotImplemented, CodeNotImplemented)
	}
}

func TestSkillsBadRequests(t *testing.T) {
	h, rec := skillsHarness(t)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/skills?agent=", nil},
		{http.MethodGet, "/skills/x?agent=", nil},
		{http.MethodDelete, "/skills/x?agent=", nil},
		{http.MethodPost, "/skills/x/restore?agent=", nil},
		{http.MethodPost, "/skills/x/revert?agent=", nil},
		{http.MethodPost, "/skills/x/revert?agent=main", map[string]any{"too": "x"}},
	} {
		errorBody(t, h.do(c.method, c.path, h.adminToken, c.body), http.StatusBadRequest, CodeBadRequest)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("the backend was called: %v", rec.calls)
	}
}

func TestSkillsRevertRejectsMalformedBody(t *testing.T) {
	h, rec := skillsHarness(t)
	for _, body := range []string{"{", `{"to":0}`, `{"to":-2}`, `{"to":"1.md"}`} {
		req, err := http.NewRequest(http.MethodPost, h.srv.URL+BaseURL+"/skills/x/revert?agent=main", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+h.adminToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		errorBody(t, resp, http.StatusBadRequest, CodeBadRequest)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("the backend was called: %v", rec.calls)
	}
}

// A token bound to a user reads only agents that user may use; any other is
// a not_found, never a forbidden.
func TestSkillsUserBoundToken(t *testing.T) {
	h, rec := skillsHarness(t)
	guest, err := h.auth.EnsureGuest(context.Background(), "telegram", []string{"gus"}, "gus")
	if err != nil {
		t.Fatal(err)
	}
	guestToken := h.token("cli-guest", auth.ScopeClient, guest.ID)
	for _, path := range []string{"/skills?agent=home", "/skills/x?agent=home"} {
		errorBody(t, h.do(http.MethodGet, path, h.otherToken, nil), http.StatusNotFound, CodeNotFound)
	}
	errorBody(t, h.do(http.MethodGet, "/skills?agent=main", guestToken, nil), http.StatusNotFound, CodeNotFound)
	if len(rec.calls) != 0 {
		t.Fatalf("the backend was called: %v", rec.calls)
	}
	decode[struct{}](t, h.do(http.MethodGet, "/skills?agent=main", h.otherToken, nil), http.StatusOK)
	// Admin sees any agent; the backend decides whether it exists.
	decode[struct{}](t, h.do(http.MethodGet, "/skills?agent=home", h.adminToken, nil), http.StatusOK)
	// Writes are admin scope.
	errorBody(t, h.do(http.MethodDelete, "/skills/x?agent=main", h.ownerToken, nil), http.StatusForbidden, CodeForbidden)
}

func TestSkillsErrorMapping(t *testing.T) {
	h, rec := skillsHarness(t)
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("x: %w", skill.ErrNotFound), http.StatusNotFound, CodeNotFound},
		{fmt.Errorf("x: %w", skill.ErrExists), http.StatusConflict, CodeConflict},
		{fmt.Errorf("x: %w", skill.ErrBundled), http.StatusForbidden, CodeForbidden},
		{fmt.Errorf("x: %w", skill.ErrBadName), http.StatusBadRequest, CodeBadRequest},
		{skill.Problem{Code: skill.CodeUnsafePath, Dir: "/skills/x", Msg: "history is a symlink"}, http.StatusConflict, CodeConflict},
		{errors.New("disk on fire"), http.StatusInternalServerError, CodeInternal},
	} {
		rec.err = c.err
		for _, r := range []struct{ method, path string }{
			{http.MethodGet, "/skills?agent=main"},
			{http.MethodGet, "/skills/x?agent=main"},
			{http.MethodDelete, "/skills/x?agent=main"},
			{http.MethodPost, "/skills/x/restore?agent=main"},
			{http.MethodPost, "/skills/x/revert?agent=main"},
		} {
			resp := h.do(r.method, r.path, h.adminToken, nil)
			e := decode[gen.Error](t, resp, c.status)
			if e.Code != c.code {
				t.Errorf("%v on %s %s: code %q", c.err, r.method, r.path, e.Code)
			}
			if c.code == CodeInternal && e.Message != "internal error" {
				t.Errorf("internal message leaked: %q", e.Message)
			}
		}
	}
	rec.err = skill.ErrBundled
	e := decode[gen.Error](t, h.do(http.MethodDelete, "/skills/plain?agent=main", h.adminToken, nil), http.StatusForbidden)
	if e.Message != "a bundled skill is never written" {
		t.Errorf("message = %q", e.Message)
	}
}

// who is the acting identity: the token's binding's, or the owner's for the socket.
func TestSkillsWho(t *testing.T) {
	h, rec := skillsHarness(t)
	userAdmin := h.token("admin-owner", auth.ScopeAdmin, h.owner.ID)
	decode[gen.Skill](t, h.do(http.MethodPost, "/skills/x/restore?agent=main", userAdmin, nil), http.StatusOK)
	if rec.who != h.owner.ID {
		t.Errorf("user-bound admin: who %q, want %q", rec.who, h.owner.ID)
	}
	res := h.socket(http.MethodPost, "/skills/x/revert?agent=main", nil)
	if res.Code != http.StatusOK || rec.who != h.owner.ID {
		t.Errorf("socket: %d who %q", res.Code, rec.who)
	}
	decode[struct{}](t, h.do(http.MethodDelete, "/skills/x?agent=main", h.adminToken, nil), http.StatusNoContent)
	if rec.who != h.admin.ID {
		t.Errorf("admin token: who %q", rec.who)
	}
}

func (s *recordingSkills) InstallSkill(_ context.Context, in gen.SkillInstall, who event.ID) (gen.SkillInstalled, error) {
	s.record("install %s git=%s upload=%s name=%s preview=%v force=%v hash=%s", in.Agent, gitString(in.Git), deref(in.Upload), deref(in.Name), in.Preview != nil && *in.Preview, in.Force != nil && *in.Force, deref(in.Hash))
	s.who = who
	return gen.SkillInstalled{
		Skill:     &gen.Skill{Name: deref(in.Name), Level: gen.SkillLevelAgent},
		Findings:  []gen.ScanFinding{},
		Hash:      "sha256:00",
		Installed: in.Preview == nil || !*in.Preview,
	}, s.err
}

// gitString renders a git source for the recording backend.
func gitString(g *gen.SkillGitSource) string {
	if g == nil {
		return ""
	}
	return g.Url + "@" + deref(g.Ref) + ":" + deref(g.Path)
}

func TestSkillsInstall(t *testing.T) {
	h, rec := skillsHarness(t)
	git := &gen.SkillGitSource{Url: "ssh://h/acme/skills", Ref: ptr("v1"), Path: ptr("release")}
	prev := decode[gen.SkillInstalled](t, h.do(http.MethodPost, "/skills", h.adminToken,
		gen.SkillInstall{Agent: "main", Git: git, Preview: ptr(true)}), http.StatusOK)
	if prev.Installed || rec.last() != "install main git=ssh://h/acme/skills@v1:release upload= name= preview=true force=false hash=" {
		t.Fatalf("preview = %+v, call %q", prev, rec.last())
	}
	// No ref: the repository's default branch.
	decode[gen.SkillInstalled](t, h.do(http.MethodPost, "/skills", h.adminToken,
		map[string]any{"agent": "main", "git": map[string]any{"url": "ssh://h/acme/skills"}, "preview": true}), http.StatusOK)
	if rec.last() != "install main git=ssh://h/acme/skills@: upload= name= preview=true force=false hash=" {
		t.Fatalf("preview without a ref: call %q", rec.last())
	}
	done := decode[gen.SkillInstalled](t, h.do(http.MethodPost, "/skills", h.adminToken,
		gen.SkillInstall{Agent: "main", Upload: ptr("01UPLOAD"), Name: ptr("ship"), Force: ptr(true), Hash: ptr("sha256:00")}), http.StatusCreated)
	if !done.Installed || rec.last() != "install main git= upload=01UPLOAD name=ship preview=false force=true hash=sha256:00" || rec.who != h.admin.ID {
		t.Fatalf("install = %+v, call %q, who %q", done, rec.last(), rec.who)
	}

	n := len(rec.calls)
	for _, body := range []any{
		gen.SkillInstall{Agent: "main"},
		gen.SkillInstall{Agent: "main", Git: git, Upload: ptr("01UPLOAD")},
		gen.SkillInstall{Git: git},
		map[string]any{"agent": "main", "upload": "01UPLOAD", "extra": 1},
	} {
		errorBody(t, h.do(http.MethodPost, "/skills", h.adminToken, body), http.StatusBadRequest, CodeBadRequest)
	}
	errorBody(t, h.do(http.MethodPost, "/skills", h.ownerToken, gen.SkillInstall{Agent: "main", Git: git}), http.StatusForbidden, CodeForbidden)
	if len(rec.calls) != n {
		t.Fatalf("the backend was called: %v", rec.calls[n:])
	}

	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("x: %w", skill.ErrUnsafe), http.StatusConflict, CodeConflict},
		{fmt.Errorf("x: %w", skill.ErrChanged), http.StatusConflict, CodeConflict},
		{fmt.Errorf("x: %w", skill.ErrExists), http.StatusConflict, CodeConflict},
		{fmt.Errorf("%w: no SKILL.md", skill.ErrBadSource), http.StatusBadRequest, CodeBadRequest},
		{fmt.Errorf("%w: pass all", skill.ErrCollection), http.StatusBadRequest, CodeBadRequest},
		{fmt.Errorf("x: %w", skill.ErrNotFound), http.StatusNotFound, CodeNotFound},
	} {
		rec.err = c.err
		e := decode[gen.Error](t, h.do(http.MethodPost, "/skills", h.adminToken, gen.SkillInstall{Agent: "main", Git: git}), c.status)
		if e.Code != c.code {
			t.Errorf("%v: code %q", c.err, e.Code)
		}
	}
}

func (s *recordingSkills) UploadSkill(_ context.Context, r io.Reader, path string, who event.ID) (gen.SkillUpload, error) {
	n, err := io.Copy(io.Discard, r)
	s.record("upload %s %d bytes", path, n)
	s.who = who
	if err != nil {
		return gen.SkillUpload{}, err
	}
	return gen.SkillUpload{Id: "01UPLOAD", ExpiresAt: time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC)}, s.err
}

// postRaw posts body as a gzipped tar to path.
func (h *harness) postRaw(path, token string, body io.Reader) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+BaseURL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// zeros reads as many zero bytes as asked.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// The client asks for the live skill with no version, for a version by its
// number whatever it is, and always names the version a revert goes to.
func TestSkillsClientVersions(t *testing.T) {
	h, rec := skillsHarness(t)
	c, err := Dial(h.srv.URL, h.adminToken)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if s, err := c.GetSkill(ctx, "deploy", "main"); err != nil || rec.last() != "get main deploy" || *s.Version != 5 {
		t.Fatalf("GetSkill: %v, call %q", err, rec.last())
	}
	if s, err := c.GetSkillVersion(ctx, "deploy", "main", 5); err != nil || rec.last() != "get main deploy @5" || *s.Version != 5 {
		t.Fatalf("GetSkillVersion: %v, call %q", err, rec.last())
	}
	calls := len(rec.calls)
	var ce *ClientError
	if _, err := c.GetSkillVersion(ctx, "deploy", "main", 0); !errors.As(err, &ce) || ce.Status != http.StatusBadRequest || len(rec.calls) != calls {
		t.Fatalf("GetSkillVersion 0: %v, calls %v", err, rec.calls[calls:])
	}
	if _, err := c.RevertSkill(ctx, "deploy", "main", 2); err != nil || rec.last() != "revert main deploy 2" {
		t.Fatalf("RevertSkill: %v, call %q", err, rec.last())
	}
}

func TestSkillsUpload(t *testing.T) {
	h, rec := skillsHarness(t)
	// Past the JSON cap: an upload is capped at the install limit instead.
	body := io.LimitReader(zeros{}, 2*MaxJSONBody)
	up := decode[gen.SkillUpload](t, h.postRaw("/skills/upload?path=%2Fhome%2Fme%2Fdeploy", h.adminToken, body), http.StatusCreated)
	if up.Id != "01UPLOAD" || rec.last() != fmt.Sprintf("upload /home/me/deploy %d bytes", 2*MaxJSONBody) || rec.who != h.admin.ID {
		t.Fatalf("upload = %+v, call %q, who %q", up, rec.last(), rec.who)
	}
	errorBody(t, h.postRaw("/skills/upload", h.adminToken, io.LimitReader(zeros{}, skill.MaxInstallTotal+1)),
		http.StatusRequestEntityTooLarge, CodeTooLarge)
	errorBody(t, h.postRaw("/skills/upload", h.ownerToken, bytes.NewReader([]byte("x"))), http.StatusForbidden, CodeForbidden)
	rec.err = fmt.Errorf("%w: upload: not gzip", skill.ErrBadSource)
	errorBody(t, h.postRaw("/skills/upload", h.adminToken, bytes.NewReader([]byte("x"))), http.StatusBadRequest, CodeBadRequest)
}
