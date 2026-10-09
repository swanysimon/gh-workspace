package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/pflag"
	"github.com/swanysimon/gh-workspace/internal/engine"
	"github.com/swanysimon/gh-workspace/internal/settings"
)

// runWorktree dispatches the "gh workspace worktree <verb>" subcommands. It
// is a thin wrapper around "git worktree": this tool only resolves the
// <org>/<repo> argument to a clone path (cloning it first if needed) and
// guards the one destructive interaction with archiving; git does
// everything else it already does correctly (branch DWIM, dirty-tree
// refusal, etc).
func runWorktree(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printWorktreeUsage(stderr)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	verb, rest := args[0], args[1:]
	switch verb {
	case "add":
		return cmdWorktreeAdd(ctx, rest, stdout, stderr)
	case "remove":
		return cmdWorktreeRemove(ctx, rest, stdout, stderr)
	case "list":
		return cmdWorktreeList(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown worktree subcommand %q\n", verb)
		printWorktreeUsage(stderr)
		return exitUsage
	}
}

func printWorktreeUsage(w io.Writer) {
	fmt.Fprintln(w, "USAGE")
	fmt.Fprintln(w, "  gh workspace worktree add [flags] <org>/<repo> <branch> [path]")
	fmt.Fprintln(w, "  gh workspace worktree remove [--force] [flags] <org>/<repo> <path> | <path>")
	fmt.Fprintln(w, "  gh workspace worktree list [flags] [<org>/<repo>|<org>]")
}

// newSubcommandFlagSet makes a subcommand's flag set whose -h/--help
// output is the subcommand's own usage line followed by its flags, plus
// the "help" flag every FlagSet carries (see main.go's newFlagSet). Used
// by every worktree subcommand, plus clone and untrack.
func newSubcommandFlagSet(name, usage string, stderr io.Writer) (*pflag.FlagSet, *bool) {
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	help := fs.BoolP("help", "h", false, "show help")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "USAGE")
		fmt.Fprintln(stderr, "  "+usage)
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "FLAGS")
		printFlagTable(stderr, flagHelpsFromSet(fs, defaultConfig()))
	}
	return fs, help
}

// resolveSubcommandConfig applies the same root/protocol/timeout/config
// precedence (flags > env > file > defaults) as the sync command, but only
// for the settings cmd declares in settingsTable; cfg.Owner is left unset
// for the caller to fill in once it has parsed <org>/<repo> out of the
// positional args. help is the pointer newSubcommandFlagSet returned; the
// caller must have added every other flag it wants (e.g. --force) to fs
// before calling this, since it parses fs itself. The returned *fileConfig
// is nil only on error; a caller that needs to check whether the config's
// own owners/repos lists track a repo (e.g. cmdUntrack) reads it directly
// rather than this function re-deriving that check for every caller.
func resolveSubcommandConfig(fs *pflag.FlagSet, help *bool, cmd commandID, args []string) (config, *fileConfig, []string, error) {
	var configPath string
	bound := bindSettings(fs, cmd)
	fs.StringVar(&configPath, "config", "", "path to a JSON config file")

	if err := fs.Parse(args); err != nil {
		fs.Usage()
		return config{}, nil, nil, err
	}
	if *help {
		fs.Usage()
		return config{}, nil, nil, errHelpRequested
	}
	positional := fs.Args()

	cfg := defaultConfig()

	fc, err := loadFileConfig(resolveConfigPath(configPath))
	if err != nil {
		return config{}, nil, nil, err
	}
	if err := resolveSettings(&cfg, cmd, fs, bound, fc); err != nil {
		return config{}, nil, nil, err
	}
	if err := expandConfigPaths(&cfg); err != nil {
		return config{}, nil, nil, err
	}

	if err := settings.Validate(cfg.Settings); err != nil {
		return config{}, nil, nil, err
	}

	return cfg, fc, positional, nil
}

// parseOwnerRepo splits "<org>/<repo>" and validates both halves against
// the same rules the sync command already trusts: settings.ValidOwnerName
// for the org, validRepoName for the repo (it becomes a path segment and a
// git argument).
func parseOwnerRepo(s string) (owner, repo string, err error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected <org>/<repo>, got %q", s)
	}
	owner, repo = parts[0], parts[1]
	if !settings.ValidOwnerName(owner) {
		return "", "", fmt.Errorf("org %q is not a valid GitHub org name", owner)
	}
	if !validRepoName(repo) {
		return "", "", fmt.Errorf("repo %q is not a valid repo name", repo)
	}
	return owner, repo, nil
}

func cmdWorktreeAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "gh workspace worktree add [flags] <org>/<repo> <branch> [path]"
	fs, help := newSubcommandFlagSet("gh workspace worktree add", usage, stderr)
	cfg, _, rest, err := resolveSubcommandConfig(fs, help, cmdIDWorktreeAdd, args)
	if errors.Is(err, errHelpRequested) {
		return exitSuccess
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(rest) != 2 && len(rest) != 3 {
		fmt.Fprintln(stderr, "usage: "+usage)
		return exitUsage
	}
	owner, repoName, err := parseOwnerRepo(rest[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	branch := rest[1]
	cfg.Owner = owner

	// An explicit path argument always wins, resolved against the current
	// directory exactly as before and never subject to the under-
	// worktree-root check -- "the user always specifies where a worktree
	// lives" (AGENTS.md): where the command runs, a configured template,
	// or an explicit path are all the user choosing. Only when path is
	// omitted does the worktree-root/worktree-path template apply.
	var path string
	if len(rest) == 3 {
		path, err = absWorktreePath(rest[2])
	} else {
		path, err = resolveWorktreePath(cfg, owner, repoName, branch)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}

	if _, err := exec.LookPath("gh"); err != nil {
		fmt.Fprintln(stderr, "gh-workspace requires the gh CLI on PATH:", err)
		return exitRuntimeFail
	}
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(stderr, "gh-workspace requires git on PATH:", err)
		return exitRuntimeFail
	}

	gctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	repo, err := getRepo(gctx, cfg, owner+"/"+repoName)
	cancel()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}

	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}

	// Held for the whole command, not just the clone: a sync run archiving
	// this repo checks for linked worktrees and then deletes the clone, so
	// a worktree added in between would be orphaned.
	release, err := acquireLock(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}
	defer release()

	dir := filepath.Join(reposDir(cfg), repo.Name)
	_, dirErr := os.Stat(dir)
	dirExists := dirErr == nil
	_, gitErr := os.Stat(filepath.Join(dir, ".git"))
	isGitDir := gitErr == nil

	switch {
	case dirExists && !isGitDir:
		fmt.Fprintf(stderr, "%s exists but is not a git repository\n", dir)
		return exitRuntimeFail
	case !dirExists && repo.IsArchived && localArchiveExists(cfg, repo.Name):
		// The repo is already on disk, as a tarball: re-cloning it only
		// for this command to refuse, and for the next sync to archive
		// it all over again, would be pure waste.
		fmt.Fprintf(stderr, "repo %q is archived upstream and already archived locally at %s; not adding a worktree\n", repo.NameWithOwner, tarballPath(cfg, repo.Name))
		return exitRuntimeFail
	case !dirExists:
		if err := ensureClonedForWorktree(ctx, cfg, repo, stderr); err != nil {
			fmt.Fprintln(stderr, err)
			return exitRuntimeFail
		}
	}

	// Cloning an archived repo is still useful (it's how you'd get a
	// worktree-free reference checkout), but adding a worktree to one
	// implies ongoing work, which archived repos are not expected to get.
	// Check after cloning, not before, so the clone still lands even when
	// this command then refuses.
	if repo.IsArchived {
		fmt.Fprintf(stderr, "repo %q is archived upstream; not adding a worktree (the clone at %s is still there for reference)\n", repo.NameWithOwner, dir)
		return exitRuntimeFail
	}

	// A clone that already existed may predate the branch being pushed.
	// Failing to fetch (e.g. offline) is not fatal: the local refs may
	// still be enough to add the worktree.
	if dirExists {
		if err := fetchRepo(ctx, cfg, dir); err != nil {
			fmt.Fprintf(stderr, "warning: could not fetch before adding worktree, using local refs: %v\n", err)
		}
	}

	// A computed (not explicit) path that already exists needs a better
	// hint than git's own error text would give: the obvious next step is
	// an explicit path or a template that includes {branch}, not a
	// generated alternative name -- "never pick an alternative name," per
	// AIDEV.md. An explicit path hitting this is left to git's own error,
	// unchanged from before this command could compute a path at all.
	if len(rest) != 3 {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(stderr, "worktree path %q already exists; pass an explicit path, or set --worktree-path to a template that resolves to a different path for this branch (note: \"/\" in a branch name is replaced with \"-\", so e.g. \"feat/x\" and \"feat-x\" can collide even with {branch} in the template)\n", path)
			return exitRuntimeFail
		}
	}

	defaultBranch := ""
	if repo.DefaultBranch != nil {
		defaultBranch = repo.DefaultBranch.Name
	}
	gitArgs, created, err := worktreeAddArgs(ctx, cfg, dir, path, branch, defaultBranch)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}
	if _, err := runGit(ctx, cfg, dir, gitArgs...); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}
	if created != "" {
		fmt.Fprintf(stdout, "created new branch %q from %s\n", branch, created)
	}
	fmt.Fprintf(stdout, "added worktree for %s@%s at %s\n", repo.NameWithOwner, branch, path)
	return exitSuccess
}

// worktreeAddArgs picks how to check out branch. A local branch, or a
// remote-tracking origin/<branch> (which git DWIMs into a local tracking
// branch), is checked out as-is. A branch that exists nowhere is created
// from the default branch's remote-tracking ref (falling back to HEAD) with
// --no-track, so it doesn't end up with the default branch as its upstream.
// createdFrom is non-empty only when a new branch is being created.
func worktreeAddArgs(ctx context.Context, cfg config, dir, path, branch, defaultBranch string) (args []string, createdFrom string, err error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	if _, err := cfg.Deps.exec(ctx, dir, "git", "check-ref-format", "--branch", branch); err != nil {
		return nil, "", fmt.Errorf("%q is not a valid branch name", branch)
	}

	refExists := func(ref string) bool {
		_, err := cfg.Deps.exec(ctx, dir, "git", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		return err == nil
	}
	if refExists("refs/heads/"+branch) || refExists("refs/remotes/origin/"+branch) {
		return []string{"worktree", "add", "--", path, branch}, "", nil
	}

	base := "HEAD"
	if defaultBranch != "" && refExists("refs/remotes/origin/"+defaultBranch) {
		base = "origin/" + defaultBranch
	}
	return []string{"worktree", "add", "--no-track", "-b", branch, "--", path, base}, base, nil
}

// absWorktreePath resolves a user-supplied worktree path against the
// caller's working directory. git is run with its working directory set to
// the central clone, so a relative path passed through unchanged would be
// resolved against the clone and land the worktree inside it.
func absWorktreePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("worktree path must not be empty")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolving worktree path %q: %w", p, err)
	}
	return abs, nil
}

// ensureClonedForWorktree clones a repo outside of a normal sync run and
// writes the same state.json entry a sync run would, so a later sync doesn't
// find a directory it doesn't remember creating. The caller must hold the
// owner's lock. engine.EnsureCloned is the real implementation now.
func ensureClonedForWorktree(ctx context.Context, cfg config, repo ghRepo, stderr io.Writer) error {
	return engine.EnsureCloned(ctx, buildEnv(cfg), repo, stderr)
}

func cmdWorktreeRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "gh workspace worktree remove [--force] [flags] <org>/<repo> <path> | <path>"
	fs, help := newSubcommandFlagSet("gh workspace worktree remove", usage, stderr)
	var force bool
	fs.BoolVar(&force, "force", false, "remove even if the worktree has uncommitted changes")
	cfg, _, rest, err := resolveSubcommandConfig(fs, help, cmdWorktree, args)
	if errors.Is(err, errHelpRequested) {
		return exitSuccess
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(rest) != 1 && len(rest) != 2 {
		fmt.Fprintln(stderr, "usage: "+usage)
		return exitUsage
	}

	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(stderr, "gh-workspace requires git on PATH:", err)
		return exitRuntimeFail
	}

	// The path-only form finds the owning repo via the worktree's own
	// ".git" file, which (for a worktree, not a normal clone) is a plain
	// text file naming the central clone's "<root>/<owner>/repos/<repo>"
	// directory, not another repo's directory. See
	// resolveWorktreeOwnerRepo.
	var owner, repoName, path string
	if len(rest) == 1 {
		path, err = absWorktreePath(rest[0])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		owner, repoName, err = resolveWorktreeOwnerRepo(cfg, path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRuntimeFail
		}
		cfg.Owner = owner
	} else {
		owner, repoName, err = parseOwnerRepo(rest[0])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		cfg.Owner = owner
		path, err = absWorktreePath(rest[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
	}

	dir := filepath.Join(reposDir(cfg), repoName)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		fmt.Fprintf(stderr, "no local clone of %s/%s at %s\n", owner, repoName, dir)
		return exitRuntimeFail
	}

	gitArgs := []string{"worktree", "remove"}
	if force {
		gitArgs = append(gitArgs, "--force")
	}
	gitArgs = append(gitArgs, "--", path)
	if _, err := runGit(ctx, cfg, dir, gitArgs...); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}
	fmt.Fprintf(stdout, "removed worktree %s\n", path)
	return exitSuccess
}

func cmdWorktreeList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "gh workspace worktree list [flags] [<org>/<repo>|<org>]"
	fs, help := newSubcommandFlagSet("gh workspace worktree list", usage, stderr)
	cfg, _, rest, err := resolveSubcommandConfig(fs, help, cmdWorktree, args)
	if errors.Is(err, errHelpRequested) {
		return exitSuccess
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(rest) > 1 {
		fmt.Fprintln(stderr, "usage: "+usage)
		return exitUsage
	}

	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(stderr, "gh-workspace requires git on PATH:", err)
		return exitRuntimeFail
	}

	if len(rest) == 0 {
		// No arg: every worktree in the workspace, across every owner
		// directory under cfg.Root -- see AIDEV.md's "worktree list
		// [<owner>[/<repo>]] | No arg lists every worktree in the
		// workspace."
		ownerEntries, err := os.ReadDir(cfg.Root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRuntimeFail
		}
		failed := false
		for _, oe := range ownerEntries {
			if !oe.IsDir() {
				continue
			}
			ownerCfg := cfg
			ownerCfg.Owner = oe.Name()
			if listOwnerWorktrees(ctx, ownerCfg, stdout, stderr, true) {
				failed = true
			}
		}
		if failed {
			return exitRuntimeFail
		}
		return exitSuccess
	}

	if strings.Contains(rest[0], "/") {
		owner, repoName, err := parseOwnerRepo(rest[0])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		cfg.Owner = owner
		dir := filepath.Join(reposDir(cfg), repoName)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			fmt.Fprintf(stderr, "no local clone of %s/%s at %s\n", owner, repoName, dir)
			return exitRuntimeFail
		}
		if err := printWorktrees(ctx, cfg, repoName, repoName, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return exitRuntimeFail
		}
		return exitSuccess
	}

	if !settings.ValidOwnerName(rest[0]) {
		fmt.Fprintf(stderr, "org %q is not a valid GitHub org name\n", rest[0])
		return exitUsage
	}
	cfg.Owner = rest[0]

	if listOwnerWorktrees(ctx, cfg, stdout, stderr, false) {
		return exitRuntimeFail
	}
	return exitSuccess
}

// listOwnerWorktrees prints every repo's worktrees under one owner.
// qualifyWithOwner controls whether each repo's header is "<repo>:" (the
// existing "worktree list <org>" behavior, unchanged) or "<owner>/<repo>:"
// (the bare, no-arg, whole-workspace form, where the owner would
// otherwise be ambiguous). Reports true if any repo's listing failed, so
// the caller can keep going through every other owner/repo and still
// signal failure once at the end.
func listOwnerWorktrees(ctx context.Context, cfg config, stdout, stderr io.Writer, qualifyWithOwner bool) bool {
	entries, err := os.ReadDir(reposDir(cfg))
	if err != nil {
		if qualifyWithOwner && os.IsNotExist(err) {
			// A bare, whole-workspace listing may see an owner directory
			// that predates "repos/" existing as a concept (or simply has
			// nothing cloned yet); that's not a failure.
			return false
		}
		fmt.Fprintln(stderr, err)
		return true
	}

	failed := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(reposDir(cfg), e.Name(), ".git")); err != nil {
			continue
		}
		label := e.Name()
		if qualifyWithOwner {
			label = cfg.Owner + "/" + label
		}
		if err := printWorktrees(ctx, cfg, e.Name(), label, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			failed = true
		}
	}
	return failed
}

func printWorktrees(ctx context.Context, cfg config, repoName, label string, stdout io.Writer) error {
	dir := filepath.Join(reposDir(cfg), repoName)
	out, err := runGit(ctx, cfg, dir, "worktree", "list")
	if err != nil {
		return fmt.Errorf("listing worktrees for %s: %w", label, err)
	}
	fmt.Fprintf(stdout, "%s:\n", label)
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		fmt.Fprintf(stdout, "  %s\n", line)
	}
	return nil
}
