package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// runWorktree dispatches the "gh org-clone worktree <verb>" subcommands. It
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
	fmt.Fprintln(w, "  gh org-clone worktree add [flags] <org>/<repo> <branch> <path>")
	fmt.Fprintln(w, "  gh org-clone worktree remove [--force] [flags] <org>/<repo> <path>")
	fmt.Fprintln(w, "  gh org-clone worktree list [flags] <org>/<repo>|<org>")
}

// newWorktreeFlagSet makes a subcommand's flag set whose -h/--help output
// is the subcommand's own usage line followed by its flags.
func newWorktreeFlagSet(name, usage string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "USAGE")
		fmt.Fprintln(stderr, "  "+usage)
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "FLAGS")
		printFlagTable(stderr, flagHelpsFromSet(fs, defaultConfig()))
	}
	return fs
}

// resolveWorktreeConfig applies the same root/protocol/timeout/config
// precedence (flags > env > file > defaults) as the sync command, but only
// for the flags worktree subcommands need; cfg.Org is left unset for the
// caller to fill in once it has parsed <org>/<repo> out of the positional
// args.
func resolveWorktreeConfig(fs *flag.FlagSet, args []string) (config, []string, error) {
	var root, protocol, timeoutStr, configPath string
	fs.StringVar(&root, "root", "", "root directory for cloned orgs")
	fs.StringVar(&protocol, "protocol", "", "clone protocol: ssh or https")
	fs.StringVar(&timeoutStr, "timeout", "", "per-subprocess timeout")
	fs.StringVar(&configPath, "config", "", "path to a JSON config file")

	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return config{}, nil, err
	}

	cfg := defaultConfig()

	fc, err := loadFileConfig(resolveConfigPath(configPath))
	if err != nil {
		return config{}, nil, err
	}
	if fc != nil {
		if fc.Root != nil {
			cfg.Root = *fc.Root
		}
		if fc.Timeout != nil {
			d, err := time.ParseDuration(*fc.Timeout)
			if err != nil {
				return config{}, nil, fmt.Errorf("config file: invalid timeout %q: %w", *fc.Timeout, err)
			}
			cfg.Timeout = d
		}
		if fc.Protocol != nil {
			cfg.Protocol = *fc.Protocol
		}
	}

	if err := overlayEnv(&cfg, "GH_ORG_CLONE_ROOT", "GH_ORG_CLONE_PROTOCOL", "GH_ORG_CLONE_TIMEOUT"); err != nil {
		return config{}, nil, err
	}

	var flagErr error
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "root":
			cfg.Root = root
		case "protocol":
			cfg.Protocol = protocol
		case "timeout":
			d, err := time.ParseDuration(timeoutStr)
			if err != nil {
				flagErr = fmt.Errorf("--timeout: invalid duration %q: %w", timeoutStr, err)
				return
			}
			cfg.Timeout = d
		}
	})
	if flagErr != nil {
		return config{}, nil, flagErr
	}

	if !filepath.IsAbs(cfg.Root) {
		return config{}, nil, fmt.Errorf("root must be an absolute path, got %q", cfg.Root)
	}
	if cfg.Protocol != "ssh" && cfg.Protocol != "https" {
		return config{}, nil, fmt.Errorf("protocol must be ssh or https, got %q", cfg.Protocol)
	}
	if cfg.Timeout <= 0 {
		return config{}, nil, fmt.Errorf("timeout must be > 0, got %s", cfg.Timeout)
	}

	return cfg, positional, nil
}

// parseOrgRepo splits "<org>/<repo>" and validates both halves against the
// same rules the sync command already trusts: orgNamePattern for the org,
// validRepoName for the repo (it becomes a path segment and a git argument).
func parseOrgRepo(s string) (org, repo string, err error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected <org>/<repo>, got %q", s)
	}
	org, repo = parts[0], parts[1]
	if !orgNamePattern.MatchString(org) {
		return "", "", fmt.Errorf("org %q is not a valid GitHub org name", org)
	}
	if !validRepoName(repo) {
		return "", "", fmt.Errorf("repo %q is not a valid repo name", repo)
	}
	return org, repo, nil
}

func cmdWorktreeAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "gh org-clone worktree add [flags] <org>/<repo> <branch> <path>"
	fs := newWorktreeFlagSet("gh org-clone worktree add", usage, stderr)
	cfg, rest, err := resolveWorktreeConfig(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return exitSuccess
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(rest) != 3 {
		fmt.Fprintln(stderr, "usage: "+usage)
		return exitUsage
	}
	org, repoName, err := parseOrgRepo(rest[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	branch := rest[1]
	path, err := absWorktreePath(rest[2])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	cfg.Org = org

	if _, err := exec.LookPath("gh"); err != nil {
		fmt.Fprintln(stderr, "gh-org-clone requires the gh CLI on PATH:", err)
		return exitRuntimeFail
	}
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(stderr, "gh-org-clone requires git on PATH:", err)
		return exitRuntimeFail
	}

	gctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	repo, err := getRepo(gctx, cfg, org+"/"+repoName)
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
// org lock.
func ensureClonedForWorktree(ctx context.Context, cfg config, repo ghRepo, stderr io.Writer) error {
	if err := cloneRepo(ctx, cfg, repo); err != nil {
		return err
	}

	st := loadState(statePath(cfg), cfg.Org, stderr)
	st.Repos[repo.Name] = repoState{
		ID:       repo.ID,
		PushedAt: repo.PushedAt,
		SyncedAt: time.Now(),
		Status:   statusCloned,
	}
	st.UpdatedAt = time.Now()
	return saveState(statePath(cfg), st)
}

func cmdWorktreeRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "gh org-clone worktree remove [--force] [flags] <org>/<repo> <path>"
	fs := newWorktreeFlagSet("gh org-clone worktree remove", usage, stderr)
	var force bool
	fs.BoolVar(&force, "force", false, "remove even if the worktree has uncommitted changes")
	cfg, rest, err := resolveWorktreeConfig(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return exitSuccess
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(rest) != 2 {
		fmt.Fprintln(stderr, "usage: "+usage)
		return exitUsage
	}
	org, repoName, err := parseOrgRepo(rest[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	cfg.Org = org
	path, err := absWorktreePath(rest[1])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}

	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(stderr, "gh-org-clone requires git on PATH:", err)
		return exitRuntimeFail
	}

	dir := filepath.Join(reposDir(cfg), repoName)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		fmt.Fprintf(stderr, "no local clone of %s/%s at %s\n", org, repoName, dir)
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
	const usage = "gh org-clone worktree list [flags] <org>/<repo>|<org>"
	fs := newWorktreeFlagSet("gh org-clone worktree list", usage, stderr)
	cfg, rest, err := resolveWorktreeConfig(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return exitSuccess
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: "+usage)
		return exitUsage
	}

	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(stderr, "gh-org-clone requires git on PATH:", err)
		return exitRuntimeFail
	}

	if strings.Contains(rest[0], "/") {
		org, repoName, err := parseOrgRepo(rest[0])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		cfg.Org = org
		dir := filepath.Join(reposDir(cfg), repoName)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			fmt.Fprintf(stderr, "no local clone of %s/%s at %s\n", org, repoName, dir)
			return exitRuntimeFail
		}
		if err := printWorktrees(ctx, cfg, repoName, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return exitRuntimeFail
		}
		return exitSuccess
	}

	if !orgNamePattern.MatchString(rest[0]) {
		fmt.Fprintf(stderr, "org %q is not a valid GitHub org name\n", rest[0])
		return exitUsage
	}
	cfg.Org = rest[0]

	entries, err := os.ReadDir(reposDir(cfg))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}

	failed := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(reposDir(cfg), e.Name(), ".git")); err != nil {
			continue
		}
		if err := printWorktrees(ctx, cfg, e.Name(), stdout); err != nil {
			fmt.Fprintln(stderr, err)
			failed = true
		}
	}
	if failed {
		return exitRuntimeFail
	}
	return exitSuccess
}

func printWorktrees(ctx context.Context, cfg config, repoName string, stdout io.Writer) error {
	dir := filepath.Join(reposDir(cfg), repoName)
	out, err := runGit(ctx, cfg, dir, "worktree", "list")
	if err != nil {
		return fmt.Errorf("listing worktrees for %s: %w", repoName, err)
	}
	fmt.Fprintf(stdout, "%s:\n", repoName)
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		fmt.Fprintf(stdout, "  %s\n", line)
	}
	return nil
}
