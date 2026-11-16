package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
)

// skillsCall is one request the fake daemon saw, past the status probe.
type skillsCall struct {
	method, path, query, body string
}

// fakeSkillsDaemon answers the status probe every command starts with, then
// every other request with status and reply marshalled as JSON (no body when
// nil), recording it.
func fakeSkillsDaemon(t *testing.T, status int, reply any) (string, *skillsCall) {
	t.Helper()
	var body []byte
	if reply != nil {
		var err error
		if body, err = json.Marshal(reply); err != nil {
			t.Fatal(err)
		}
	}
	got := &skillsCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/status" {
			_ = json.NewEncoder(w).Encode(gen.Status{})
			return
		}
		b, _ := io.ReadAll(r.Body)
		*got = skillsCall{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(b)}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, got
}

func (got *skillsCall) expect(t *testing.T, method, path, query string) {
	t.Helper()
	if got.method != method || got.path != path || got.query != query {
		t.Errorf("request = %s %s?%s, want %s %s?%s", got.method, got.path, got.query, method, path, query)
	}
}

// The anonymous nested types of gen.Skill, named for the fixtures.
type (
	skillHidden = struct {
		Code    *string               `json:"code,omitempty"`
		Message *string               `json:"message,omitempty"`
		Reason  gen.SkillHiddenReason `json:"reason"`
		Subject *string               `json:"subject,omitempty"`
	}
	skillLearned = struct {
		By     string                 `json:"by"`
		ByName *string                `json:"by_name,omitempty"`
		From   string                 `json:"from"`
		On     time.Time              `json:"on"`
		Origin gen.SkillLearnedOrigin `json:"origin"`
	}
	skillRequires = struct {
		Bins  *[]string `json:"bins,omitempty"`
		Tools *[]string `json:"tools,omitempty"`
	}
	skillList = struct {
		Skills []gen.Skill `json:"skills"`
	}
)

func learnedBy(on, by, byName string, origin gen.SkillLearnedOrigin) *skillLearned {
	ts, _ := time.Parse(time.RFC3339, on)
	l := &skillLearned{By: by, From: "01TRACE", On: ts, Origin: origin}
	if byName != "" {
		l.ByName = ptrOf(byName)
	}
	return l
}

func apiError(code, msg string) gen.Error { return gen.Error{Code: code, Message: msg} }

func TestSkillsList(t *testing.T) {
	addr, got := fakeSkillsDaemon(t, http.StatusOK, skillList{Skills: []gen.Skill{
		{Name: "release", Description: "Cut a release of a Go module", Level: gen.SkillLevelBundled, Eligible: true},
		{Name: "deploy", Description: "Deploy a service", Level: gen.SkillLevelAgent,
			Hidden: &skillHidden{Reason: gen.SkillHiddenReasonDeniedTool, Subject: ptrOf("exec"), Message: ptrOf("tool exec is denied for this role")}},
		{Name: "gif-search", Description: "Find a GIF\nfor a mood", Level: gen.SkillLevelAgent, Eligible: true,
			Learned: learnedBy("2026-09-12T10:00:00Z", "01USER", "Angel", gen.Correction)},
		{Name: "old", Description: "An old one", Level: gen.SkillLevelAgent, Trashed: ptrOf(true),
			Hidden:  &skillHidden{Reason: gen.SkillHiddenReasonTrashed, Message: ptrOf("in the trash")},
			Learned: learnedBy("2026-09-01T10:00:00Z", "01GONE", "", gen.Explicit)},
	}})
	code, out, _ := runCLI(t, "", "skills", "list", "home", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got.expect(t, http.MethodGet, "/v1/skills", "agent=home")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "LEVEL") || !strings.Contains(lines[0], "TAUGHT") ||
		strings.Contains(lines[0], "SPACE") {
		t.Fatalf("table = %q", out)
	}
	for i, want := range [][]string{
		{"bundled", "release", "●", " - ", "Cut a release of a Go module"},
		{"agent", "deploy", "○", " - ", "Deploy a service (tool exec is denied for this role)"},
		{"agent", "gif-search", "●", "Angel 2026-09-12", "Find a GIF for a mood"},
		{"agent (trash)", "old", "○", "01GONE 2026-09-01", "An old one (in the trash)"},
	} {
		for _, w := range want {
			if !strings.Contains(lines[i+1], w) {
				t.Errorf("row %d = %q, missing %q", i+1, lines[i+1], w)
			}
		}
		if strings.Contains(lines[i+1], "UTC") { // the column is a date, not a timestamp
			t.Errorf("row %d = %q", i+1, lines[i+1])
		}
	}
}

// Every hidden reason reads in words: the message, after the code of a load
// problem; the reason and subject when there is no message.
func TestSkillsHiddenText(t *testing.T) {
	for _, c := range []struct {
		h    *skillHidden
		want string
	}{
		{nil, ""},
		{&skillHidden{Reason: gen.SkillHiddenReasonMissingTool, Subject: ptrOf("browser"), Message: ptrOf("requires tool browser, not in the agent's tools")},
			"requires tool browser, not in the agent's tools"},
		{&skillHidden{Reason: gen.SkillHiddenReasonDeniedTool, Subject: ptrOf("exec"), Message: ptrOf("tool exec is denied for this role")},
			"tool exec is denied for this role"},
		{&skillHidden{Reason: gen.SkillHiddenReasonNeedsBinary, Subject: ptrOf("gh"), Message: ptrOf("requires binary gh (binaries are not supported yet)")},
			"requires binary gh (binaries are not supported yet)"},
		{&skillHidden{Reason: gen.SkillHiddenReasonDisabled, Message: ptrOf("disabled")}, "disabled"},
		{&skillHidden{Reason: gen.SkillHiddenReasonTrashed, Message: ptrOf("in the trash")}, "in the trash"},
		{&skillHidden{Reason: gen.SkillHiddenReasonInvalid, Code: ptrOf("SKL101"), Message: ptrOf("no frontmatter")}, "SKL101: no frontmatter"},
		{&skillHidden{Reason: gen.SkillHiddenReasonMissingTool, Subject: ptrOf("browser")}, "missing_tool browser"},
		{&skillHidden{Reason: gen.SkillHiddenReasonInvalid, Code: ptrOf("SKL101")}, "SKL101: invalid"},
	} {
		if got := hiddenText(gen.Skill{Hidden: c.h}); got != c.want {
			t.Errorf("hiddenText(%+v) = %q, want %q", c.h, got, c.want)
		}
	}
}

func TestSkillsListDefaultAgentAndEmpty(t *testing.T) {
	addr, got := fakeSkillsDaemon(t, http.StatusOK, skillList{Skills: []gen.Skill{}})
	code, out, _ := runCLI(t, "", "skills", "list", "--addr", addr)
	if code != 0 || strings.TrimSpace(out) != "no skills" {
		t.Fatalf("exit = %d, out = %q", code, out)
	}
	got.expect(t, http.MethodGet, "/v1/skills", "agent=main")
}

func TestSkillsShowSaved(t *testing.T) {
	on := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	addr, got := fakeSkillsDaemon(t, http.StatusOK, gen.Skill{
		Name: "my skill", Description: "Find a GIF", Level: gen.SkillLevelAgent, Version: ptrOf(4), Eligible: true,
		Learned:    learnedBy("2026-09-20T14:03:05Z", "01USER", "Angel", gen.Correction),
		Requires:   &skillRequires{Tools: &[]string{"web.fetch"}, Bins: &[]string{"curl", "jq"}},
		References: &[]string{"moods.md"},
		Body:       ptrOf("# Steps\n\n1. Ask.\n"),
		Versions: &[]gen.SkillVersion{
			{Version: 3, On: &on, By: ptrOf("01USER"), ByName: ptrOf("Angel"), Trace: ptrOf("01TRACEB")},
			{Version: 2, By: ptrOf("01USER2")},
			{Version: 1},
		},
	})
	code, out, _ := runCLI(t, "", "skills", "show", "my skill", "--agent", "home", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got.expect(t, http.MethodGet, "/v1/skills/my%20skill", "agent=home")
	want := `name: my skill
description: Find a GIF
level: agent
version: 4
provenance: [skill: my skill · learned from Angel on 2026-09-20]
eligible: yes
learned on: 2026-09-20T14:03:05Z
learned by: Angel
learned from: 01TRACE
origin: correction
requires tools: web.fetch
requires bins: curl, jq
references: moods.md

# Steps

1. Ask.

versions:
  3  2026-09-20  by Angel  from 01TRACEB
  2  by 01USER2
  1
`
	if out != want {
		t.Errorf("show =\n%s\nwant\n%s", out, want)
	}
}

// A teacher with no display name: the identity on the by line, the date
// alone in the provenance line, as skill.load gives it.
func TestSkillsShowUnnamedAuthor(t *testing.T) {
	addr, _ := fakeSkillsDaemon(t, http.StatusOK, gen.Skill{Name: "x", Description: "X", Level: gen.SkillLevelAgent, Eligible: true,
		Learned: learnedBy("2026-09-20T14:03:05Z", "01USER", "", gen.Explicit)})
	code, out, _ := runCLI(t, "", "skills", "show", "x", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, w := range []string{"provenance: [skill: x · learned on 2026-09-20]\n", "learned by: 01USER\n"} {
		if !strings.Contains(out, w) {
			t.Errorf("show = %q, missing %q", out, w)
		}
	}
}

func TestSkillsShowAgentSkill(t *testing.T) {
	addr, got := fakeSkillsDaemon(t, http.StatusOK, gen.Skill{Name: "deploy", Description: "Deploy", Level: gen.SkillLevelAgent,
		Hidden: &skillHidden{Reason: gen.SkillHiddenReasonNeedsBinary, Subject: ptrOf("gh"), Message: ptrOf("requires binary gh (binaries are not supported yet)")},
		Body:   ptrOf("Run it.")})
	code, out, _ := runCLI(t, "", "skills", "show", "deploy", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got.expect(t, http.MethodGet, "/v1/skills/deploy", "agent=main")
	for _, w := range []string{"provenance: [skill: deploy · agent skill]\n", "eligible: no\n",
		"hidden: requires binary gh (binaries are not supported yet)\n", "\nRun it.\n"} {
		if !strings.Contains(out, w) {
			t.Errorf("show = %q, missing %q", out, w)
		}
	}
	if strings.Contains(out, "versions:") || strings.Contains(out, "learned") {
		t.Errorf("a hand-written skill printed saved facts: %q", out)
	}
}

func TestSkillsShowBundled(t *testing.T) {
	addr, _ := fakeSkillsDaemon(t, http.StatusOK, gen.Skill{Name: "release", Description: "R", Level: gen.SkillLevelBundled, Eligible: true})
	code, out, _ := runCLI(t, "", "skills", "show", "release", "--addr", addr)
	if code != 0 || !strings.Contains(out, "provenance: [skill: release · bundled skill]\n") || strings.Contains(out, "version:") {
		t.Errorf("exit = %d, out = %q", code, out)
	}
}

func TestSkillsShowNotFound(t *testing.T) {
	addr, _ := fakeSkillsDaemon(t, http.StatusNotFound, apiError("not_found", "no skill nope"))
	code, out, errOut := runCLI(t, "", "skills", "show", "nope", "--addr", addr)
	if code != 1 || out != "" || !strings.Contains(errOut, "no skill nope") {
		t.Errorf("exit = %d, out = %q, stderr = %q", code, out, errOut)
	}
}

func TestSkillsRemove(t *testing.T) {
	addr, got := fakeSkillsDaemon(t, http.StatusNoContent, nil)
	code, out, _ := runCLI(t, "", "skills", "remove", "main", "gif-search", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got.expect(t, http.MethodDelete, "/v1/skills/gif-search", "agent=main")
	want := "removed gif-search from main; a live skill went to the trash: holaclaw skills restore main gif-search\n"
	if out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

// A bundled skill is refused by the daemon; the CLI passes the refusal through.
func TestSkillsRemoveBundledPassesRefusal(t *testing.T) {
	addr, got := fakeSkillsDaemon(t, http.StatusForbidden, apiError("forbidden", "a bundled skill is never written"))
	code, out, errOut := runCLI(t, "", "skills", "remove", "main", "release", "--addr", addr)
	if code != 1 || out != "" || !strings.Contains(errOut, "a bundled skill is never written") {
		t.Errorf("exit = %d, out = %q, stderr = %q", code, out, errOut)
	}
	got.expect(t, http.MethodDelete, "/v1/skills/release", "agent=main")
}

func TestSkillsRestore(t *testing.T) {
	addr, got := fakeSkillsDaemon(t, http.StatusOK, gen.Skill{Name: "old", Description: "x", Level: gen.SkillLevelAgent, Eligible: true})
	code, out, _ := runCLI(t, "", "skills", "restore", "home", "old", "--addr", addr)
	if code != 0 || out != "restored old of home\n" {
		t.Fatalf("exit = %d, out = %q", code, out)
	}
	got.expect(t, http.MethodPost, "/v1/skills/old/restore", "agent=home")
}

func TestSkillsRestoreConflict(t *testing.T) {
	addr, _ := fakeSkillsDaemon(t, http.StatusConflict, apiError("conflict", "a live old exists"))
	code, _, errOut := runCLI(t, "", "skills", "restore", "main", "old", "--addr", addr)
	if code != 1 || !strings.Contains(errOut, "a live old exists") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
}

func TestSkillsRevert(t *testing.T) {
	addr, got := fakeSkillsDaemon(t, http.StatusOK, gen.Skill{
		Name: "gif-search", Description: "x", Level: gen.SkillLevelAgent, Eligible: true,
		Versions: &[]gen.SkillVersion{{Version: 3}, {Version: 2}},
	})
	code, out, _ := runCLI(t, "", "skills", "revert", "main", "gif-search", "--to", "1", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got.expect(t, http.MethodPost, "/v1/skills/gif-search/revert", "agent=main")
	var body gen.SkillRevert
	if err := json.Unmarshal([]byte(got.body), &body); err != nil || body.To == nil || *body.To != 1 {
		t.Errorf("body = %q, %v", got.body, err)
	}
	if want := "reverted gif-search of main\n\nversions:\n  3\n  2\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestSkillsRevertNotFound(t *testing.T) {
	addr, _ := fakeSkillsDaemon(t, http.StatusNotFound, apiError("not_found", "no version 9"))
	code, _, errOut := runCLI(t, "", "skills", "revert", "main", "x", "--to", "9", "--addr", addr)
	if code != 1 || !strings.Contains(errOut, "no version 9") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
}

// Every wrong invocation is caught before any request: no --addr is given,
// so a dial would fail differently. --space is gone: skills follow the agent.
func TestSkillsUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"list", "a", "b"},
		{"show"},
		{"show", "a", "b"},
		{"show", "a", "--space", "dm/u1"},
		{"remove", "main"},
		{"remove", "main", "a", "b"},
		{"remove", "main", "a", "--space", "dm/u1"},
		{"restore", "main"},
		{"restore", "main", "old", "--space", "dm/u1"},
		{"revert", "main"},
		{"revert", "main", "x"},
		{"revert", "main", "x", "--to", "0"},
		{"revert", "main", "--to", "1"},
		{"revert", "main", "x", "--space", "dm/u1"},
		{"list", "--bogus"},
		{"install"},
		{"install", "a", "b"},
		{"install", "a", "--as", "x", "--all"},
	} {
		code, _, errOut := runCLI(t, "", append([]string{"skills"}, args...)...)
		if code != 2 || !strings.Contains(errOut, "usage: holaclaw skills "+args[0]) {
			t.Errorf("skills %v: exit = %d, stderr = %q", args, code, errOut)
		}
	}
	if _, _, errOut := runCLI(t, "", "skills", "remove", "main", "a", "--space", "dm/u1"); !strings.Contains(errOut, "unknown flag: --space") {
		t.Errorf("--space: stderr = %q", errOut)
	}
}

func TestSkillsHelpAndUnknown(t *testing.T) {
	for _, args := range [][]string{{"skills", "-h"}, {"skills", "--help"}, {"skills", "help"}, {"help", "skills"}} {
		code, out, _ := runCLI(t, "", args...)
		if code != 0 || strings.Contains(out, "space") {
			t.Errorf("%v: exit = %d, stdout = %q", args, code, out)
		}
		for _, sub := range []string{"list", "show", "install", "remove", "restore", "revert"} {
			if !strings.Contains(out, "\n  "+sub+" ") {
				t.Errorf("%v does not list %s: %q", args, sub, out)
			}
		}
	}
	for sub, line := range map[string]string{
		"revert":  "usage: holaclaw skills revert <agent> <name> [flags]",
		"install": "usage: holaclaw skills install <path|git-url> [flags]",
	} {
		if code, out, _ := runCLI(t, "", "skills", sub, "-h"); code != 0 || !strings.Contains(out, line) {
			t.Errorf("skills %s -h: exit = %d, stdout = %q", sub, code, out)
		}
	}
	code, _, errOut := runCLI(t, "", "skills", "promote")
	if code != 2 || !strings.Contains(errOut, `unknown command "promote" for "holaclaw skills"`) {
		t.Errorf("unknown: exit = %d, stderr = %q", code, errOut)
	}
}

// installLog is what the fake daemon saw: every install body and every
// upload, in order.
type installLog struct {
	bodies  []map[string]any
	uploads []uploaded
}

// uploaded is one upload: the path the CLI sent and the archive's entries.
type uploaded struct {
	path    string
	entries []string
}

// uploadID is the id the fake daemon gives every upload.
const uploadID = "01J9Q4UPLOADUPLOADUPLOADUP"

// fakeInstallDaemon answers an upload with uploadID, the preview POST with
// preview and the install POST with status and reply, both marshalled as
// JSON, recording each.
func fakeInstallDaemon(t *testing.T, preview any, status int, reply any) (string, *installLog) {
	t.Helper()
	log := &installLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/status":
			_ = json.NewEncoder(w).Encode(gen.Status{})
			return
		case r.Method == http.MethodPost && r.URL.Path == "/v1/skills/upload":
			if ct := r.Header.Get("Content-Type"); ct != "application/gzip" {
				t.Errorf("upload content type %q", ct)
			}
			entries, err := tarEntries(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(apiError("bad_request", err.Error()))
				return
			}
			log.uploads = append(log.uploads, uploaded{path: r.URL.Query().Get("path"), entries: entries})
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(gen.SkillUpload{Id: uploadID, ExpiresAt: time.Now().Add(time.Hour)})
			return
		case r.Method != http.MethodPost || r.URL.Path != "/v1/skills":
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		log.bodies = append(log.bodies, body)
		if body["preview"] == true {
			_ = json.NewEncoder(w).Encode(preview)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, log
}

// tarEntries lists the names in the gzipped tar r.
func tarEntries(r io.Reader) ([]string, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	var names []string
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		names = append(names, h.Name)
	}
}

// localSkill is a skill folder <tmp>/<name> with a SKILL.md.
func localSkill(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: "+name+"\ndescription: When deploying.\n---\necho hello\n")
	return dir
}

// localCollection is a folder <tmp>/acme holding skills/release and
// skills/gif.
func localCollection(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "acme")
	for _, name := range []string{"release", "gif"} {
		writeFile(t, filepath.Join(dir, "skills", name, "SKILL.md"), "---\nname: "+name+"\ndescription: When needed.\n---\necho hello\n")
	}
	return dir
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const installHash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// installOut is the reply to an install of the deploy fixture: a preview
// unless installed, with findings.
func installOut(installed bool, findings ...gen.ScanFinding) gen.SkillInstalled {
	return gen.SkillInstalled{
		Skill: &gen.Skill{Name: "deploy", Description: "Deploy a service", Level: gen.SkillLevelAgent, Eligible: true,
			Requires: &skillRequires{Bins: &[]string{"gh"}}, Scripts: &[]string{"run.sh"},
			Body: ptrOf("Run it.\n"), Versions: &[]gen.SkillVersion{{Version: 1}}},
		Findings:  append([]gen.ScanFinding{}, findings...),
		Hash:      installHash,
		Installed: installed,
	}
}

var (
	installClean   = installOut(false)
	installFlagged = installOut(false,
		gen.ScanFinding{File: "run.sh", Line: 3, Code: "SCN001", Message: "a pipe into a shell"},
		gen.ScanFinding{File: "SKILL.md", Line: 9, Code: "SCN003", Message: "bidirectional Unicode"})
	installDone = installOut(true)
)

func TestSkillsInstallAsksAndInstalls(t *testing.T) {
	addr, posts := fakeInstallDaemon(t, installClean, http.StatusCreated, installDone)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "deploy-skill", "SKILL.md"), "---\nname: deploy\ndescription: When deploying.\n---\necho hello\n")
	writeFile(t, filepath.Join(dir, "deploy-skill", ".git", "HEAD"), "x")
	t.Chdir(dir)
	code, out, errOut := runCLI(t, "yes\n", "skills", "install", "deploy-skill", "--agent", "home", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	want := `name: deploy
description: Deploy a service
level: agent
provenance: [skill: deploy · agent skill]
eligible: yes
requires bins: gh
scripts: run.sh

Run it.

scan: clean
install deploy into agent home? [y/N] installed deploy into home (sha256:0123456789ab)
`
	if out != want {
		t.Errorf("out =\n%s\nwant\n%s", out, want)
	}
	src := filepath.Join(dir, "deploy-skill")
	if !strings.Contains(errOut, "uploading "+src+"…") {
		t.Errorf("stderr = %q", errOut)
	}
	// One upload, the folder as its one top-level entry, .git left out; the
	// preview and the install both name it.
	want2 := []uploaded{{path: src, entries: []string{"deploy-skill/", "deploy-skill/SKILL.md"}}}
	if !reflect.DeepEqual(posts.uploads, want2) {
		t.Errorf("uploads = %+v, want %+v", posts.uploads, want2)
	}
	if len(posts.bodies) != 2 {
		t.Fatalf("posts = %v", posts.bodies)
	}
	p, i := posts.bodies[0], posts.bodies[1]
	if p["preview"] != true || p["upload"] != uploadID || p["agent"] != "home" || p["hash"] != nil || p["name"] != nil || p["force"] != nil {
		t.Errorf("preview body = %v", p)
	}
	if i["preview"] != nil || i["hash"] != installHash || i["upload"] != uploadID || i["name"] != nil || i["force"] != nil {
		t.Errorf("install body = %v", i)
	}
}

func TestSkillsInstallFindingsRefuseWithoutForce(t *testing.T) {
	addr, posts := fakeInstallDaemon(t, installFlagged, http.StatusCreated, installDone)
	code, out, errOut := runCLI(t, "y\n", "skills", "install", "ssh://h/acme/skills", "--git-ref", "v1.2.0", "--git-path", "release", "--addr", addr)
	if code != 1 {
		t.Errorf("exit = %d", code)
	}
	for _, w := range []string{
		"\nRun it.\n\nscan: 2 findings\n",
		"  run.sh:3 SCN001 a pipe into a shell\n",
		"  SKILL.md:9 SCN003 bidirectional Unicode\n",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("out = %q, missing %q", out, w)
		}
	}
	if strings.Contains(out, "[y/N]") || strings.Contains(out, "versions") {
		t.Errorf("out = %q", out)
	}
	if !strings.Contains(errOut, "refusing to install: run again with --force after reading the findings") {
		t.Errorf("stderr = %q", errOut)
	}
	want := map[string]any{"url": "ssh://h/acme/skills", "ref": "v1.2.0", "path": "release"}
	if len(posts.bodies) != 1 || !reflect.DeepEqual(posts.bodies[0]["git"], want) || posts.bodies[0]["upload"] != nil || len(posts.uploads) != 0 {
		t.Errorf("posts = %v", posts.bodies)
	}
}

func TestSkillsInstallForceYes(t *testing.T) {
	addr, posts := fakeInstallDaemon(t, installFlagged, http.StatusCreated, installDone)
	src := "git@h:acme/skills.git"
	code, out, _ := runCLI(t, "", "skills", "install", "--force", src, "--git-ref", "main", "--yes", "--as", "ship", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, "[y/N]") || !strings.Contains(out, "scan: 2 findings") ||
		!strings.HasSuffix(out, "installed deploy into main (sha256:0123456789ab)\n") {
		t.Errorf("out = %q", out)
	}
	if len(posts.bodies) != 2 {
		t.Fatalf("posts = %v", posts.bodies)
	}
	for _, b := range posts.bodies {
		// The CLI sends the url as typed; the daemon reads the scp form.
		if !reflect.DeepEqual(b["git"], map[string]any{"url": src, "ref": "main"}) || b["name"] != "ship" || b["agent"] != "main" {
			t.Errorf("body = %v", b)
		}
	}
	if posts.bodies[0]["force"] != nil || posts.bodies[1]["force"] != true || posts.bodies[1]["hash"] != installHash {
		t.Errorf("posts = %v", posts.bodies)
	}
}

func TestSkillsInstallDeclined(t *testing.T) {
	for name, stdin := range map[string]string{"no": "n\n", "eof": ""} {
		t.Run(name, func(t *testing.T) {
			addr, posts := fakeInstallDaemon(t, installClean, http.StatusCreated, installDone)
			code, out, _ := runCLI(t, stdin, "skills", "install", localSkill(t, "deploy"), "--addr", addr)
			if code != 0 {
				t.Fatalf("exit = %d", code)
			}
			if !strings.Contains(out, "install deploy into agent main? [y/N] ") || !strings.HasSuffix(out, "not installed\n") {
				t.Errorf("out = %q", out)
			}
			if len(posts.bodies) != 1 || posts.bodies[0]["upload"] != uploadID {
				t.Errorf("posts = %v", posts.bodies)
			}
		})
	}
}

func TestSkillsInstallConflictPassesThrough(t *testing.T) {
	addr, posts := fakeInstallDaemon(t, installClean, http.StatusConflict,
		apiError("conflict", "the source changed since the preview"))
	code, out, errOut := runCLI(t, "", "skills", "install", localSkill(t, "deploy"), "--yes", "--addr", addr)
	if code != 1 || !strings.Contains(errOut, "the source changed since the preview") || strings.Contains(out, "installed deploy") {
		t.Errorf("exit = %d, out = %q, stderr = %q", code, out, errOut)
	}
	if len(posts.bodies) != 2 {
		t.Errorf("posts = %v", posts.bodies)
	}
}

func TestSkillsInstallPreviewError(t *testing.T) {
	addr, _ := fakeSkillsDaemon(t, http.StatusBadRequest, apiError("bad_request", "SKL301: git fetch: no such ref"))
	code, out, errOut := runCLI(t, "", "skills", "install", "ssh://h/acme/skills", "--git-ref", "nope", "--addr", addr)
	if code != 1 || out != "" || !strings.Contains(errOut, "git fetch: no such ref") {
		t.Errorf("exit = %d, out = %q, stderr = %q", code, out, errOut)
	}
}

// A git url without --git-ref sends no ref: the daemon fetches the
// repository's default branch.
func TestSkillsInstallGitDefaultBranch(t *testing.T) {
	addr, posts := fakeInstallDaemon(t, installClean, http.StatusCreated, installDone)
	src := "https://github.com/curriedsoftware/nixomatic-skill"
	code, out, errOut := runCLI(t, "", "skills", "install", src, "--git-path", "release", "--yes", "--addr", addr)
	if code != 0 || !strings.HasSuffix(out, "installed deploy into main (sha256:0123456789ab)\n") {
		t.Fatalf("exit = %d, out = %q, stderr = %q", code, out, errOut)
	}
	want := map[string]any{"url": src, "path": "release"}
	if len(posts.bodies) != 2 || !reflect.DeepEqual(posts.bodies[0]["git"], want) || !reflect.DeepEqual(posts.bodies[1]["git"], want) {
		t.Errorf("posts = %v", posts.bodies)
	}
}

// The git flags go with a git url; nothing is sent otherwise.
func TestSkillsInstallGitFlags(t *testing.T) {
	addr, posts := fakeInstallDaemon(t, installClean, http.StatusCreated, installDone)
	for _, c := range []struct {
		args []string
		msg  string
	}{
		{[]string{"/srv/deploy", "--git-ref", "v1"}, "--git-ref and --git-path go with a git url"},
		{[]string{"/srv/deploy", "--git-path", "x"}, "--git-ref and --git-path go with a git url"},
	} {
		code, _, errOut := runCLI(t, "", append([]string{"skills", "install", "--addr", addr}, c.args...)...)
		if code != 1 || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: exit = %d, stderr = %q", c.args, code, errOut)
		}
	}
	if len(posts.bodies) != 0 {
		t.Errorf("posts = %v", posts.bodies)
	}
}

const (
	collHash = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	relHash  = "sha256:1111111111112222222222223333333333334444444444445555555555556666"
	gifHash  = "sha256:aaaaaaaaaaaabbbbbbbbbbbbccccccccccccddddddddddddeeeeeeeeeeeeffff"
)

// collection is a collection answer, preview or install, with or without a
// finding in the gif-search entry.
func collection(installed, flagged bool) gen.SkillInstalled {
	gif := []gen.ScanFinding{}
	if flagged {
		gif = append(gif, gen.ScanFinding{File: "run.sh", Line: 3, Code: "SCN001", Message: "a pipe into a shell"})
	}
	return gen.SkillInstalled{Findings: []gen.ScanFinding{}, Hash: collHash, Installed: installed, Skills: &[]gen.SkillInstalled{
		{Path: ptrOf("skills/release"), Skill: &gen.Skill{Name: "release", Description: "Cut a\nrelease", Level: gen.SkillLevelAgent, Eligible: true},
			Findings: []gen.ScanFinding{}, Hash: relHash, Installed: installed},
		{Path: ptrOf("skills/gif"), Skill: &gen.Skill{Name: "gif-search", Description: "Find a GIF", Level: gen.SkillLevelAgent, Eligible: true},
			Findings: gif, Hash: gifHash, Installed: installed},
	}}
}

func TestSkillsInstallCollectionLists(t *testing.T) {
	local := localCollection(t)
	for name, tc := range map[string]struct {
		args             []string
		source, one, all string
	}{
		"git": {[]string{"ssh://h/acme/skills", "--git-ref", "v1"}, "ssh://h/acme/skills",
			"ssh://h/acme/skills --git-ref v1 --git-path skills/release", "ssh://h/acme/skills --git-ref v1"},
		"git path": {[]string{"ssh://h/acme/skills", "--git-ref", "v1", "--git-path", "pack"}, "ssh://h/acme/skills",
			"ssh://h/acme/skills --git-ref v1 --git-path pack/skills/release", "ssh://h/acme/skills --git-ref v1 --git-path pack"},
		"git default branch": {[]string{"ssh://h/acme/skills"}, "ssh://h/acme/skills",
			"ssh://h/acme/skills --git-path skills/release", "ssh://h/acme/skills"},
		"local": {[]string{local + "/"}, local, filepath.Join(local, "skills", "release"), local},
	} {
		t.Run(name, func(t *testing.T) {
			addr, posts := fakeInstallDaemon(t, collection(false, true), http.StatusCreated, collection(true, true))
			code, out, errOut := runCLI(t, "y\n", append([]string{"skills", "install", "--addr", addr}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, errOut)
			}
			lines := strings.Split(out, "\n")
			if len(lines) != 8 || lines[0] != "2 skills in "+tc.source+":" || !strings.HasPrefix(lines[1], "NAME") ||
				!strings.Contains(lines[1], "SCAN") || !strings.Contains(lines[1], "DESCRIPTION") {
				t.Fatalf("out = %q", out)
			}
			for i, want := range [][]string{{"release", "clean", "Cut a release"}, {"gif-search", "1 findings", "Find a GIF"}} {
				for _, w := range want {
					if !strings.Contains(lines[i+2], w) {
						t.Errorf("row %d = %q, missing %q", i, lines[i+2], w)
					}
				}
			}
			want := "\ninstall one:  holaclaw skills install " + tc.one + "\ninstall all:  holaclaw skills install " + tc.all + " --all\n"
			if !strings.HasSuffix(out, want) {
				t.Errorf("out = %q, want suffix %q", out, want)
			}
			if strings.Contains(out, "[y/N]") || strings.Contains(out, "SCN001") {
				t.Errorf("a listing asked or showed findings: %q", out)
			}
			if len(posts.bodies) != 1 || posts.bodies[0]["all"] != nil {
				t.Errorf("posts = %v", posts.bodies)
			}
		})
	}
}

func TestSkillsInstallAllRefusesFindings(t *testing.T) {
	addr, posts := fakeInstallDaemon(t, collection(false, true), http.StatusCreated, collection(true, true))
	code, out, errOut := runCLI(t, "y\n", "skills", "install", localCollection(t), "--all", "--addr", addr)
	if code != 1 {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(out, "\nscan: 1 findings\n  skills/gif/run.sh:3 SCN001 a pipe into a shell\n") ||
		strings.Contains(out, "[y/N]") || strings.Contains(out, "install one:") {
		t.Errorf("out = %q", out)
	}
	if !strings.Contains(errOut, "refusing to install: run again with --force after reading the findings") {
		t.Errorf("stderr = %q", errOut)
	}
	if len(posts.bodies) != 1 {
		t.Errorf("posts = %v", posts.bodies)
	}
}

func TestSkillsInstallAllForceYes(t *testing.T) {
	addr, posts := fakeInstallDaemon(t, collection(false, true), http.StatusCreated, collection(true, true))
	src := "ssh://h/acme/skills"
	code, out, _ := runCLI(t, "", "skills", "install", src, "--git-ref", "v1", "--all", "--force", "--yes", "--agent", "home", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	want := "installed release into home (sha256:111111111111)\ninstalled gif-search into home (sha256:aaaaaaaaaaaa)\n"
	if !strings.HasSuffix(out, want) || strings.Contains(out, "[y/N]") {
		t.Errorf("out = %q", out)
	}
	if len(posts.bodies) != 2 {
		t.Fatalf("posts = %v", posts.bodies)
	}
	p, i := posts.bodies[0], posts.bodies[1]
	if p["all"] != nil || p["hash"] != nil || p["force"] != nil {
		t.Errorf("preview body = %v", p)
	}
	if i["all"] != true || i["hash"] != collHash || i["force"] != true || i["preview"] != nil ||
		!reflect.DeepEqual(i["git"], map[string]any{"url": src, "ref": "v1"}) || i["agent"] != "home" || i["name"] != nil {
		t.Errorf("install body = %v", i)
	}
}

func TestSkillsInstallAllCleanAsks(t *testing.T) {
	for name, tc := range map[string]struct {
		stdin string
		posts int
		tail  string
	}{
		"yes": {"y\n", 2, "? [y/N] installed release into main (sha256:111111111111)\ninstalled gif-search into main (sha256:aaaaaaaaaaaa)\n"},
		"no":  {"n\n", 1, "? [y/N] not installed\n"},
	} {
		t.Run(name, func(t *testing.T) {
			addr, posts := fakeInstallDaemon(t, collection(false, false), http.StatusCreated, collection(true, false))
			code, out, _ := runCLI(t, tc.stdin, "skills", "install", localCollection(t), "--all", "--addr", addr)
			if code != 0 {
				t.Fatalf("exit = %d", code)
			}
			if !strings.Contains(out, "\nscan: clean\ninstall 2 skills into agent main? [y/N] ") || !strings.HasSuffix(out, tc.tail) {
				t.Errorf("out = %q", out)
			}
			if len(posts.bodies) != tc.posts {
				t.Errorf("posts = %v", posts.bodies)
			}
			if tc.posts == 2 && (posts.bodies[1]["all"] != true || posts.bodies[1]["force"] != nil) {
				t.Errorf("install body = %v", posts.bodies[1])
			}
		})
	}
}

// --all with a single skill is ignored: today's flow, no `all` sent.
func TestSkillsInstallAllSingleSkill(t *testing.T) {
	addr, posts := fakeInstallDaemon(t, installClean, http.StatusCreated, installDone)
	code, out, _ := runCLI(t, "", "skills", "install", localSkill(t, "deploy"), "--all", "--yes", "--addr", addr)
	if code != 0 || !strings.HasSuffix(out, "installed deploy into main (sha256:0123456789ab)\n") {
		t.Fatalf("exit = %d, out = %q", code, out)
	}
	if len(posts.bodies) != 2 || posts.bodies[1]["all"] != nil {
		t.Errorf("posts = %v", posts.bodies)
	}
}

// A path to a SKILL.md installs its folder.
func TestSkillsInstallSkillFile(t *testing.T) {
	addr, log := fakeInstallDaemon(t, installClean, http.StatusCreated, installDone)
	dir := localSkill(t, "deploy")
	code, _, errOut := runCLI(t, "", "skills", "install", filepath.Join(dir, "SKILL.md"), "--yes", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	want := []uploaded{{path: dir, entries: []string{"deploy/", "deploy/SKILL.md"}}}
	if !reflect.DeepEqual(log.uploads, want) || len(log.bodies) != 2 {
		t.Errorf("uploads = %+v, bodies = %v", log.uploads, log.bodies)
	}
}

// A local path is checked before anything is sent: it is a skill or a
// collection, and a url that does not parse says why.
func TestSkillsInstallLocalRefused(t *testing.T) {
	addr, log := fakeInstallDaemon(t, installClean, http.StatusCreated, installDone)
	empty := t.TempDir()
	writeFile(t, filepath.Join(empty, "docs", "README.md"), "echo hello\n")
	notes := filepath.Join(empty, "notes.md")
	writeFile(t, notes, "echo hello\n")
	linked := localSkill(t, "linked")
	if err := os.Symlink("/etc/hostname", filepath.Join(linked, "link")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, msg string }{
		{filepath.Join(empty, "missing"), "no such file or directory"},
		{empty, "is not a skill (no SKILL.md) and holds no skill folders"},
		{notes, "is not a folder or a SKILL.md"},
		{"https://h", "no path"},
		{"http://h/acme/skills", "the scheme is not https, ssh, git or file"},
		{linked, linked + ": link: fsutil: is a symlink"},
	} {
		code, _, errOut := runCLI(t, "", "skills", "install", c.path, "--addr", addr)
		if code != 1 || !strings.Contains(errOut, c.msg) {
			t.Errorf("%s: exit = %d, stderr = %q", c.path, code, errOut)
		}
	}
	if len(log.bodies) != 0 {
		t.Errorf("bodies = %v", log.bodies)
	}
}
