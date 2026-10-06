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
// remember creating. The caller must hold the owner's lock.
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
	}
	st.UpdatedAt = time.Now()
	return store.SaveState(env.StatePath(), st)
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
// decide() shim did, now engine's to own since BuildTasks is the only
// caller of plan.Decide for a full sync's tasks.
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
	plan.Clone:         "cloning",
	plan.Fetch:         "fetching",
	plan.Archive:       "archiving",
	plan.AdoptArchived: "adopting existing archive",
	plan.Unarchive:     "unarchiving (repo live again upstream)",
	plan.NotARepo:      "checking",
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
	}

	res.Err = fmt.Errorf("unknown action %q for repo %q", t.Action, repo.Name)
	return res
}
