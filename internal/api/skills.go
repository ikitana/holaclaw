package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/skill"
)

// --- skills -------------------------------------------------------------------
//
// The explorer surface of docs/skills.md §7. Reads are client scope: a token
// bound to a user sees only the agents that user may use, and any other agent
// is a not_found, so its existence is not revealed. Writes are admin scope (the
// scope table) and aimed at agent skills only: a bundled one is never written.

// ListSkills lists every skill of one agent, the trash included.
func (h *handlers) ListSkills(w http.ResponseWriter, r *http.Request, params gen.ListSkillsParams) {
	if !h.skillsReady(w, r, params.Agent) {
		return
	}
	sks, err := h.b.Skills.ListSkills(r.Context(), params.Agent)
	if err != nil {
		h.failSkill(w, "list skills", err)
		return
	}
	if sks == nil {
		sks = []gen.Skill{}
	}
	writeJSON(w, http.StatusOK, struct {
		Skills []gen.Skill `json:"skills"`
	}{sks})
}

// GetSkill shows one skill with its body and, for an agent one, its versions,
// or the version the query names.
func (h *handlers) GetSkill(w http.ResponseWriter, r *http.Request, id gen.Id, params gen.GetSkillParams) {
	if !h.skillsReady(w, r, params.Agent) {
		return
	}
	var sk gen.Skill
	var err error
	switch v := params.Version; {
	case v == nil:
		sk, err = h.b.Skills.GetSkill(r.Context(), params.Agent, id)
	case *v < 1:
		badRequest(w, "version must be a version number, 1 or more")
		return
	default:
		sk, err = h.b.Skills.GetSkillVersion(r.Context(), params.Agent, id, *v)
	}
	if err != nil {
		h.failSkill(w, "get skill", err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

// RemoveSkill trashes a live agent skill, or deletes a trashed one for good.
func (h *handlers) RemoveSkill(w http.ResponseWriter, r *http.Request, id gen.Id, params gen.RemoveSkillParams) {
	if !h.skillsReady(w, r, params.Agent) {
		return
	}
	if err := h.b.Skills.RemoveSkill(r.Context(), params.Agent, id, skillWho(r)); err != nil {
		h.failSkill(w, "remove skill", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RestoreSkill moves an agent skill out of the trash.
func (h *handlers) RestoreSkill(w http.ResponseWriter, r *http.Request, id gen.Id, params gen.RestoreSkillParams) {
	if !h.skillsReady(w, r, params.Agent) {
		return
	}
	sk, err := h.b.Skills.RestoreSkill(r.Context(), params.Agent, id, skillWho(r))
	if err != nil {
		h.failSkill(w, "restore skill", err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

// RevertSkill copies an earlier version of a live agent skill into a new
// current one; the body is optional, an omitted to or the live number is a
// no-op, and a version below 1 is a 400.
func (h *handlers) RevertSkill(w http.ResponseWriter, r *http.Request, id gen.Id, params gen.RevertSkillParams) {
	if !h.skillsReady(w, r, params.Agent) {
		return
	}
	var in gen.SkillRevert
	if !decodeOptionalJSON(w, r, &in) {
		return
	}
	if in.To != nil && *in.To < 1 {
		badRequest(w, "to must be a version number, 1 or more")
		return
	}
	sk, err := h.b.Skills.RevertSkill(r.Context(), params.Agent, id, in.To, skillWho(r))
	if err != nil {
		h.failSkill(w, "revert skill", err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

// InstallSkill brings a skill folder from a source the person chose into
// agents/<agent>/skills/, or previews it (docs/skills.md §7). The body
// carries the agent, so the readiness checks run after decoding it.
func (h *handlers) InstallSkill(w http.ResponseWriter, r *http.Request) {
	var in gen.SkillInstall
	if !decodeJSON(w, r, &in) {
		return
	}
	if !h.skillsReady(w, r, in.Agent) {
		return
	}
	if (in.Git == nil) == (in.Upload == nil) {
		badRequest(w, "exactly one of git and upload is required")
		return
	}
	out, err := h.b.Skills.InstallSkill(r.Context(), in, skillWho(r))
	if err != nil {
		h.failSkill(w, "install skill", err)
		return
	}
	status := http.StatusCreated
	if !out.Installed {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// UploadSkill keeps a skill folder the client read and sent as a gzipped
// tar, for an install to name (docs/skills.md §7). The body is capped at
// the install limit; past it the answer is 413.
func (h *handlers) UploadSkill(w http.ResponseWriter, r *http.Request, params gen.UploadSkillParams) {
	if h.b.Skills == nil {
		notImplemented(w, "skills")
		return
	}
	out, err := h.b.Skills.UploadSkill(r.Context(), r.Body, deref(params.Path), skillWho(r))
	if tooLarge(err) {
		writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "payload too large")
		return
	}
	if err != nil {
		h.failSkill(w, "upload skill", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// skillsReady answers 501 without a backend, 400 without an agent, and 404
// for an agent a user-bound token may not use.
func (h *handlers) skillsReady(w http.ResponseWriter, r *http.Request, agent string) bool {
	if h.b.Skills == nil {
		notImplemented(w, "skills")
		return false
	}
	if agent == "" {
		badRequest(w, "agent is required")
		return false
	}
	if p, _ := PrincipalFrom(r.Context()); !mayUseAgent(p, h.b.Config, agent) {
		notFound(w)
		return false
	}
	return true
}

// skillWho is who a skill write is recorded as: the acting identity, which the
// socket and every token have through their binding.
func skillWho(r *http.Request) event.ID {
	p, _ := PrincipalFrom(r.Context())
	return p.UserID
}

// failSkill maps the skill errors onto the stable codes.
func (h *handlers) failSkill(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, skill.ErrNotFound):
		notFound(w)
	case errors.Is(err, skill.ErrExists):
		writeError(w, http.StatusConflict, CodeConflict, "a live skill of that name exists")
	case errors.Is(err, skill.ErrBundled):
		writeError(w, http.StatusForbidden, CodeForbidden, "a bundled skill is never written")
	case errors.Is(err, skill.ErrUnsafe):
		writeError(w, http.StatusConflict, CodeConflict, "the scan found something; read the findings and force to install anyway")
	case errors.Is(err, skill.ErrChanged):
		writeError(w, http.StatusConflict, CodeConflict, "the source changed since the preview")
	case errors.Is(err, skill.ErrBadSource), errors.Is(err, skill.ErrCollection):
		badRequest(w, err.Error())
	case errors.Is(err, skill.ErrBadName):
		badRequest(w, "name is not a valid skill name")
	case errors.Is(err, skill.ErrUnsafePath):
		writeError(w, http.StatusConflict, CodeConflict, "the skill folder is not safe to write; a person must fix it")
	default:
		h.b.fail(w, what, err)
	}
}

// decodeOptionalJSON is decodeJSON for a body that may be empty.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	switch err := dec.Decode(v); {
	case err == nil, errors.Is(err, io.EOF):
		return true
	case tooLarge(err):
		writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "payload too large")
	default:
		badRequest(w, "invalid JSON body")
	}
	return false
}

// deref returns *p, or the zero value when p is nil.
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
