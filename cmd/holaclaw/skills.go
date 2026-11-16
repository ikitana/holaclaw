package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/fsutil"
	"holaclaw/internal/skill"
)

// newSkillsCmd is the skills group, through the API like memory. The explorer
// is what `holaclaw skills` on its own does (docs/skills.md §7), as `holaclaw
// memory` opens its browser; the subcommands stay for scripts.
func newSkillsCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "browse the skills of an agent, or manage them",
		Long: "browse the skills of an agent: pick the agent, then the level, then the skill\n\n" +
			"the subcommands are the same skills for scripts, through the daemon",
		Args: subcommands,
	}
	cn := c.connFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmdSkillsBrowse(cmd.Context(), c, cn) }

	// sub declares one subcommand with its own --addr and --token.
	sub := func(use, short string, args cobra.PositionalArgs) (*cobra.Command, *conn) {
		s := &cobra.Command{Use: use, Short: short, Args: args}
		return s, c.connFlags(s)
	}

	list, listConn := sub("list [agent]", "list the skills of an agent", cobra.MaximumNArgs(1))
	list.RunE = func(cmd *cobra.Command, args []string) error { return cmdSkillsList(cmd.Context(), c, listConn, args) }

	show, showConn := sub("show <name>", "show one skill and its versions", cobra.ExactArgs(1))
	showAgent := show.Flags().String("agent", "main", "agent the skill belongs to")
	show.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdSkillsShow(cmd.Context(), c, showConn, *showAgent, args[0])
	}

	install, installConn := sub("install <path|git-url>",
		"install a skill or a collection from a directory or a git repository", cobra.ExactArgs(1))
	var inst skillsInstallOptions
	flags := install.Flags()
	flags.StringVar(&inst.gitRef, "git-ref", "", "the tag, branch or commit of a git url; its default branch when omitted")
	flags.StringVar(&inst.gitPath, "git-path", "", "the skill or collection folder inside the repository")
	flags.StringVar(&inst.agent, "agent", "main", "agent to install the skill into")
	flags.StringVar(&inst.as, "as", "", "install under this name instead of the source's")
	flags.BoolVar(&inst.all, "all", false, "install every skill of a collection")
	flags.BoolVar(&inst.force, "force", false, "install although the scan found something")
	flags.BoolVar(&inst.yes, "yes", false, "do not ask; the preview is still shown")
	install.MarkFlagsMutuallyExclusive("as", "all")
	install.RunE = func(cmd *cobra.Command, args []string) error {
		if args[0] == "" {
			return usage(cmd)
		}
		return cmdSkillsInstall(cmd.Context(), c, installConn, inst, args[0])
	}

	remove, removeConn := sub("remove <agent> <name>", "move a skill to the trash, or delete it from there", cobra.ExactArgs(2))
	remove.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdSkillsRemove(cmd.Context(), c, removeConn, args[0], args[1])
	}

	restore, restoreConn := sub("restore <agent> <name>", "bring a skill back from the trash", cobra.ExactArgs(2))
	restore.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdSkillsRestore(cmd.Context(), c, restoreConn, args[0], args[1])
	}

	revert, revertConn := sub("revert <agent> <name>", "make an earlier version of a skill current", cobra.ExactArgs(2))
	to := revert.Flags().Int("to", 0, "version number from history/ to make current")
	required(revert, "to")
	revert.RunE = func(cmd *cobra.Command, args []string) error {
		if *to < 1 {
			return usagef(cmd, "--to must be a version number from history/, 1 or more")
		}
		return cmdSkillsRevert(cmd.Context(), c, revertConn, args[0], args[1], *to)
	}

	cmd.AddCommand(list, show, install, remove, restore, revert)
	return cmd
}

func cmdSkillsList(ctx context.Context, c *cli, cn *conn, pos []string) error {
	agent := "main"
	if len(pos) == 1 {
		agent = pos[0]
	}
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	skills, err := client.ListSkills(ctx, agent)
	if err != nil {
		return err
	}
	if len(skills) == 0 {
		_, _ = fmt.Fprintln(c.stdout, "no skills")
		return nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "LEVEL\tNAME\tELIGIBLE\tTAUGHT\tDESCRIPTION")
	for _, s := range skills {
		level := string(s.Level)
		if s.Trashed != nil && *s.Trashed {
			level += " (trash)"
		}
		taught := "-"
		if l := s.Learned; l != nil {
			taught = oneLine(author(s)) + " " + l.On.UTC().Format(time.DateOnly)
		}
		dot := "○"
		if s.Eligible {
			dot = "●"
		}
		desc := s.Description
		if h := hiddenText(s); h != "" {
			desc += " (" + h + ")"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", level, s.Name, dot, taught, oneLine(desc))
	}
	return w.Flush()
}

// author is who taught a saved skill: the display name, else the identity.
func author(s gen.Skill) string {
	if n := deref(s.Learned.ByName); n != "" {
		return n
	}
	return s.Learned.By
}

func cmdSkillsShow(ctx context.Context, c *cli, cn *conn, agent, name string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	s, err := client.GetSkill(ctx, name, agent)
	if err != nil {
		return err
	}
	printSkill(c, s)
	return nil
}

// printSkill is the provenance line the model reads, the frontmatter facts
// and an agent skill's live number as `key: value` lines, the body verbatim,
// then the versions of an agent skill.
func printSkill(c *cli, s gen.Skill) {
	line := func(k, v string) {
		if v != "" {
			_, _ = fmt.Fprintf(c.stdout, "%s: %s\n", k, v)
		}
	}
	line("name", s.Name)
	line("description", s.Description)
	line("level", string(s.Level))
	if s.Version != nil {
		line("version", strconv.Itoa(*s.Version))
	}
	line("provenance", provenance(s))
	if s.Trashed != nil && *s.Trashed {
		line("trashed", "yes")
	}
	if s.Eligible {
		line("eligible", "yes")
	} else {
		line("eligible", "no")
	}
	line("hidden", hiddenText(s))
	if l := s.Learned; l != nil {
		line("learned on", l.On.UTC().Format(time.RFC3339))
		line("learned by", oneLine(author(s)))
		line("learned from", l.From)
		line("origin", string(l.Origin))
	}
	if r := s.Requires; r != nil {
		line("requires tools", strings.Join(deref(r.Tools), ", "))
		line("requires bins", strings.Join(deref(r.Bins), ", "))
	}
	line("references", strings.Join(deref(s.References), ", "))
	line("scripts", strings.Join(deref(s.Scripts), ", "))
	if b := deref(s.Body); b != "" {
		_, _ = fmt.Fprintln(c.stdout)
		_, _ = fmt.Fprint(c.stdout, b)
		if !strings.HasSuffix(b, "\n") {
			_, _ = fmt.Fprintln(c.stdout)
		}
	}
	printVersions(c, s)
}

// provenance is the line skill.load puts before the body (docs/skills.md
// §4), rebuilt from the wire form with the teacher's display name.
func provenance(s gen.Skill) string {
	sk := skill.Skill{Name: s.Name, Level: skill.Level(s.Level)}
	var name string
	if l := s.Learned; l != nil {
		sk.Learned = &skill.Learned{On: l.On}
		name = deref(l.ByName)
	}
	return skill.Provenance(sk, name)
}

// printVersions lists an agent skill's history, newest first, each with who
// taught it and the trace that wrote it when its file says; nothing when
// there is none.
func printVersions(c *cli, s gen.Skill) {
	if s.Versions == nil || len(*s.Versions) == 0 {
		return
	}
	_, _ = fmt.Fprintln(c.stdout, "\nversions:")
	for _, v := range *s.Versions {
		line := "  " + strconv.Itoa(v.Version)
		if v.On != nil {
			line += "  " + v.On.Format("2006-01-02")
		}
		if by := versionBy(v); by != "" {
			line += "  by " + oneLine(by)
		}
		if v.Trace != nil {
			line += "  from " + *v.Trace
		}
		_, _ = fmt.Fprintln(c.stdout, line)
	}
}

// versionBy is who taught a version: the display name, else the identity,
// "" for a version a person wrote.
func versionBy(v gen.SkillVersion) string {
	if n := deref(v.ByName); n != "" {
		return n
	}
	return deref(v.By)
}

// skillsInstallOptions are the flags of `skills install`.
type skillsInstallOptions struct {
	gitRef, gitPath, agent, as string
	all, force, yes            bool
}

func cmdSkillsInstall(ctx context.Context, c *cli, cn *conn, o skillsInstallOptions, source string) error {
	gitRef, gitPath, agent, as, all, force, yes := &o.gitRef, &o.gitPath, &o.agent, &o.as, &o.all, &o.force, &o.yes
	in := gen.SkillInstall{Agent: *agent}
	if skill.IsGitURL(source) {
		in.Git = &gen.SkillGitSource{Url: source}
		if *gitRef != "" {
			in.Git.Ref = gitRef
		}
		if *gitPath != "" {
			in.Git.Path = gitPath
		}
	} else {
		if strings.Contains(source, "://") {
			_, err := skill.ParseGitURL(source) // meant as a url: say why it is not one
			return err
		}
		if *gitRef != "" || *gitPath != "" {
			return fmt.Errorf("--git-ref and --git-path go with a git url, and %s is not one", source)
		}
		var err error
		if source, err = localSource(source); err != nil {
			return err
		}
	}
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	if *as != "" {
		in.Name = as
	}
	if in.Git != nil {
		_, _ = fmt.Fprintf(c.stderr, "fetching %s…\n", source)
	} else {
		// The daemon may be on another host: this reads the folder and sends
		// it, and the preview and the install both name the one upload.
		_, _ = fmt.Fprintf(c.stderr, "uploading %s…\n", source)
		id, err := uploadSkill(ctx, client, source)
		if err != nil {
			return err
		}
		in.Upload = &id
	}
	preview := true
	in.Preview = &preview
	p, err := client.InstallSkill(ctx, in)
	if err != nil {
		return err
	}
	if p.Skills != nil {
		return installCollection(ctx, c, client, in, source, p, *all, *force, *yes)
	}
	s := *p.Skill
	s.Versions = nil
	printSkill(c, s)
	if len(p.Findings) == 0 {
		_, _ = fmt.Fprintln(c.stdout, "\nscan: clean")
	} else {
		_, _ = fmt.Fprintf(c.stdout, "\nscan: %d findings\n", len(p.Findings))
		for _, f := range p.Findings {
			_, _ = fmt.Fprintf(c.stdout, "  %s:%d %s %s\n", f.File, f.Line, f.Code, f.Message)
		}
		if !*force {
			_, _ = fmt.Fprintln(c.stderr, "refusing to install: run again with --force after reading the findings")
			return exitError(1)
		}
	}
	if !*yes && !confirm(c, fmt.Sprintf("install %s into agent %s?", s.Name, *agent)) {
		return nil
	}
	in.Preview, in.Hash = nil, &p.Hash
	if *force {
		in.Force = force
	}
	done, err := client.InstallSkill(ctx, in)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "installed %s into %s (%s)\n", done.Skill.Name, *agent, shortHash(done.Hash))
	return nil
}

// installCollection answers a preview that found a collection read from
// source (a git url, or a local folder): the table of its skills, then
// either the hints to install one or all, or (with all) the findings, the
// question and the install of every one, pinned to the preview's hash.
func installCollection(ctx context.Context, c *cli, client *api.Client, in gen.SkillInstall, source string, p gen.SkillInstalled, all, force, yes bool) error {
	entries := *p.Skills
	_, _ = fmt.Fprintf(c.stdout, "%d skills in %s:\n", len(entries), source)
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSCAN\tDESCRIPTION")
	var findings int
	for _, e := range entries {
		scan := "clean"
		if n := len(e.Findings); n > 0 {
			scan = fmt.Sprintf("%d findings", n)
			findings += n
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", e.Skill.Name, scan, oneLine(e.Skill.Description))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if !all {
		// A listing is the answer.
		if len(entries) > 0 {
			_, _ = fmt.Fprintf(c.stdout, "\ninstall one:  %s\n", installLine(source, in.Git, deref(entries[0].Path)))
		}
		_, _ = fmt.Fprintf(c.stdout, "install all:  %s --all\n", installLine(source, in.Git, ""))
		return nil
	}
	if findings == 0 {
		_, _ = fmt.Fprintln(c.stdout, "\nscan: clean")
	} else {
		_, _ = fmt.Fprintf(c.stdout, "\nscan: %d findings\n", findings)
		for _, e := range entries {
			for _, f := range e.Findings {
				_, _ = fmt.Fprintf(c.stdout, "  %s/%s:%d %s %s\n", deref(e.Path), f.File, f.Line, f.Code, f.Message)
			}
		}
		if !force {
			_, _ = fmt.Fprintln(c.stderr, "refusing to install: run again with --force after reading the findings")
			return exitError(1)
		}
	}
	if !yes && !confirm(c, fmt.Sprintf("install %d skills into agent %s?", len(entries), in.Agent)) {
		return nil
	}
	in.Preview, in.Hash, in.All = nil, &p.Hash, &all
	if force {
		in.Force = &force
	}
	done, err := client.InstallSkill(ctx, in)
	if err != nil {
		return err
	}
	if done.Skills != nil {
		for _, e := range *done.Skills {
			_, _ = fmt.Fprintf(c.stdout, "installed %s into %s (%s)\n", e.Skill.Name, in.Agent, shortHash(e.Hash))
		}
	}
	return nil
}

// confirm asks question and reads the answer; anything but a yes, a closed
// stdin included, prints "not installed".
func confirm(c *cli, question string) bool {
	_, _ = fmt.Fprintf(c.stdout, "%s [y/N] ", question)
	line, err := bufio.NewReader(c.stdin).ReadString('\n')
	if err != nil {
		// A closed stdin is a no; end the question's line.
		_, _ = fmt.Fprintln(c.stdout)
	}
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		_, _ = fmt.Fprintln(c.stdout, "not installed")
		return false
	}
	return true
}

// localSource is the absolute folder a local path names: the path itself,
// or the folder of a SKILL.md. It must be a skill (it holds SKILL.md) or a
// collection (skill.Candidates finds skill folders in it), checked here
// before anything is sent.
func localSource(p string) (string, error) {
	dir, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		if filepath.Base(dir) != "SKILL.md" {
			return "", fmt.Errorf("%s is not a folder or a SKILL.md", p)
		}
		dir = filepath.Dir(dir)
	}
	if st, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil && st.Mode().IsRegular() {
		return dir, nil
	}
	if found, err := skill.Candidates(dir); err != nil || len(found) == 0 {
		return "", fmt.Errorf("%s is not a skill (no SKILL.md) and holds no skill folders", p)
	}
	return dir, nil
}

// uploadSkill packs the folder dir as a gzipped tar, `.git` and `.holaclaw`
// left out and within the install limits, streams it to the daemon and
// returns the upload's id.
func uploadSkill(ctx context.Context, client *api.Client, dir string) (string, error) {
	o := fsutil.CopyOptions{
		Skip:           func(_ string, d fs.DirEntry) bool { return d.Name() == ".git" || d.Name() == ".holaclaw" },
		RefuseSymlinks: true, MaxFiles: skill.MaxInstallFiles, MaxBytes: skill.MaxInstallTotal,
	}
	pr, pw := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		err := fsutil.WriteTarGz(pw, dir, o)
		_ = pw.CloseWithError(err)
		packed <- err
	}()
	up, err := client.UploadSkill(ctx, pr, dir)
	// A daemon that stopped reading leaves the writer blocked: this ends it.
	_ = pr.Close()
	// The folder's own error (a symlink, a limit) says more than the
	// request's.
	if perr := <-packed; perr != nil && !errors.Is(perr, io.ErrClosedPipe) {
		return "", fmt.Errorf("%s: %w", dir, perr)
	}
	if err != nil {
		return "", err
	}
	return up.Id, nil
}

// installLine is the command that installs sub, a folder of the collection
// read from source ("" for the whole of it): with git, sub joins --git-path;
// a local sub is the subfolder.
func installLine(source string, g *gen.SkillGitSource, sub string) string {
	if g == nil {
		return "holaclaw skills install " + filepath.Join(source, filepath.FromSlash(sub))
	}
	line := "holaclaw skills install " + g.Url
	if r := deref(g.Ref); r != "" {
		line += " --git-ref " + r
	}
	if p := path.Join(deref(g.Path), sub); p != "" {
		line += " --git-path " + p
	}
	return line
}

// shortHash is `sha256:` and the first 12 hex digits, enough to tell two
// installs apart.
func shortHash(h string) string {
	const prefix = "sha256:"
	if strings.HasPrefix(h, prefix) && len(h) > len(prefix)+12 {
		return h[:len(prefix)+12]
	}
	return h
}

func cmdSkillsRemove(ctx context.Context, c *cli, cn *conn, agent, name string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	if err := client.RemoveSkill(ctx, name, agent); err != nil {
		return err
	}
	// 204 either way: moved to the trash or deleted from it. The hint is for
	// the first case, the one a person is likely to regret.
	_, _ = fmt.Fprintf(c.stdout, "removed %s from %s; a live skill went to the trash: holaclaw skills restore %s %s\n",
		name, agent, agent, name)
	return nil
}

func cmdSkillsRestore(ctx context.Context, c *cli, cn *conn, agent, name string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	if _, err := client.RestoreSkill(ctx, name, agent); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "restored %s of %s\n", name, agent)
	return nil
}

func cmdSkillsRevert(ctx context.Context, c *cli, cn *conn, agent, name string, to int) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	s, err := client.RevertSkill(ctx, name, agent, to)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "reverted %s of %s\n", name, agent)
	printVersions(c, s)
	return nil
}

// hiddenText is why a skill is hidden, in words: the message, after the
// code of a load problem; the bare reason when the daemon sent no message.
func hiddenText(s gen.Skill) string {
	h := s.Hidden
	if h == nil {
		return ""
	}
	msg := deref(h.Message)
	if msg == "" {
		msg = string(h.Reason)
		if sub := deref(h.Subject); sub != "" {
			msg += " " + sub
		}
	}
	if c := deref(h.Code); c != "" {
		return c + ": " + msg
	}
	return msg
}
