package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/swanysimon/gh-org-clone/internal/engine"
)

func printCloneUsage(w io.Writer) {
	fmt.Fprintln(w, "USAGE")
	fmt.Fprintln(w, "  gh org-clone clone [flags] <org>/<repo>...")
}

func printUntrackUsage(w io.Writer) {
	fmt.Fprintln(w, "USAGE")
	fmt.Fprintln(w, "  gh org-clone untrack [flags] <org>/<repo>")
}

// cmdClone implements `clone <org>/<repo>...`: an explicit, single-repo
// counterpart to a full sync, for a repo outside any owner you sync
// wholesale. Each argument is resolved and synced independently via
// engine.SyncOne (clone if missing, fetch if changed, skip if unchanged,
// or archive/adopt an archive exactly as a full sync would) -- the same
// per-repo continue-past-failure philosophy a full sync uses, so one bad
// argument doesn't stop the rest from being processed. Every repo named
// here is recorded Tracked: true.
func cmdClone(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "gh org-clone clone [flags] <org>/<repo>..."
	fs, help := newSubcommandFlagSet("gh org-clone clone", usage, stderr)
	cfg, rest, err := resolveSubcommandConfig(fs, help, cmdIDClone, args)
	if errors.Is(err, errHelpRequested) {
		return exitSuccess
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "usage: "+usage)
		return exitUsage
	}

	if _, err := exec.LookPath("gh"); err != nil {
		fmt.Fprintln(stderr, "gh-org-clone requires the gh CLI on PATH:", err)
		return exitRuntimeFail
	}
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(stderr, "gh-org-clone requires git on PATH:", err)
		return exitRuntimeFail
	}

	failed := 0
	for _, arg := range rest {
		owner, repoName, err := parseOwnerRepo(arg)
		if err != nil {
			fmt.Fprintln(stderr, err)
			failed++
			continue
		}
		if err := cloneOne(ctx, cfg, owner, repoName, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "error: %s/%s: %v\n", owner, repoName, err)
			failed++
		}
	}
	if failed > 0 {
		return exitRuntimeFail
	}
	return exitSuccess
}

// cloneOne resolves and syncs one repo, taking owner's lock for the
// duration (planning through state save), same as a full sync and
// worktree add both do.
func cloneOne(ctx context.Context, cfg config, owner, repoName string, stdout, stderr io.Writer) error {
	cfg.Owner = owner

	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(archivesDir(cfg), 0o700); err != nil {
		return err
	}

	release, err := acquireLock(cfg)
	if err != nil {
		return err
	}
	defer release()

	gctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	repo, err := getRepo(gctx, cfg, owner+"/"+repoName)
	cancel()
	if err != nil {
		return err
	}

	res, err := engine.SyncOne(ctx, buildEnv(cfg), repo, true, stderr)
	if err != nil {
		return err
	}
	for _, note := range res.Notes {
		fmt.Fprintf(stderr, "warning: %s/%s: %s\n", owner, repoName, note)
	}
	fmt.Fprintf(stdout, "%s/%s: %s\n", owner, repoName, res.Action)
	return nil
}

// cmdUntrack implements `untrack <org>/<repo>`: clears a repo's explicit
// Tracked bit without touching any local data. It is the only command in
// this tool that can make a repo *less* tracked, and it still never
// deletes anything -- the clone or archive, if either exists, is left
// exactly where it is.
func cmdUntrack(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "gh org-clone untrack [flags] <org>/<repo>"
	fs, help := newSubcommandFlagSet("gh org-clone untrack", usage, stderr)
	cfg, rest, err := resolveSubcommandConfig(fs, help, cmdWorktree, args)
	if errors.Is(err, errHelpRequested) {
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
	owner, repoName, err := parseOwnerRepo(rest[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	cfg.Owner = owner

	release, err := acquireLock(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}
	defer release()

	st := loadState(statePath(cfg), owner, stderr)
	rs, ok := st.Repos[repoName]
	if !ok {
		fmt.Fprintf(stderr, "no local data for %s/%s; nothing to untrack\n", owner, repoName)
		return exitRuntimeFail
	}
	if !rs.Tracked {
		fmt.Fprintf(stdout, "%s/%s is not explicitly tracked; nothing to do\n", owner, repoName)
		return exitSuccess
	}

	// AIDEV: once the config's owners/repos lists exist (the next Phase 3
	// step), untrack must fail here instead -- naming the config entry to
	// edit -- if the config still tracks this repo, rather than silently
	// clearing a bit the next sync would just set again. The config shape
	// doesn't exist yet, so that check isn't implemented yet either.
	rs.Tracked = false
	st.Repos[repoName] = rs
	st.UpdatedAt = time.Now()
	if err := saveState(statePath(cfg), st); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}

	fmt.Fprintf(stdout, "untracked %s/%s; any local clone or archive is left in place\n", owner, repoName)
	return exitSuccess
}
