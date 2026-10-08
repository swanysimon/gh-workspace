// Package engine is the orchestration layer: given a resolved list of repos
// and the previous state, it decides what to do with each one (via plan),
// fans the work out to a worker pool, and reports what happened — plus the
// single-repo clone path shared by a full sync, worktree add, and (in a
// later phase) a standalone clone command. It calls into ghcli, gitcli,
// store, plan and archive directly; nothing above it in the dependency
// graph needs to.
package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swanysimon/gh-org-clone/internal/archive"
	"github.com/swanysimon/gh-org-clone/internal/execx"
	"github.com/swanysimon/gh-org-clone/internal/ghcli"
	"github.com/swanysimon/gh-org-clone/internal/gitcli"
	"github.com/swanysimon/gh-org-clone/internal/plan"
	"github.com/swanysimon/gh-org-clone/internal/settings"
	"github.com/swanysimon/gh-org-clone/internal/store"
)

// Env is everything engine needs that isn't specific to one call: the
// subprocess/confirm seams, which owner's data this is, and that owner's
// resolved settings (concurrency, timeout, protocol, root, ...).
type Env struct {
	Exec     execx.Exec
	Confirm  archive.ConfirmFunc
	Owner    string
	Settings settings.Settings
}

func (e Env) ReposDir() string    { return store.ReposDir(e.Settings.Root, e.Owner) }
func (e Env) ArchivesDir() string { return store.ArchivesDir(e.Settings.Root, e.Owner) }
func (e Env) StatePath() string   { return store.StatePath(e.Settings.Root, e.Owner) }

// SweepTempClones removes leftover .tmp-* directories from a previous
// interrupted run. These are ours by construction and can never contain
// user data.
func SweepTempClones(env Env) {
	entries, err := os.ReadDir(env.ReposDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			os.RemoveAll(filepath.Join(env.ReposDir(), e.Name()))
		}
	}
}

// CloneInto clones repo into its conventional place under env.ReposDir(),
// via a temp sibling directory so an interrupted clone can never be
// mistaken for a complete one. This is the single-repo clone path shared by
// a full sync (via BuildTasks/RunTasks), worktree add (via EnsureCloned),
// and archiving a repo that isn't locally cloned yet.
func CloneInto(ctx context.Context, env Env, repo ghcli.Repo) error {
	url := ghcli.CloneURL(repo, env.Settings.Protocol)
	if url == "" {
		return fmt.Errorf("repo %q has no clone URL for protocol %q", repo.Name, env.Settings.Protocol)
	}

	dest := filepath.Join(env.ReposDir(), repo.Name)
	tmp := filepath.Join(env.ReposDir(), ".tmp-"+repo.Name+"-"+strconv.Itoa(os.Getpid()))

	return gitcli.Clone(ctx, env.Exec, env.Settings.Timeout, url, dest, tmp)
}

// EnsureCloned clones repo if needed and writes the same state.json entry a
// sync run would, so a later sync doesn't find a directory it doesn't
// remember creating. It is worktree add's single-repo clone path
// specifically: always clones unconditionally (worktree add needs a live
// checkout and makes its own archived-repo decision afterward), unlike
// SyncOne below, which is the clone command's path and makes the same
// skip/fetch/archive decision a full sync would. The resulting RepoState
// is marked Tracked: true -- this is, by definition, an explicit add. The
// caller must hold the owner's lock.
func EnsureCloned(ctx context.Context, env Env, repo ghcli.Repo, stderr io.Writer) error {
	if err := CloneInto(ctx, env, repo); err != nil {
		return err
	}

	st := store.LoadState(env.StatePath(), env.Owner, stderr)
	st.Repos[repo.Name] = store.RepoState{
		ID:       repo.ID,
		PushedAt: repo.PushedAt,
		SyncedAt: time.Now(),
		Status:   store.StatusCloned,
		Tracked:  true,
	}
	st.UpdatedAt = time.Now()
	return store.SaveState(env.StatePath(), st)
}

// SyncOne resolves one repo's action via the same decision plan.Decide
// would make inside a full owner sync (skip if unchanged, fetch if
// changed, archive if archived upstream and env.Settings.Archive is set,
// adopt an existing local archive, or clone if nothing exists locally yet),
// runs it, and records the result in state.json -- the single-repo
// counterpart to BuildTasks+RunTasks, for callers (the clone command) that
// resolved one repo via gh instead of a full owner listing. Unlike
// EnsureCloned, this can tarball-and-delete an archived-upstream repo
// instead of always cloning it, matching what a full sync would have done
// to the same repo -- which is why worktree add does not use this: it
// always wants a live checkout, and makes its own decision about an
// archived repo afterward (see cmdWorktreeAdd).
//
// explicit marks the resulting RepoState.Tracked, ORed with whatever the
// repo's Tracked bit already was (tracking is sticky: once true, only
// Untrack clears it). Every caller of SyncOne today passes explicit=true,
// since every current caller is an explicit add; the parameter exists
// rather than a hardcoded true so a later single-owner refresh path could
// reuse this with explicit=false. On failure, no state is written -- the
// repo keeps whatever was there before, so it's retried next time,
// matching how a full sync's per-repo failure handling works. The caller
// must hold the owner's lock.
func SyncOne(ctx context.Context, env Env, repo ghcli.Repo, explicit bool, stderr io.Writer) (Result, error) {
	st := store.LoadState(env.StatePath(), env.Owner, stderr)
	prev, known := st.Repos[repo.Name]

	dir := filepath.Join(env.ReposDir(), repo.Name)
	_, dirErr := os.Stat(dir)
	dirExists := dirErr == nil
	_, gitErr := os.Stat(filepath.Join(dir, ".git"))
	isGitDir := gitErr == nil
	archiveExists := archive.LocalArchiveExists(env.ArchivesDir(), repo.Name)

	act, reason := plan.Decide(
		plan.RepoFacts{Archived: repo.IsArchived, PushedAt: repo.PushedAt},
		decidePrevState(prev, known),
		dirExists, isGitDir, archiveExists,
		plan.Options{Force: env.Settings.Force, Archive: env.Settings.Archive},
	)

	res := processTask(ctx, env, Task{Repo: repo, Action: act, Reason: reason, Prev: prev}, nil)
	if res.Err != nil {
		return res, res.Err
	}

	id := repo.ID
	if id == "" {
		id = prev.ID
	}
	st.Repos[repo.Name] = store.RepoState{
		ID:          id,
		PushedAt:    res.PushedAt,
		SyncedAt:    time.Now(),
		Status:      res.Status,
		ArchivePath: res.ArchivePath,
		Tracked:     explicit || prev.Tracked,
	}
	st.UpdatedAt = time.Now()
	if err := store.SaveState(env.StatePath(), st); err != nil {
		return res, err
	}
	return res, nil
}

// Task is one repo's planned action, decided by BuildTasks.
type Task struct {
	Repo   ghcli.Repo
	Action plan.Action
	Reason string
	Prev   store.RepoState
}

// decidePrevState maps a store.RepoState's Status into plan.PrevState's two
// independent bools -- the same mapping gh-org-clone's old root-level
// decide() shim did, now shared by BuildTasks (a full owner sync) and
// SyncOne (a single resolved repo).
func decidePrevState(prev store.RepoState, known bool) plan.PrevState {
	return plan.PrevState{
		Known:    known,
		Archived: known && prev.Status == store.StatusArchived,
		Cloned:   known && prev.Status == store.StatusCloned,
		PushedAt: prev.PushedAt,
	}
}

// BuildTasks is the sequential pre-pass: fork filtering, name validation,
// case-collision detection, ID-based rename fix-up, then plan.Decide per
// repo. It runs single-threaded, before any worker goroutine starts. failed
// counts repos the pre-pass refused (invalid or colliding names); they are
// reported here and must fail the run, since they will never be synced.
func BuildTasks(ctx context.Context, env Env, repos []ghcli.Repo, st store.State, stderr io.Writer) (tasks []Task, seen map[string]bool, failed int) {
	seen = make(map[string]bool, len(repos))

	stateNameByID := map[string]string{}
	for name, rs := range st.Repos {
		if rs.ID != "" {
			stateNameByID[rs.ID] = name
		}
	}

	// APFS is case-insensitive: an owner holding both Foo and foo maps to
	// one directory. Skip both sides rather than interleaving two repos
	// into it. Only repos that would otherwise be synced count: an
	// excluded fork or an invalid name never gets a directory to collide
	// over.
	lowerNames := map[string][]string{}
	for _, r := range repos {
		if (r.IsFork && !env.Settings.IncludeForks) || !store.ValidRepoName(r.Name) {
			continue
		}
		lower := strings.ToLower(r.Name)
		lowerNames[lower] = append(lowerNames[lower], r.Name)
	}
	collided := map[string]bool{}
	for lower, names := range lowerNames {
		if len(names) > 1 {
			fmt.Fprintf(stderr, "error: repos %v collide on a case-insensitive filesystem (%q); skipping all of them\n", names, lower)
			for _, n := range names {
				collided[n] = true
			}
			failed += len(names)
		}
	}

	for _, repo := range repos {
		if repo.IsFork && !env.Settings.IncludeForks {
			if env.Settings.Verbose {
				fmt.Fprintf(stderr, "skipping fork %q\n", repo.Name)
			}
			continue
		}
		if !store.ValidRepoName(repo.Name) {
			fmt.Fprintf(stderr, "error: %q is not a valid repo name, skipping\n", repo.Name)
			failed++
			continue
		}
		if collided[repo.Name] {
			continue
		}
		seen[repo.Name] = true

		// In a dry run a pending rename is planned, not performed: the
		// decision is made against the old directory and state entry, as
		// if the rename had already happened.
		renamedFrom := ""
		if oldName, ok := stateNameByID[repo.ID]; ok && oldName != repo.Name {
			if env.Settings.DryRun {
				if renameApplies(env, oldName, repo.Name) {
					renamedFrom = oldName
				}
			} else {
				fixupRename(ctx, env, st, oldName, repo, stderr)
			}
		}

		prev, known := st.Repos[repo.Name]
		dir := filepath.Join(env.ReposDir(), repo.Name)
		if renamedFrom != "" {
			seen[renamedFrom] = true
			prev, known = st.Repos[renamedFrom]
			dir = filepath.Join(env.ReposDir(), renamedFrom)
		}
		_, dirErr := os.Stat(dir)
		dirExists := dirErr == nil
		_, gitErr := os.Stat(filepath.Join(dir, ".git"))
		isGitDir := gitErr == nil
		archiveExists := archive.LocalArchiveExists(env.ArchivesDir(), repo.Name)

		act, reason := plan.Decide(
			plan.RepoFacts{Archived: repo.IsArchived, PushedAt: repo.PushedAt},
			decidePrevState(prev, known),
			dirExists, isGitDir, archiveExists,
			plan.Options{Force: env.Settings.Force, Archive: env.Settings.Archive},
		)
		if renamedFrom != "" {
			reason = fmt.Sprintf("rename from %q, then: %s", renamedFrom, reason)
		}
		tasks = append(tasks, Task{Repo: repo, Action: act, Reason: reason, Prev: prev})
	}
	return tasks, seen, failed
}

// BuildExplicitTasks is BuildTasks' counterpart for repos resolved via a
// batched ghcli.ViewRepos lookup instead of a full owner listing -- the
// "explicit repos outside configured owners" half of a workspace-wide
// sync, and the whole of sync --tracked-only. results must already be
// filtered to this one owner (ViewRepos itself freely batches across
// owners; grouping its results back by owner is the caller's job -- see
// AIDEV.md "Multi-owner sync").
//
// A nil Repo (upstream reports the repo missing: gone, renamed without a
// redirect, or no longer visible) produces a MissingUpstream task rather
// than being decided by plan.Decide, which has no RepoFacts for a repo gh
// can't resolve at all. A repo whose returned NameWithOwner's owner half
// doesn't match the owner it was requested under (a transfer) is still
// decided normally -- data is never moved between owner directories
// automatically -- but the reason string notes the transfer so a human
// sees it.
func BuildExplicitTasks(env Env, results []ghcli.RepoResult, st store.State) []Task {
	tasks := make([]Task, 0, len(results))
	for _, r := range results {
		prev, known := st.Repos[r.Name]

		if r.Repo == nil {
			tasks = append(tasks, Task{
				Repo:   ghcli.Repo{Name: r.Name},
				Action: plan.MissingUpstream,
				Reason: "explicitly tracked repo no longer resolves upstream (gone, renamed without a redirect, or no longer visible)",
				Prev:   prev,
			})
			continue
		}

		repo := *r.Repo
		dir := filepath.Join(env.ReposDir(), r.Name)
		_, dirErr := os.Stat(dir)
		dirExists := dirErr == nil
		_, gitErr := os.Stat(filepath.Join(dir, ".git"))
		isGitDir := gitErr == nil
		archiveExists := archive.LocalArchiveExists(env.ArchivesDir(), r.Name)

		act, reason := plan.Decide(
			plan.RepoFacts{Archived: repo.IsArchived, PushedAt: repo.PushedAt},
			decidePrevState(prev, known),
			dirExists, isGitDir, archiveExists,
			plan.Options{Force: env.Settings.Force, Archive: env.Settings.Archive},
		)
		if owner, _, ok := strings.Cut(repo.NameWithOwner, "/"); ok && owner != "" && owner != r.Owner {
			reason = fmt.Sprintf("owner on GitHub is now %q (transferred); local data stays under %q: %s", owner, r.Owner, reason)
		}
		tasks = append(tasks, Task{Repo: repo, Action: act, Reason: reason, Prev: prev})
	}
	return tasks
}

// fixupRename moves a renamed repo's directory and state entry without an
// orphaned directory plus a full re-clone.
func fixupRename(ctx context.Context, env Env, st store.State, oldName string, repo ghcli.Repo, stderr io.Writer) {
	if !renameApplies(env, oldName, repo.Name) {
		return
	}
	oldDir := filepath.Join(env.ReposDir(), oldName)
	newDir := filepath.Join(env.ReposDir(), repo.Name)
	if err := os.Rename(oldDir, newDir); err != nil {
		fmt.Fprintf(stderr, "error: renaming %q to %q: %v\n", oldName, repo.Name, err)
		return
	}
	st.Repos[repo.Name] = st.Repos[oldName]
	delete(st.Repos, oldName)
	if url := ghcli.CloneURL(repo, env.Settings.Protocol); url != "" {
		if err := gitcli.SetRemoteURL(ctx, env.Exec, env.Settings.Timeout, newDir, url); err != nil {
			fmt.Fprintf(stderr, "warning: could not update remote url for renamed repo %q: %v\n", repo.Name, err)
		}
	}
}

// renameApplies reports whether a renamed repo's old directory exists and
// its new one does not, i.e. whether fixupRename would move anything.
func renameApplies(env Env, oldName, newName string) bool {
	if _, err := os.Stat(filepath.Join(env.ReposDir(), oldName)); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(env.ReposDir(), newName))
	return os.IsNotExist(err)
}

// OwnerWork is one owner's planned tasks for a multi-owner run (MultiOwnerRun),
// plus enough to apply and save that owner's own results independently of
// every other owner's. The caller builds this after successfully acquiring
// that owner's lock and loading its State -- MultiOwnerRun does not do
// either; it only runs tasks and calls Release once this owner's state has
// been saved.
type OwnerWork struct {
	Owner   string
	Env     Env
	Tasks   []Task
	State   store.State
	Release func()
}

// OwnerOutcome is one owner's results from a MultiOwnerRun call, plus the
// error (if any) saving its state.
type OwnerOutcome struct {
	Owner   string
	Results []Result
	SaveErr error
}

// MultiOwnerRun fans every owner's tasks into one shared pool of poolSize
// workers -- "one worker pool for the whole run," not one pool per owner,
// per AIDEV.md's "Multi-owner sync" -- and, as soon as the last task for a
// given owner completes, applies that owner's results to its already-loaded
// State, saves it, and calls its Release, so an interrupt loses progress
// only for owners still in flight. A single shared progress reporter
// covers every owner's actionable tasks, so a workspace-wide run reports
// activity the same way a single-owner one does, just across more repos.
//
// State mutation here mirrors the single-owner path (Run()'s own result
// loop, and SyncOne): a fresh repo ID (from the task that produced the
// result) wins over whatever was stored, Tracked carries forward from
// whatever was already recorded, and a failed task's repo keeps its
// previous state entry untouched so it's retried next time.
func MultiOwnerRun(ctx context.Context, poolSize int, owners []OwnerWork, stderr io.Writer) []OwnerOutcome {
	type routedTask struct {
		ownerIdx int
		task     Task
	}
	type routedResult struct {
		ownerIdx int
		result   Result
	}

	totalActionable := 0
	remaining := make([]int, len(owners))
	tasksByOwnerName := make([]map[string]Task, len(owners))
	for i, ow := range owners {
		remaining[i] = len(ow.Tasks)
		byName := make(map[string]Task, len(ow.Tasks))
		for _, t := range ow.Tasks {
			byName[t.Repo.Name] = t
			if t.Action != plan.Skip {
				totalActionable++
			}
		}
		tasksByOwnerName[i] = byName
	}
	progress := newProgressReporter(stderr, totalActionable)

	if poolSize < 1 {
		poolSize = 1
	}
	taskCh := make(chan routedTask)
	resultCh := make(chan routedResult)

	var wg sync.WaitGroup
	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rt := range taskCh {
				res := processTask(ctx, owners[rt.ownerIdx].Env, rt.task, progress)
				resultCh <- routedResult{ownerIdx: rt.ownerIdx, result: res}
			}
		}()
	}

	go func() {
		for i, ow := range owners {
			for _, t := range ow.Tasks {
				taskCh <- routedTask{ownerIdx: i, task: t}
			}
		}
		close(taskCh)
	}()

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	outcomes := make([]OwnerOutcome, len(owners))
	for i, ow := range owners {
		outcomes[i].Owner = ow.Owner
	}
	for rr := range resultCh {
		i := rr.ownerIdx
		outcomes[i].Results = append(outcomes[i].Results, rr.result)
		remaining[i]--
		if remaining[i] == 0 {
			applyResultsToState(&owners[i].State, outcomes[i].Results, tasksByOwnerName[i])
			outcomes[i].SaveErr = store.SaveState(owners[i].Env.StatePath(), owners[i].State)
			if owners[i].Release != nil {
				owners[i].Release()
			}
		}
	}
	return outcomes
}

// applyResultsToState is MultiOwnerRun's per-owner state update, factored
// out so it can be unit-tested without a worker pool. A failed result's
// repo is left alone -- whatever was there before stays, so it is retried
// on the next run, exactly like the single-owner path's handling.
func applyResultsToState(st *store.State, results []Result, tasksByName map[string]Task) {
	for _, res := range results {
		if res.Err != nil {
			continue
		}
		id := st.Repos[res.Name].ID
		if t, ok := tasksByName[res.Name]; ok && t.Repo.ID != "" {
			id = t.Repo.ID
		}
		st.Repos[res.Name] = store.RepoState{
			ID:          id,
			PushedAt:    res.PushedAt,
			SyncedAt:    time.Now(),
			Status:      res.Status,
			ArchivePath: res.ArchivePath,
			Tracked:     st.Repos[res.Name].Tracked,
		}
	}
	st.UpdatedAt = time.Now()
}

// Result carries a worker's outcome. Errors travel in this struct, never
// out of a worker, so one repo's failure can never abort another's work.
type Result struct {
	Name        string
	Action      plan.Action
	PushedAt    time.Time
	Status      store.Status
	ArchivePath string
	Notes       []string
	Err         error
}

// progressReporter prints one line per non-skip task as a worker picks it
// up, so a long sync run shows activity instead of going silent until the
// final summary. It is safe for concurrent use by Env.Settings.Concurrency
// workers.
type progressReporter struct {
	mu      sync.Mutex
	w       io.Writer
	total   int
	started int
}

func newProgressReporter(w io.Writer, total int) *progressReporter {
	return &progressReporter{w: w, total: total}
}

var actionVerbs = map[plan.Action]string{
	plan.Clone:           "cloning",
	plan.Fetch:           "fetching",
	plan.Archive:         "archiving",
	plan.AdoptArchived:   "adopting existing archive",
	plan.Unarchive:       "unarchiving (repo live again upstream)",
	plan.NotARepo:        "checking",
	plan.MissingUpstream: "checking",
}

func (p *progressReporter) starting(name string, act plan.Action) {
	if p == nil {
		return
	}
	verb, ok := actionVerbs[act]
	if !ok {
		verb = string(act)
	}
	p.mu.Lock()
	p.started++
	fmt.Fprintf(p.w, "[%d/%d] %s: %s\n", p.started, p.total, name, verb)
	p.mu.Unlock()
}

// RunTasks fans work out to Env.Settings.Concurrency workers and fans
// results back in through a single collector loop. State is mutated only
// by the caller, never here — so there is no mutex and no data race.
func RunTasks(ctx context.Context, env Env, tasks []Task, stderr io.Writer) []Result {
	actionable := 0
	for _, t := range tasks {
		if t.Action != plan.Skip {
			actionable++
		}
	}
	progress := newProgressReporter(stderr, actionable)

	taskCh := make(chan Task)
	resultCh := make(chan Result)

	var wg sync.WaitGroup
	for i := 0; i < env.Settings.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range taskCh {
				resultCh <- processTask(ctx, env, t, progress)
			}
		}()
	}

	go func() {
		for _, t := range tasks {
			taskCh <- t
		}
		close(taskCh)
	}()

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	results := make([]Result, 0, len(tasks))
	for res := range resultCh {
		results = append(results, res)
	}
	return results
}

func processTask(ctx context.Context, env Env, t Task, progress *progressReporter) Result {
	repo := t.Repo
	res := Result{Name: repo.Name, Action: t.Action}

	if t.Action != plan.Skip {
		progress.starting(repo.Name, t.Action)
	}

	switch t.Action {
	case plan.Skip:
		res.PushedAt = t.Prev.PushedAt
		res.Status = t.Prev.Status
		res.ArchivePath = t.Prev.ArchivePath
		return res

	case plan.Clone, plan.Fetch:
		dir := filepath.Join(env.ReposDir(), repo.Name)
		if t.Action == plan.Clone {
			if err := CloneInto(ctx, env, repo); err != nil {
				res.Err = err
				return res
			}
		} else if err := gitcli.Fetch(ctx, env.Exec, env.Settings.Timeout, dir); err != nil {
			res.Err = err
			return res
		}

		defaultBranch := ""
		if repo.DefaultBranch != nil {
			defaultBranch = repo.DefaultBranch.Name
		}
		warn, dirty, err := gitcli.UpdateWorktree(ctx, env.Exec, env.Settings.Timeout, dir, defaultBranch)
		if err != nil {
			res.Err = err
			return res
		}
		if warn != "" {
			res.Notes = append(res.Notes, warn)
		}
		res.Status = store.StatusCloned
		// AIDEV: a dirty repo never gets its pushedAt recorded, so the
		// warning repeats every run instead of silently serving a stale
		// tree forever; the cost is one wasted fetch per run.
		if !dirty {
			res.PushedAt = repo.PushedAt
		}
		return res

	case plan.Archive, plan.AdoptArchived:
		dir := filepath.Join(env.ReposDir(), repo.Name)
		defaultBranch := ""
		if repo.DefaultBranch != nil {
			defaultBranch = repo.DefaultBranch.Name
		}
		archiveRepoInfo := archive.Repo{
			ID:            repo.ID,
			Owner:         env.Owner,
			Name:          repo.Name,
			NameWithOwner: repo.NameWithOwner,
			URL:           repo.URL,
			PushedAt:      repo.PushedAt,
			ArchivedAt:    repo.ArchivedAt,
			DefaultBranch: defaultBranch,
		}
		clone := func(ctx context.Context) error { return CloneInto(ctx, env, repo) }
		rs, notes, err := archive.Archive(ctx, env.Exec, env.Settings.Timeout, env.Confirm, archiveRepoInfo, dir, env.ArchivesDir(), clone)
		res.Notes = notes
		if err != nil {
			res.Err = err
			return res
		}
		res.PushedAt = rs.PushedAt
		res.Status = rs.Status
		res.ArchivePath = rs.ArchivePath
		return res

	case plan.Unarchive:
		if err := CloneInto(ctx, env, repo); err != nil {
			res.Err = err
			return res
		}
		res.Notes = append(res.Notes, fmt.Sprintf(
			"repo %q is live upstream again; stale archive at archives/%s.tar.gz and archives/%s.json left in place",
			repo.Name, repo.Name, repo.Name))
		res.Status = store.StatusCloned
		res.PushedAt = repo.PushedAt
		return res

	case plan.NotARepo:
		dir := filepath.Join(env.ReposDir(), repo.Name)
		res.Err = fmt.Errorf("%s exists but is not a git repository; left untouched", dir)
		return res

	case plan.MissingUpstream:
		res.Notes = append(res.Notes, t.Reason)
		res.PushedAt = t.Prev.PushedAt
		res.Status = t.Prev.Status
		res.ArchivePath = t.Prev.ArchivePath
		return res
	}

	res.Err = fmt.Errorf("unknown action %q for repo %q", t.Action, repo.Name)
	return res
}
