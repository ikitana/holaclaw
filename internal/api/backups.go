package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/backup"
)

// --- backups ------------------------------------------------------------------
//
// Backups the daemon takes of itself and keeps (ARCHITECTURE.md §4). Every
// operation is admin scope, and admin is all it takes: an archive made with
// include_secrets holds auth.toml and is downloaded like any other, the one
// place the API gives a credential back.

// BackupBackend is the managed backups of the daemon; *backup.Manager
// implements it. Errors: backup.ErrNotFound, backup.ErrBusy,
// backup.ErrNotReady, backup.ErrBadArchive and backup.ErrIncompatible map
// onto the stable codes. by is who asked, for the trace.
type BackupBackend interface {
	Start(includeSecrets bool, by string) (backup.Record, error)
	List() []backup.Record
	Get(id string) (backup.Record, error)
	// Open returns the archive of a ready backup; the caller closes it.
	Open(id string) (*os.File, backup.Record, error)
	// Verify returns nil for an archive that matches its manifest, and what
	// does not match otherwise.
	Verify(id string) error
	Delete(id, by string) error
	// Import keeps an uploaded archive, once it matches its manifest.
	Import(r io.Reader, by string) (backup.Record, error)
	// StageRestore checks a backup and stages its data for the next start.
	StageRestore(id, by string) error
}

func (h *handlers) ListBackups(w http.ResponseWriter, _ *http.Request) {
	if h.b.Backups == nil {
		notImplemented(w, "backups")
		return
	}
	recs := h.b.Backups.List()
	out := make([]gen.Backup, 0, len(recs))
	for _, r := range recs {
		out = append(out, toGenBackup(r))
	}
	writeJSON(w, http.StatusOK, struct {
		Backups []gen.Backup `json:"backups"`
	}{out})
}

// CreateBackup starts a backup and answers at once: the archive is written in
// the background and the record says how it went.
func (h *handlers) CreateBackup(w http.ResponseWriter, r *http.Request) {
	if h.b.Backups == nil {
		notImplemented(w, "backups")
		return
	}
	var in gen.BackupCreate
	if !decodeOptionalJSON(w, r, &in) {
		return
	}
	rec, err := h.b.Backups.Start(deref(in.IncludeSecrets), string(skillWho(r)))
	if err != nil {
		h.failBackup(w, "create backup", err)
		return
	}
	writeJSON(w, http.StatusAccepted, toGenBackup(rec))
}

func (h *handlers) GetBackup(w http.ResponseWriter, _ *http.Request, id gen.Id) {
	if h.b.Backups == nil {
		notImplemented(w, "backups")
		return
	}
	rec, err := h.b.Backups.Get(id)
	if err != nil {
		h.failBackup(w, "get backup", err)
		return
	}
	writeJSON(w, http.StatusOK, toGenBackup(rec))
}

// GetBackupContent streams the archive of a ready backup.
func (h *handlers) GetBackupContent(w http.ResponseWriter, _ *http.Request, id gen.Id) {
	if h.b.Backups == nil {
		notImplemented(w, "backups")
		return
	}
	f, rec, err := h.b.Backups.Open(id)
	if err != nil {
		h.failBackup(w, "open backup", err)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.FormatInt(rec.Size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="holaclaw-backup-`+rec.ID+`.tar.gz"`)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, io.LimitReader(f, rec.Size)); err != nil {
		h.b.logger().Warn("api: backup stream interrupted", "backup_id", rec.ID, "error", err)
	}
}

// VerifyBackup re-checks an archive against its manifest. A damaged archive
// is an answer, not an error of the request.
func (h *handlers) VerifyBackup(w http.ResponseWriter, _ *http.Request, id gen.Id) {
	if h.b.Backups == nil {
		notImplemented(w, "backups")
		return
	}
	err := h.b.Backups.Verify(id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, gen.BackupVerification{Ok: true})
	case errors.Is(err, backup.ErrNotFound), errors.Is(err, backup.ErrNotReady):
		h.failBackup(w, "verify backup", err)
	default:
		writeJSON(w, http.StatusOK, gen.BackupVerification{Ok: false, Error: ptr(err.Error())})
	}
}

func (h *handlers) DeleteBackup(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if h.b.Backups == nil {
		notImplemented(w, "backups")
		return
	}
	if err := h.b.Backups.Delete(id, string(skillWho(r))); err != nil {
		h.failBackup(w, "delete backup", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// UploadBackup keeps an archive the client sends as a managed backup. The
// body is capped at the archive limit; past it the answer is 413.
func (h *handlers) UploadBackup(w http.ResponseWriter, r *http.Request) {
	if h.b.Backups == nil {
		notImplemented(w, "backups")
		return
	}
	rec, err := h.b.Backups.Import(r.Body, string(skillWho(r)))
	switch {
	case tooLarge(err):
		writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "payload too large")
	case errors.Is(err, backup.ErrBadArchive):
		badRequest(w, "not a holaclaw backup, or it does not match its manifest")
	case err != nil:
		h.failBackup(w, "upload backup", err)
	default:
		writeJSON(w, http.StatusCreated, toGenBackup(rec))
	}
}

// RestoreBackup stages the data of a backup and restarts the daemon onto
// it. Everything that can refuse does so before the answer: once this says
// 202 the restore is on disk and the next start applies it, whatever happens
// to this process.
func (h *handlers) RestoreBackup(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if h.b.Backups == nil || h.b.Restart == nil {
		notImplemented(w, "restore")
		return
	}
	who := string(skillWho(r))
	if err := h.b.Backups.StageRestore(id, who); err != nil {
		h.failBackup(w, "restore backup", err)
		return
	}
	rec, err := h.b.Backups.Get(id)
	if err != nil {
		rec = backup.Record{ID: id, Status: backup.StatusReady}
	}
	h.b.logger().Warn("api: restoring a backup, the daemon restarts", "backup_id", id, "by", who)
	writeJSON(w, http.StatusAccepted, toGenBackup(rec))
	// The shutdown this starts lets the request finish, so the answer is out.
	h.b.Restart()
}

// failBackup maps the backup errors onto the stable codes.
func (h *handlers) failBackup(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, backup.ErrNotFound):
		notFound(w)
	case errors.Is(err, backup.ErrBusy):
		writeError(w, http.StatusConflict, CodeConflict, "another backup is running, or a restore is under way")
	case errors.Is(err, backup.ErrBadArchive):
		writeError(w, http.StatusConflict, CodeConflict, "the archive is damaged: it does not match its manifest, or holds no database")
	case errors.Is(err, backup.ErrIncompatible):
		writeError(w, http.StatusConflict, CodeConflict, "the archive was written by a newer holaclaw: upgrade before restoring it")
	case errors.Is(err, backup.ErrNotReady):
		writeError(w, http.StatusConflict, CodeConflict, "the backup has no archive: it is still running, or it failed")
	default:
		h.b.fail(w, what, err)
	}
}

func toGenBackup(r backup.Record) gen.Backup {
	return gen.Backup{
		Id:              r.ID,
		Origin:          gen.BackupOrigin(r.Origin),
		Status:          gen.BackupStatus(r.Status),
		Reason:          optString(r.Reason),
		CreatedAt:       r.Created,
		Size:            r.Size,
		Version:         r.Version,
		SecretsIncluded: r.SecretsIncluded,
	}
}
