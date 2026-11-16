package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/app"
	"holaclaw/internal/backup"
	"holaclaw/internal/config"
	"holaclaw/internal/daemon"
	"holaclaw/internal/fsutil"
)

// backupPoll is how often `backup create --wait` asks how the backup is doing.
const backupPoll = 200 * time.Millisecond

// newBackupCmd is two things under one name. With a subcommand it manages
// the backups the running daemon takes and keeps, and restores one, through
// the Control API; `restore <file>` alone works on the files, with no daemon.
// With none it writes an archive itself, offline and with no live DB handle,
// so a stopped or broken installation can still be backed up: when the
// daemon is running and left data in the WAL, internal/backup refuses and
// says so — that message is passed through unchanged (ARCHITECTURE.md §4).
func newBackupCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "archive the data and config, or manage the daemon's backups",
		Args:  subcommands,
	}
	out := cmd.Flags().String("out", "", "output file or directory (default: this directory)")
	withSecrets := cmd.Flags().Bool("include-secrets", false, "include auth.toml (a backup is meant to travel: off by default)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		dataDir, secretsPath := backupDirs(c.configDir)
		path, err := backup.Create(cmd.Context(), backup.Options{
			DataDir:        dataDir,
			ConfigDir:      c.configDir,
			SecretsPath:    secretsPath,
			Out:            *out,
			IncludeSecrets: *withSecrets,
		})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(c.stdout, path)
		return nil
	}

	// sub declares one subcommand with its own --addr and --token.
	sub := func(use, short string, args cobra.PositionalArgs) (*cobra.Command, *conn) {
		s := &cobra.Command{Use: use, Short: short, Args: args}
		return s, c.connFlags(s)
	}

	list, listConn := sub("list", "list the backups the daemon keeps", cobra.NoArgs)
	listJSON := list.Flags().Bool("json", false, "print the backups as JSON")
	list.RunE = func(cmd *cobra.Command, _ []string) error {
		return cmdBackupList(cmd.Context(), c, listConn, *listJSON)
	}

	create, createConn := sub("create", "have the daemon take a backup", cobra.NoArgs)
	var co backupCreateOptions
	create.Flags().BoolVar(&co.withSecrets, "include-secrets", false, "include auth.toml: whoever downloads the archive holds every credential")
	create.Flags().BoolVar(&co.wait, "wait", false, "wait until the backup is ready or has failed")
	create.Flags().BoolVar(&co.asJSON, "json", false, "print the backup as JSON")
	create.RunE = func(cmd *cobra.Command, _ []string) error { return cmdBackupCreate(cmd.Context(), c, createConn, co) }

	download, downloadConn := sub("download <id>", "fetch the archive of a backup", cobra.ExactArgs(1))
	downloadOut := download.Flags().String("out", "", "output file or directory (default: holaclaw-backup-<id>.tar.gz in this directory)")
	download.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdBackupDownload(cmd.Context(), c, downloadConn, args[0], *downloadOut)
	}

	upload, uploadConn := sub("upload <file>", "hand the daemon an archive to keep", cobra.ExactArgs(1))
	upload.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdBackupUpload(cmd.Context(), c, uploadConn, args[0])
	}

	verify, verifyConn := sub("verify <id|file>", "check an archive against its manifest", cobra.ExactArgs(1))
	verify.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdBackupVerify(cmd.Context(), c, verifyConn, args[0])
	}

	restore, restoreConn := sub("restore <id|file>", "restore a backup of the daemon, or a whole installation from an archive", cobra.ExactArgs(1))
	yes := restore.Flags().Bool("yes", false, "do not ask for confirmation")
	restore.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdBackupRestore(cmd.Context(), c, restoreConn, args[0], *yes)
	}

	rm, rmConn := sub("rm <id>", "delete a backup", cobra.ExactArgs(1))
	rm.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdBackupRemove(cmd.Context(), c, rmConn, args[0])
	}

	cmd.AddCommand(list, create, download, upload, verify, restore, rm)
	return cmd
}

func cmdBackupList(ctx context.Context, c *cli, cn *conn, asJSON bool) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	backups, err := client.ListBackups(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		if backups == nil {
			backups = []gen.Backup{}
		}
		return writeIndented(c.stdout, backups)
	}
	if len(backups) == 0 {
		_, _ = fmt.Fprintln(c.stdout, "no backups")
		return nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tCREATED\tORIGIN\tSTATUS\tSIZE\tSECRETS\tVERSION")
	for _, b := range backups {
		status := string(b.Status)
		if b.Reason != nil && *b.Reason != "" {
			status += ": " + *b.Reason
		}
		secrets := "no"
		if b.SecretsIncluded {
			secrets = "yes"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			b.Id, stamp(b.CreatedAt), b.Origin, status, b.Size, secrets, b.Version)
	}
	return w.Flush()
}

// backupCreateOptions are the flags of `backup create`.
type backupCreateOptions struct {
	withSecrets, wait, asJSON bool
}

// cmdBackupCreate asks the daemon for a backup. Without --wait it returns as
// soon as the daemon has accepted it; with it, a backup that fails is exit 1.
func cmdBackupCreate(ctx context.Context, c *cli, cn *conn, o backupCreateOptions) error {
	withSecrets, wait, asJSON := &o.withSecrets, &o.wait, &o.asJSON
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	in := gen.BackupCreate{}
	if *withSecrets {
		in.IncludeSecrets = withSecrets
	}
	b, err := client.CreateBackup(ctx, in)
	if err != nil {
		return err
	}
	for *wait && b.Status == gen.BackupStatusRunning {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backupPoll):
		}
		if b, err = client.GetBackup(ctx, b.Id); err != nil {
			return err
		}
	}
	if *asJSON {
		if err := writeIndented(c.stdout, b); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintf(c.stdout, "id      %s\nstatus  %s\n", b.Id, b.Status)
		if b.Status == gen.BackupStatusReady {
			_, _ = fmt.Fprintf(c.stdout, "size    %d\n", b.Size)
		}
	}
	if b.Status == gen.BackupStatusFailed {
		return fmt.Errorf("backup %s failed: %s", b.Id, deref(b.Reason))
	}
	return nil
}

// cmdBackupDownload fetches an archive into a file that only exists once it
// is whole: a temp file beside it, renamed into place.
func cmdBackupDownload(ctx context.Context, c *cli, cn *conn, id, out string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	path := downloadPath(out, id)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".holaclaw-download-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed
	_, err = client.DownloadBackup(ctx, id, tmp)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// CreateTemp made it 0600 and rename keeps that: an archive is private.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(c.stdout, path)
	return nil
}

// downloadPath turns --out into a file path, the way the offline backup
// reads its own --out: a directory gets the default name inside it.
func downloadPath(out, id string) string {
	name := "holaclaw-backup-" + filepath.Base(id) + ".tar.gz"
	if out == "" {
		return name
	}
	if strings.HasSuffix(out, string(filepath.Separator)) {
		return filepath.Join(out, name)
	}
	if info, err := os.Stat(out); err == nil && info.IsDir() {
		return filepath.Join(out, name)
	}
	return out
}

// cmdBackupVerify checks an archive against its manifest: a file is read
// here, with no daemon; anything else is the id of a managed backup, which
// the daemon checks where it lies.
func cmdBackupVerify(ctx context.Context, c *cli, cn *conn, target string) error {
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		if err := backup.Verify(target); err != nil {
			return err
		}
	} else {
		client, err := cn.dial(ctx)
		if err != nil {
			return err
		}
		v, err := client.VerifyBackup(ctx, target)
		if err != nil {
			var ce *api.ClientError
			if errors.As(err, &ce) && ce.Code == api.CodeNotFound {
				return fmt.Errorf("%s is neither a file nor a backup of this daemon", target)
			}
			return err
		}
		if !v.Ok {
			return errors.New(deref(v.Error))
		}
	}
	_, _ = fmt.Fprintf(c.stdout, "ok: %s matches its manifest\n", target)
	return nil
}

// cmdBackupUpload hands the daemon an archive to keep: one the offline
// backup wrote, or one downloaded from this or another installation.
func cmdBackupUpload(ctx context.Context, c *cli, cn *conn, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	b, err := client.UploadBackup(ctx, f)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "id      %s\nstatus  %s\nsize    %d\n", b.Id, b.Status, b.Size)
	return nil
}

// cmdBackupRestore is two restores. Given the id of a managed backup it
// replaces the daemon's database and blobs with that backup's and has the
// daemon restart onto them. Given an archive file it recreates the whole
// installation from it — config, secrets if the archive has them, database
// and blobs — with no daemon, for a new host or one whose daemon is down.
// Both destroy something, so they ask first unless told --yes.
func cmdBackupRestore(ctx context.Context, c *cli, cn *conn, id string, yes bool) error {
	if info, err := os.Stat(id); err == nil && !info.IsDir() {
		return restoreFile(ctx, c, *cn.dir, id, yes)
	} else if err != nil && looksLikeFile(id) {
		return err // a mistyped path is not the id of a backup
	}
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	b, err := client.GetBackup(ctx, id)
	if err != nil {
		return err
	}
	if !yes {
		_, _ = fmt.Fprintf(c.stderr, "Restore backup %s of %s?\n"+
			"The database and the blobs are replaced: everything stored since then is gone,\n"+
			"API tokens included, and the daemon restarts. Config files and auth.toml stay.\n",
			b.Id, stamp(b.CreatedAt))
		if !confirmed(c, "not restored") {
			return exitError(1)
		}
	}
	if _, err := client.RestoreBackup(ctx, id); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "restoring %s: the daemon is restarting onto it\n", id)
	return nil
}

// looksLikeFile tells a path from the id of a managed backup, which has
// neither a separator nor an extension.
func looksLikeFile(s string) bool {
	return strings.ContainsRune(s, filepath.Separator) || filepath.Ext(s) != ""
}

// confirmed asks for a typed yes, and prints refused when it did not get
// one.
func confirmed(c *cli, refused string) bool {
	_, _ = fmt.Fprint(c.stderr, "Type yes to go on: ")
	answer, _ := bufio.NewReader(c.stdin).ReadString('\n')
	if strings.TrimSpace(answer) != "yes" {
		_, _ = fmt.Fprintln(c.stderr, refused)
		return false
	}
	return true
}

// restoreFile recreates the installation of the config directory from an
// archive, offline. Where the data and the secrets go is what the archive's
// own config.toml says, since that is the config the daemon will then read.
// Everything it would create or replace is printed before it asks.
func restoreFile(ctx context.Context, c *cli, dir, archive string, yes bool) error {
	cfgDir, err := fsutil.Abs(dir)
	if err != nil {
		return err
	}
	if daemonAnswers(ctx, cfgDir) {
		return fmt.Errorf("the daemon of %s is running: stop it first, or hand it the archive with "+
			"`holaclaw backup upload` and restore that backup by its id", cfgDir)
	}
	manifest, cfgTOML, err := backup.Inspect(archive)
	if err != nil {
		return err
	}
	target, err := restoreTarget(cfgDir, cfgTOML)
	if err != nil {
		return err
	}
	plan, err := backup.PlanInstall(manifest, target, time.Now())
	if err != nil {
		return err
	}
	printPlan(c.stderr, archive, plan)
	if !yes && !confirmed(c, "not restored") {
		return exitError(1)
	}
	if err := backup.Install(archive, plan); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "restored %s into %s\n", archive, cfgDir)
	_, _ = fmt.Fprintf(c.stderr, "Review it with `holaclaw check --config %s`, then start the daemon with `holaclaw run --config %s`.\n", cfgDir, cfgDir)
	return nil
}

// daemonAnswers reports whether a daemon is serving the installation in dir.
// A directory with no config that loads has none.
func daemonAnswers(ctx context.Context, dir string) bool {
	a, err := app.Static(dir)
	if err != nil {
		return false
	}
	client, err := api.Dial(daemon.SocketPath(a), "")
	if err != nil {
		return false
	}
	_, err = client.Status(ctx)
	var ce *api.ClientError
	return err == nil || errors.As(err, &ce)
}

// restoreTarget resolves where an archive is restored to: the data and
// secrets directories its config.toml names, read the way the daemon will.
// An archive with no config.toml goes where the config already there says.
func restoreTarget(cfgDir string, cfgTOML []byte) (backup.Target, error) {
	if cfgTOML == nil {
		dataDir, secretsPath := backupDirs(cfgDir)
		return backup.Target{ConfigDir: cfgDir, DataDir: dataDir, SecretsPath: secretsPath}, nil
	}
	s, err := parseServerDirs(cfgTOML)
	if err != nil {
		return backup.Target{}, fmt.Errorf("the archive's %w", err)
	}
	return backup.Target{
		ConfigDir:   cfgDir,
		DataDir:     app.DataDir(cfgDir, s.DataDir),
		SecretsPath: secretsPathOf(&config.Config{Server: s}),
	}, nil
}

// printPlan shows what a restore is about to do: the directories, every file
// it creates or replaces and where a replaced one is kept.
func printPlan(w io.Writer, archive string, p *backup.Plan) {
	t := p.Target
	_, _ = fmt.Fprintf(w, "Restore %s, taken %s, into:\n", archive, stamp(p.Created))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "  config\t%s\n  data\t%s\n  secrets\t%s\n", t.ConfigDir, t.DataDir, t.SecretsPath)
	_ = tw.Flush()
	for _, d := range p.Dirs {
		_, _ = fmt.Fprintf(w, "%s does not exist and is created.\n", d)
	}
	_, _ = fmt.Fprintln(w)
	same, replacesDB := 0, false
	for _, ch := range p.Changes {
		switch ch.Action {
		case backup.ActionKeep:
			same++
		case backup.ActionReplace:
			replacesDB = replacesDB || ch.Path == filepath.Join(t.DataDir, "holaclaw.db")
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t(the current one is kept as %s)\n", ch.Action, ch.Path, ch.Kept)
		default:
			_, _ = fmt.Fprintf(tw, "  %s\t%s\n", ch.Action, ch.Path)
		}
	}
	_ = tw.Flush()
	if same > 0 {
		_, _ = fmt.Fprintf(w, "  %d already as in the backup, left untouched.\n", same)
	}
	_, _ = fmt.Fprintf(w, "  %d blobs, %d of them new.\n\n", p.Blobs, p.NewBlobs)
	if !p.Secrets {
		state := "it is left as it is"
		if _, err := os.Stat(t.SecretsPath); err != nil {
			state = "there is none, so credentials have to be set again (`holaclaw auth`, `holaclaw login`)"
		}
		_, _ = fmt.Fprintf(w, "The archive holds no secrets (auth.toml): %s.\n", state)
	}
	if replacesDB {
		_, _ = fmt.Fprintln(w, "The database is replaced: everything stored since the backup is gone, API tokens included.")
	}
	_, _ = fmt.Fprintln(w, "Files the archive does not hold are left alone.")
}

func cmdBackupRemove(ctx context.Context, c *cli, cn *conn, id string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	if err := client.DeleteBackup(ctx, id); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "removed %s\n", id)
	return nil
}

func writeIndented(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// backupDirs resolves server.data_dir and the auth.toml path without failing
// on a config that does not fully validate: a backup of a broken installation
// is exactly what an operator wants before repairing it.
func backupDirs(dir string) (dataDir, secretsPath string) {
	cfg, err := config.Load(dir)
	var ps config.Problems
	if err != nil && !errors.As(err, &ps) {
		cfg = nil
	}
	return dataDirOf(cfg, dir), secretsPathOf(cfg)
}
