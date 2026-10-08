// This file is the "gh org-clone sync" subcommand: the explicit form of
// the bare top-level entry point (main.go's runSingleOwnerSync, which this
// file's single-owner path also calls), plus three things main.go's bare
// form cannot express at all: a bare workspace-wide sync (every configured
// owner, plus every explicitly tracked repo outside them), a single
// "<org>/<repo>" sync, and --tracked-only. See AIDEV.md's "Commands" table
// and "Multi-owner sync".
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/pflag"
	"github.com/swanysimon/gh-org-clone/internal/engine"
	"github.com/swanysimon/gh-org-clone/internal/ghcli"
	"github.com/swanysimon/gh-org-clone/internal/plan"
	"github.com/swanysimon/gh-org-clone/internal/settings"
	"github.com/swanysimon/gh-org-clone/internal/store"
)

// runSync is "gh org-clone sync [flags] [<org>[/<repo>]]"'s dispatch.
// Named runSync, not cmdSync like every other subcommand's dispatch
// function (cmdWorktreeAdd, cmdClone, cmdUntrack), to avoid colliding with
// the pre-existing cmdSync commandID constant -- see settings.go's doc
// comment on cmdIDClone for the same kind of collision, resolved the
// other way there.
func runSync(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "gh org-clone sync [flags] [<org>[/<repo>]]"
	fs, help := newSubcommandFlagSet("gh org-clone sync", usage, stderr)
	cfg, fc, positional, err := resolveSyncConfig(fs, help, args)
	if errors.Is(err, errHelpRequested) {
		return exitSuccess
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(positional) > 1 {
		fmt.Fprintln(stderr, "usage: "+usage)
		return exitUsage
	}

	if len(positional) == 1 {
		if owner, repo, ok := strings.Cut(positional[0], "/"); ok {
			if cfg.TrackedOnly {
				fmt.Fprintln(stderr, "--tracked-only has no effect with a single <org>/<repo> argument; ignoring it")
			}
			return runSingleRepoSync(ctx, cfg, owner, repo, stdout, stderr)
		}
		cfg.Owner = positional[0]
		if err := validateConfig(cfg); err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		if cfg.TrackedOnly {
			return runWorkspaceSync(ctx, cfg, fc, []string{cfg.Owner}, stdout, stderr)
		}
		return runSingleOwnerSync(ctx, cfg, stdout, stderr)
	}

	return runWorkspaceSync(ctx, cfg, fc, nil, stdout, stderr)
}

// resolveSyncConfig is resolveConfig/resolveSubcommandConfig's third
// sibling: like resolveSubcommandConfig, it accepts any number of
// positional arguments (runSync itself checks the count, since 0 and 1 are
// both valid and mean different things), but unlike either of them, its
// caller also needs the raw *fileConfig (for Owners/Repos), not just the
// resolved Settings.
func resolveSyncConfig(fs *pflag.FlagSet, help *bool, args []string) (cfg config, fc *fileConfig, positional []string, err error) {
	var configPath string
	bound := bindSettings(fs, cmdSync)
	fs.StringVar(&configPath, "config", "", "path to a JSON config file")

	if err := fs.Parse(args); err != nil {
		fs.Usage()
		return config{}, nil, nil, err
	}
	if *help {
		fs.Usage()
		return config{}, nil, nil, errHelpRequested
	}

	cfg = defaultConfig()

	fc, err = loadFileConfig(resolveConfigPath(configPath))
	if err != nil {
		return config{}, nil, nil, err
	}
	if err := resolveSettings(&cfg, cmdSync, fs, bound, fc); err != nil {
		return config{}, nil, nil, err
	}
	if err := expandConfigPaths(&cfg); err != nil {
		return config{}, nil, nil, err
	}
	if err := settings.Validate(cfg.Settings); err != nil {
		return config{}, nil, nil, err
	}

	return cfg, fc, fs.Args(), nil
}

// runSingleRepoSync is "sync <org>/<repo>": the same per-repo decision a
// full sync would make for this one repo (engine.SyncOne), but explicit=
// false -- this is an ad hoc refresh, not a track action. Only clone and
// worktree add mark a repo explicitly tracked.
func runSingleRepoSync(ctx context.Context, cfg config, owner, repoName string, stdout, stderr io.Writer) int {
	if err := validateOwnerRepoArgs(owner, repoName); err != nil {
		fmt.Fprintln(stderr, err)
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

	cfg.Owner = owner
	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}
	if err := os.MkdirAll(archivesDir(cfg), 0o700); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}
	release, err := acquireLock(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}
	defer release()

	repo, err := getRepo(ctx, cfg, owner+"/"+repoName)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}

	res, err := engine.SyncOne(ctx, buildEnv(cfg), repo, false, stderr)
	for _, n := range res.Notes {
		fmt.Fprintf(stderr, "warning: %s: %s\n", res.Name, n)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}
	fmt.Fprintf(stdout, "%s/%s: %s\n", owner, repoName, res.Action)
	return exitSuccess
}

// validateOwnerRepoArgs reuses parseOwnerRepo's validation without its
// splitting, since runSingleRepoSync already has owner/repoName apart via
// strings.Cut (which, unlike parseOwnerRepo's SplitN(s, "/", 2), is what
// let runSync tell "<org>" apart from "<org>/<repo>" in the first place).
func validateOwnerRepoArgs(owner, repoName string) error {
	_, _, err := parseOwnerRepo(owner + "/" + repoName)
	return err
}

// discoverExplicitOwners finds every owner directory under cfg.Root that
// has its own state.json -- the "no workspace-level index... enumerate
// <root>/*/state.json" rule from AIDEV.md's "Discovering tracked repos."
// A root that doesn't exist yet (nothing has ever been synced) is not an
// error; it just contributes no owners.
func discoverExplicitOwners(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var owners []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(store.StatePath(root, e.Name())); err == nil {
			owners = append(owners, e.Name())
		}
	}
	return owners
}

// ownerWorkPlan is one owner's planning result for a workspace-wide (or
// tracked-only) sync, before any lock is taken or state is written -- so
// the same planning step serves both a dry run (which never locks or
// writes) and a real one.
type ownerWorkPlan struct {
	owner             string
	env               engine.Env
	state             store.State
	tasks             []engine.Task
	explicitRemaining []string // repo names still needing a batched lookup
	planErr           error    // a listing failure; this owner is reported and skipped entirely
}

// runWorkspaceSync is bare "sync" (restrictOwners == nil: every configured
// owner, plus every explicitly tracked repo outside them) and
// --tracked-only (restrictOwners set to exactly one owner by runSync, or
// nil for the bare --tracked-only form: every owner discovered under
// cfg.Root, with no listing for any of them regardless of configuration).
func runWorkspaceSync(ctx context.Context, cfg config, fc *fileConfig, restrictOwners []string, stdout, stderr io.Writer) int {
	if _, err := exec.LookPath("gh"); err != nil {
		fmt.Fprintln(stderr, "gh-org-clone requires the gh CLI on PATH:", err)
		return exitRuntimeFail
	}
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(stderr, "gh-org-clone requires git on PATH:", err)
		return exitRuntimeFail
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	configuredOwners := map[string]settings.OwnerConfig{}
	if fc != nil {
		for _, oc := range fc.Owners {
			configuredOwners[oc.Name] = oc
		}
	}
	explicitByOwner := map[string]map[string]bool{}
	addExplicit := func(owner, repo string) {
		if explicitByOwner[owner] == nil {
			explicitByOwner[owner] = map[string]bool{}
		}
		explicitByOwner[owner][repo] = true
	}
	if fc != nil {
		for _, r := range fc.Repos {
			if owner, repo, ok := strings.Cut(r, "/"); ok {
				addExplicit(owner, repo)
			}
		}
	}
	for _, owner := range discoverExplicitOwners(cfg.Root) {
		st := loadState(store.StatePath(cfg.Root, owner), owner, stderr)
		for name, rs := range st.Repos {
			if rs.Tracked {
				addExplicit(owner, name)
			}
		}
	}

	ownerSet := map[string]bool{}
	for name := range configuredOwners {
		ownerSet[name] = true
	}
	for name := range explicitByOwner {
		ownerSet[name] = true
	}
	if restrictOwners != nil {
		restricted := map[string]bool{}
		for _, o := range restrictOwners {
			restricted[o] = true
		}
		for name := range ownerSet {
			if !restricted[name] {
				delete(ownerSet, name)
			}
		}
		// --tracked-only with a single explicit <org> argument must still
		// run even if that owner has never been seen before (nothing
		// local, nothing configured) -- there's simply nothing to do, and
		// that is reported, not an error.
		for _, o := range restrictOwners {
			ownerSet[o] = true
		}
	}

	owners := make([]string, 0, len(ownerSet))
	for name := range ownerSet {
		owners = append(owners, name)
	}
	sort.Strings(owners) // deterministic ordering for output and tests

	trackedOnly := cfg.TrackedOnly
	plans := make([]ownerWorkPlan, 0, len(owners))
	type batchEntry struct {
		planIdx int
		name    string
	}
	var batch []batchEntry
	var batchPairs []ghcli.OwnerRepo

	for _, owner := range owners {
		ownerSettings := cfg.Settings
		if oc, ok := configuredOwners[owner]; ok {
			ownerSettings = settings.ApplyOwnerOverrides(cfg.Settings, oc)
		}
		ownerCfg := cfg
		ownerCfg.Owner = owner
		ownerCfg.Settings = ownerSettings
		env := buildEnv(ownerCfg)
		st := loadState(store.StatePath(cfg.Root, owner), owner, stderr)

		plan := ownerWorkPlan{owner: owner, env: env, state: st}

		remaining := map[string]bool{}
		for name := range explicitByOwner[owner] {
			remaining[name] = true
		}

		_, configured := configuredOwners[owner]
		if configured && !trackedOnly {
			repos, err := ghcli.ListRepos(ctx, cfg.Deps.exec, owner, ownerSettings.MaxRepos)
			if err != nil {
				plan.planErr = err
			} else {
				if len(repos) >= ownerSettings.MaxRepos {
					fmt.Fprintf(stderr, "warning: %s: gh repo list returned %d repos, the --max-repos limit; the listing may be truncated, raise --max-repos to be sure\n", owner, len(repos))
				}
				tasks, seen, prepassFailed := engine.BuildTasks(ctx, env, repos, st, stderr)
				plan.tasks = tasks
				if prepassFailed > 0 {
					fmt.Fprintf(stderr, "error: %s: %d repo(s) could not be planned\n", owner, prepassFailed)
				}
				for name := range remaining {
					if seen[name] {
						delete(remaining, name)
					}
				}
				// Mirrors runSingleOwnerSync's own "in state but not in
				// listing" note -- but it must not fire for a name still
				// in remaining: that name is explicitly tracked and the
				// owner's filter (e.g. a fork with includeForks=false)
				// excluded it from the listing, so it is about to get a
				// real decision of its own via BuildExplicitTasks, not
				// just a note. See AIDEV.md's invariant about this.
				for name := range st.Repos {
					if !seen[name] && !remaining[name] {
						fmt.Fprintf(stderr, "note: %s/%s is in local state but was not returned by gh repo list; leaving any local data untouched\n", owner, name)
					}
				}
			}
		}

		for name := range remaining {
			batch = append(batch, batchEntry{planIdx: len(plans), name: name})
			batchPairs = append(batchPairs, ghcli.OwnerRepo{Owner: owner, Name: name})
		}
		plans = append(plans, plan)
	}

	if len(batchPairs) > 0 {
		results, err := ghcli.ViewRepos(ctx, cfg.Deps.exec, batchPairs)
		if err != nil {
			fmt.Fprintln(stderr, "error: batch repo lookup failed:", err)
			// Every plan that contributed any entry to this batch must be
			// marked failed, regardless of whether it also has
			// listing-derived tasks -- a batch failure must never result
			// in exitSuccess with that owner's explicitly tracked repos
			// silently dropped from this run. Keying off batch (not
			// explicitByOwner) ties this exactly to what was actually
			// requested, not what could have been requested.
			affected := map[int]bool{}
			for _, be := range batch {
				affected[be.planIdx] = true
			}
			for idx := range affected {
				if plans[idx].planErr == nil {
					plans[idx].planErr = err
				}
			}
		} else {
			resultsByPlan := map[int][]ghcli.RepoResult{}
			for i, be := range batch {
				resultsByPlan[be.planIdx] = append(resultsByPlan[be.planIdx], results[i])
			}
			for idx, rs := range resultsByPlan {
				extra := engine.BuildExplicitTasks(plans[idx].env, rs, plans[idx].state)
				plans[idx].tasks = append(plans[idx].tasks, extra...)
			}
		}
	}

	if cfg.DryRun {
		return reportWorkspaceDryRun(plans, stdout, stderr)
	}
	return runWorkspacePlans(ctx, cfg, plans, stdout, stderr)
}

// reportWorkspaceDryRun prints every owner's planned tasks without doing
// anything -- no directories, no lock files, no state writes, matching the
// single-owner dry run's own "only reads" rule.
func reportWorkspaceDryRun(plans []ownerWorkPlan, stdout, stderr io.Writer) int {
	failed := 0
	for _, p := range plans {
		if p.planErr != nil {
			fmt.Fprintf(stderr, "error: %s: %v\n", p.owner, p.planErr)
			failed++
			continue
		}
		for _, t := range p.tasks {
			fmt.Fprintf(stdout, "%s/%s: %s (%s)\n", p.owner, t.Repo.Name, t.Action, t.Reason)
		}
	}
	if failed > 0 {
		return exitRuntimeFail
	}
	return exitSuccess
}

// runWorkspacePlans acquires each owner's lock (reporting and skipping an
// owner whose lock is already held, exactly like a locked single owner
// sync would, but never aborting the other owners), prepares the
// directories a sync needs, sweeps leftover temp clones, then runs every
// successfully-locked owner's tasks through one shared engine.MultiOwnerRun
// pool. See AIDEV.md "Multi-owner sync": lock order never matters because
// nothing ever waits on a lock.
func runWorkspacePlans(ctx context.Context, cfg config, plans []ownerWorkPlan, stdout, stderr io.Writer) int {
	var owners []engine.OwnerWork
	failedOwners := 0
	for _, p := range plans {
		if p.planErr != nil {
			fmt.Fprintf(stderr, "error: %s: %v\n", p.owner, p.planErr)
			failedOwners++
			continue
		}

		ownerCfg := cfg
		ownerCfg.Owner = p.owner
		ownerCfg.Settings = p.env.Settings
		if err := os.MkdirAll(reposDir(ownerCfg), 0o700); err != nil {
			fmt.Fprintf(stderr, "error: %s: %v\n", p.owner, err)
			failedOwners++
			continue
		}
		if err := os.MkdirAll(archivesDir(ownerCfg), 0o700); err != nil {
			fmt.Fprintf(stderr, "error: %s: %v\n", p.owner, err)
			failedOwners++
			continue
		}
		release, err := acquireLock(ownerCfg)
		if err != nil {
			fmt.Fprintf(stderr, "error: %s: %v (skipping)\n", p.owner, err)
			failedOwners++
			continue
		}
		engine.SweepTempClones(p.env)

		owners = append(owners, engine.OwnerWork{
			Owner:   p.owner,
			Env:     p.env,
			Tasks:   p.tasks,
			State:   p.state,
			Release: release,
		})
	}

	fmt.Fprintf(stderr, "syncing %d owner(s)\n", len(owners))
	outcomes := engine.MultiOwnerRun(ctx, cfg.Settings.Concurrency, owners, stderr)

	var cloned, fetched, archived, skipped, failed int
	failed += failedOwners
	for _, oc := range outcomes {
		for _, res := range oc.Results {
			for _, n := range res.Notes {
				fmt.Fprintf(stderr, "warning: %s/%s: %s\n", oc.Owner, res.Name, n)
			}
			if res.Err != nil {
				fmt.Fprintf(stderr, "error: %s/%s: %v\n", oc.Owner, res.Name, res.Err)
				failed++
				continue
			}
			switch res.Action {
			case plan.Skip, plan.MissingUpstream, plan.NotARepo:
				skipped++
			case plan.Clone, plan.Unarchive:
				cloned++
			case plan.Fetch:
				fetched++
			case plan.Archive, plan.AdoptArchived:
				archived++
			}
		}
		if oc.SaveErr != nil {
			fmt.Fprintf(stderr, "error: %s: saving state: %v\n", oc.Owner, oc.SaveErr)
			failed++
		}
	}

	fmt.Fprintf(stdout, "owners=%d cloned=%d fetched=%d archived=%d skipped=%d failed=%d\n", len(owners), cloned, fetched, archived, skipped, failed)

	if ctx.Err() != nil {
		return exitInterrupted
	}
	if failed > 0 {
		return exitRuntimeFail
	}
	return exitSuccess
}
