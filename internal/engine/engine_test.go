package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swanysimon/gh-workspace/internal/archive"
	"github.com/swanysimon/gh-workspace/internal/execx"
	"github.com/swanysimon/gh-workspace/internal/ghcli"
	"github.com/swanysimon/gh-workspace/internal/plan"
	"github.com/swanysimon/gh-workspace/internal/progress"
	"github.com/swanysimon/gh-workspace/internal/settings"
	"github.com/swanysimon/gh-workspace/internal/store"
	"github.com/swanysimon/gh-workspace/internal/vcs"
)

var errFakeFailure = errors.New("fake failure")

const testTimeout = 30 * time.Second

// initTestRepo creates a real git repo in a fresh temp dir with one commit
// on "main" and returns its path. Duplicated from the same-shaped helper in
// internal/gitcli and internal/archive's own test suites, rather than
// shared across package boundaries for a test-only fixture.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		if _, err := execx.Run(context.Background(), dir, "git", args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	run("init", "--quiet", "-b", "main")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "file.txt")
	run("commit", "--quiet", "-m", "initial")
	return dir
}

func testEnv(t *testing.T) Env {
	t.Helper()
	root := t.TempDir()
	env := Env{
		Exec:    execx.Run,
		Confirm: archive.DefaultConfirm(true),
		Owner:   "shimorg",
		Settings: settings.Settings{
			Root:     root,
			Protocol: "https",
			Timeout:  testTimeout,
			Archive:  true,
			// Concurrency must be >= 1: RunTasks starts exactly this many
			// worker goroutines, so leaving this at its zero value makes
			// RunTasks silently return an empty []Result with no error --
			// no hang, no panic, nothing pointing at the actual cause. This
			// bit the first draft of this helper; it's commented here so it
			// doesn't happen again.
			Concurrency: 1,
		},
	}
	if err := os.MkdirAll(env.ReposDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(env.ArchivesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	return env
}

// TestProcessTaskArchiveMapsFieldsIntoManifest confirms the archive branch
// of processTask (exercised via RunTasks, since processTask itself is
// unexported) actually builds archive.Repo's fields from the ghcli.Repo and
// Env.Owner it was given -- this is the one piece of real logic that
// branch has of its own, previously tested at the root as
// TestArchiveRepoShimMapsFieldsIntoManifest before archiveRepo moved here.
func TestProcessTaskArchiveMapsFieldsIntoManifest(t *testing.T) {
	origin := initTestRepo(t)
	env := testEnv(t)

	pushedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	archivedAt := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	repo := ghcli.Repo{
		Name:          "repo1",
		NameWithOwner: "shimorg/repo1",
		URL:           "file://" + origin,
		IsArchived:    true,
		PushedAt:      pushedAt,
		ArchivedAt:    archivedAt,
		DefaultBranch: &ghcli.RefName{Name: "main"},
	}

	results := RunTasks(context.Background(), env, []Task{{Repo: repo, Action: plan.Archive}}, os.Stderr)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("RunTasks: %+v", results)
	}

	m, ok := archive.ReadManifest(archive.ManifestPath(env.ArchivesDir(), "repo1"))
	if !ok {
		t.Fatalf("could not read manifest")
	}
	if m.Org != "shimorg" {
		t.Errorf("manifest.Org = %q, want %q (Env.Owner did not reach the manifest)", m.Org, "shimorg")
	}
	if m.NameWithOwner != repo.NameWithOwner {
		t.Errorf("manifest.NameWithOwner = %q, want %q", m.NameWithOwner, repo.NameWithOwner)
	}
	if m.URL != repo.URL {
		t.Errorf("manifest.URL = %q, want %q", m.URL, repo.URL)
	}
	if m.DefaultBranch != "main" {
		t.Errorf("manifest.DefaultBranch = %q, want %q", m.DefaultBranch, "main")
	}
	if !m.PushedAt.Equal(pushedAt) {
		t.Errorf("manifest.PushedAt = %v, want %v", m.PushedAt, pushedAt)
	}
	if !m.ArchivedAt.Equal(archivedAt) {
		t.Errorf("manifest.ArchivedAt = %v, want %v", m.ArchivedAt, archivedAt)
	}
}

// TestProcessTaskArchiveComputesDir confirms the archive branch's dir
// computation (env.ReposDir() + repo.Name) is what actually gets archived
// and removed, by checking that directory specifically disappears rather
// than trusting that *some* directory did.
func TestProcessTaskArchiveComputesDir(t *testing.T) {
	origin := initTestRepo(t)
	env := testEnv(t)

	repo := ghcli.Repo{
		Name: "repo1", NameWithOwner: "shimorg/repo1", URL: "file://" + origin,
		IsArchived: true, DefaultBranch: &ghcli.RefName{Name: "main"},
	}
	wantDir := filepath.Join(env.ReposDir(), "repo1")

	results := RunTasks(context.Background(), env, []Task{{Repo: repo, Action: plan.Archive}}, os.Stderr)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("RunTasks: %+v", results)
	}
	if _, err := os.Stat(wantDir); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be gone after archiving, stat err = %v", wantDir, err)
	}
	if results[0].Status != store.StatusArchived {
		t.Fatalf("status = %q, want archived", results[0].Status)
	}
}

// TestEnsureClonedMarksTracked confirms EnsureCloned's one piece of state
// semantics that isn't just "clone it": every repo it clones is recorded
// as Tracked: true, since EnsureCloned is -- by construction -- always an
// explicit add (worktree add today, a standalone clone command later).
func TestEnsureClonedMarksTracked(t *testing.T) {
	origin := initTestRepo(t)
	env := testEnv(t)

	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file://" + origin, PushedAt: time.Now(),
	}

	if err := EnsureCloned(context.Background(), env, repo, os.Stderr); err != nil {
		t.Fatalf("EnsureCloned: %v", err)
	}

	st := store.LoadState(env.StatePath(), env.Owner, os.Stderr)
	rs, ok := st.Repos["repo1"]
	if !ok {
		t.Fatalf("no state entry written for repo1")
	}
	if !rs.Tracked {
		t.Fatalf("Tracked = false, want true: %+v", rs)
	}
	if rs.Status != store.StatusCloned {
		t.Fatalf("Status = %q, want %q", rs.Status, store.StatusCloned)
	}
}

// TestSyncOneClonesWhenNothingLocal confirms the simplest case: no local
// clone, no local archive, not archived upstream -> clone, Tracked: true.
func TestSyncOneClonesWhenNothingLocal(t *testing.T) {
	origin := initTestRepo(t)
	env := testEnv(t)
	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file://" + origin, PushedAt: time.Now(), DefaultBranch: &ghcli.RefName{Name: "main"},
	}

	res, err := SyncOne(context.Background(), env, repo, true, os.Stderr)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if res.Action != plan.Clone {
		t.Fatalf("Action = %q, want %q", res.Action, plan.Clone)
	}
	if _, statErr := os.Stat(filepath.Join(env.ReposDir(), "repo1", ".git")); statErr != nil {
		t.Fatalf("repo should have been cloned: %v", statErr)
	}

	st := store.LoadState(env.StatePath(), env.Owner, os.Stderr)
	rs := st.Repos["repo1"]
	if !rs.Tracked {
		t.Fatalf("Tracked = false, want true: %+v", rs)
	}
	if rs.Status != store.StatusCloned {
		t.Fatalf("Status = %q, want %q", rs.Status, store.StatusCloned)
	}
}

// TestSyncOneSkipsWhenUnchanged confirms an already-cloned repo whose
// pushedAt matches state is skipped -- zero git calls -- and stays marked
// Tracked.
func TestSyncOneSkipsWhenUnchanged(t *testing.T) {
	origin := initTestRepo(t)
	env := testEnv(t)
	pushedAt := time.Now().Truncate(time.Second)
	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file://" + origin, PushedAt: pushedAt, DefaultBranch: &ghcli.RefName{Name: "main"},
	}

	if _, err := SyncOne(context.Background(), env, repo, true, os.Stderr); err != nil {
		t.Fatalf("first SyncOne: %v", err)
	}

	gitCalls := 0
	wrapped := env
	wrapped.Exec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gitCalls++
		return execx.Run(ctx, dir, name, args...)
	}

	res, err := SyncOne(context.Background(), wrapped, repo, true, os.Stderr)
	if err != nil {
		t.Fatalf("second SyncOne: %v", err)
	}
	if res.Action != plan.Skip {
		t.Fatalf("Action = %q, want %q", res.Action, plan.Skip)
	}
	if gitCalls != 0 {
		t.Fatalf("second SyncOne made %d git calls, want 0", gitCalls)
	}

	st := store.LoadState(env.StatePath(), env.Owner, os.Stderr)
	if !st.Repos["repo1"].Tracked {
		t.Fatalf("Tracked should still be true after a skip")
	}
}

// TestSyncOneFetchesWhenChanged confirms an already-cloned repo whose
// pushedAt changed gets fetched (not re-cloned).
func TestSyncOneFetchesWhenChanged(t *testing.T) {
	origin := initTestRepo(t)
	env := testEnv(t)
	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file://" + origin, PushedAt: time.Now(), DefaultBranch: &ghcli.RefName{Name: "main"},
	}
	if _, err := SyncOne(context.Background(), env, repo, true, os.Stderr); err != nil {
		t.Fatalf("first SyncOne: %v", err)
	}

	// New commit upstream, and a later pushedAt to reflect it.
	if err := os.WriteFile(filepath.Join(origin, "file.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := execx.Run(context.Background(), origin, "git", "add", "file.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := execx.Run(context.Background(), origin, "git", "commit", "--quiet", "-m", "second"); err != nil {
		t.Fatal(err)
	}
	repo.PushedAt = time.Now()

	res, err := SyncOne(context.Background(), env, repo, true, os.Stderr)
	if err != nil {
		t.Fatalf("second SyncOne: %v", err)
	}
	if res.Action != plan.Fetch {
		t.Fatalf("Action = %q, want %q", res.Action, plan.Fetch)
	}
	got, err := os.ReadFile(filepath.Join(env.ReposDir(), "repo1", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2\n" {
		t.Fatalf("working tree file = %q, want %q (fast-forward did not happen)", got, "v2\n")
	}
}

// TestSyncOneArchivesWhenArchivedUpstream confirms SyncOne tarballs an
// archived-upstream repo (respecting env.Settings.Archive) instead of
// always cloning it -- the behavior that distinguishes it from
// EnsureCloned/worktree add.
func TestSyncOneArchivesWhenArchivedUpstream(t *testing.T) {
	origin := initTestRepo(t)
	env := testEnv(t)
	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file://" + origin, IsArchived: true, PushedAt: time.Now(),
		DefaultBranch: &ghcli.RefName{Name: "main"},
	}

	res, err := SyncOne(context.Background(), env, repo, true, os.Stderr)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if res.Action != plan.Archive {
		t.Fatalf("Action = %q, want %q", res.Action, plan.Archive)
	}
	if _, statErr := os.Stat(filepath.Join(env.ReposDir(), "repo1")); !os.IsNotExist(statErr) {
		t.Fatalf("clone dir should be gone after archiving, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(archive.TarballPath(env.ArchivesDir(), "repo1")); statErr != nil {
		t.Fatalf("tarball missing: %v", statErr)
	}

	st := store.LoadState(env.StatePath(), env.Owner, os.Stderr)
	rs := st.Repos["repo1"]
	if rs.Status != store.StatusArchived || !rs.Tracked {
		t.Fatalf("got %+v, want archived and tracked", rs)
	}
}

// TestSyncOneAdoptsExistingArchive confirms a repo that's already archived
// locally (manifest + tarball on disk, no clone) is adopted rather than
// re-cloned-then-re-archived.
func TestSyncOneAdoptsExistingArchive(t *testing.T) {
	origin := initTestRepo(t)
	env := testEnv(t)
	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file://" + origin, IsArchived: true, PushedAt: time.Now(),
		DefaultBranch: &ghcli.RefName{Name: "main"},
	}

	if _, err := SyncOne(context.Background(), env, repo, true, os.Stderr); err != nil {
		t.Fatalf("first SyncOne (creates the archive): %v", err)
	}

	gitCalls := 0
	wrapped := env
	wrapped.Exec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gitCalls++
		return execx.Run(ctx, dir, name, args...)
	}

	res, err := SyncOne(context.Background(), wrapped, repo, true, os.Stderr)
	if err != nil {
		t.Fatalf("second SyncOne: %v", err)
	}
	if res.Action != plan.AdoptArchived && res.Action != plan.Skip {
		t.Fatalf("Action = %q, want %q or %q", res.Action, plan.AdoptArchived, plan.Skip)
	}
	if gitCalls != 0 {
		t.Fatalf("adopting an existing archive made %d git calls, want 0", gitCalls)
	}
}

// TestSyncOneDoesNotWriteStateOnFailure confirms a failed SyncOne call
// leaves the existing state entry alone, matching how a full sync's
// per-repo failure handling works (so the repo is retried next time
// instead of recording a result for a run that didn't actually happen).
func TestSyncOneDoesNotWriteStateOnFailure(t *testing.T) {
	env := testEnv(t)
	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file:///does/not/exist", PushedAt: time.Now(),
	}

	if _, err := SyncOne(context.Background(), env, repo, true, os.Stderr); err == nil {
		t.Fatalf("expected an error cloning a nonexistent repo")
	}

	st := store.LoadState(env.StatePath(), env.Owner, os.Stderr)
	if _, ok := st.Repos["repo1"]; ok {
		t.Fatalf("state should not have an entry for repo1 after a failed sync")
	}
}

// TestBuildExplicitTasksMissingUpstream confirms a nil Repo in a
// ghcli.RepoResult produces a MissingUpstream task, not an attempt at
// plan.Decide (which has no RepoFacts for a repo gh can't resolve at all).
func TestBuildExplicitTasksMissingUpstream(t *testing.T) {
	env := testEnv(t)
	st := store.State{Repos: map[string]store.RepoState{
		"gone": {ID: "R1", Status: store.StatusCloned, Tracked: true},
	}}

	tasks := BuildExplicitTasks(env, []ghcli.RepoResult{
		{Owner: env.Owner, Name: "gone", Repo: nil},
	}, st, nil)

	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].Action != plan.MissingUpstream {
		t.Fatalf("Action = %q, want %q", tasks[0].Action, plan.MissingUpstream)
	}
	if tasks[0].Prev.ID != "R1" {
		t.Fatalf("Prev not carried forward: %+v", tasks[0].Prev)
	}
}

// TestBuildExplicitTasksDecidesLikeAFullSync confirms a present Repo in a
// ghcli.RepoResult is decided by the same plan.Decide path a full owner
// listing would use -- nothing missing just because it arrived through
// ViewRepos instead of ListRepos.
func TestBuildExplicitTasksDecidesLikeAFullSync(t *testing.T) {
	env := testEnv(t)
	repo := ghcli.Repo{ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1", PushedAt: time.Now()}

	tasks := BuildExplicitTasks(env, []ghcli.RepoResult{
		{Owner: env.Owner, Name: "repo1", Repo: &repo},
	}, store.State{}, nil)

	if len(tasks) != 1 || tasks[0].Action != plan.Clone {
		t.Fatalf("got %+v, want a single Clone task (nothing local yet)", tasks)
	}
}

// TestBuildExplicitTasksNotesTransfer confirms a repo whose returned
// NameWithOwner names a different owner than it was requested under gets a
// note in its reason, without changing which owner directory it's decided
// against -- "report it, don't move data between owner directories
// automatically."
func TestBuildExplicitTasksNotesTransfer(t *testing.T) {
	env := testEnv(t)
	repo := ghcli.Repo{ID: "R1", Name: "repo1", NameWithOwner: "newowner/repo1", PushedAt: time.Now()}

	tasks := BuildExplicitTasks(env, []ghcli.RepoResult{
		{Owner: env.Owner, Name: "repo1", Repo: &repo},
	}, store.State{}, nil)

	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if !strings.Contains(tasks[0].Reason, "newowner") || !strings.Contains(tasks[0].Reason, "transferred") {
		t.Fatalf("reason should note the transfer: %q", tasks[0].Reason)
	}
}

// TestMultiOwnerRunSavesEachOwnerIndependently confirms two owners' tasks,
// run through one shared pool, each get their own state.json saved with
// their own results -- not merged, not skipped, not waiting on each other.
func TestMultiOwnerRunSavesEachOwnerIndependently(t *testing.T) {
	root := t.TempDir()

	makeEnv := func(owner string) Env {
		e := testEnv(t)
		e.Owner = owner
		e.Settings.Root = root
		return e
	}
	envA := makeEnv("owner-a")
	envB := makeEnv("owner-b")

	repoA := ghcli.Repo{ID: "RA", Name: "repoa", NameWithOwner: "owner-a/repoa", PushedAt: time.Now()}
	repoB := ghcli.Repo{ID: "RB", Name: "repob", NameWithOwner: "owner-b/repob", PushedAt: time.Now()}

	var releasedA, releasedB bool
	owners := []OwnerWork{
		{
			Owner:   "owner-a",
			Env:     envA,
			Tasks:   []Task{{Repo: repoA, Action: plan.Clone, Reason: "no local clone exists"}},
			State:   store.State{Repos: map[string]store.RepoState{}},
			Release: func() { releasedA = true },
		},
		{
			Owner:   "owner-b",
			Env:     envB,
			Tasks:   []Task{{Repo: repoB, Action: plan.Clone, Reason: "no local clone exists"}},
			State:   store.State{Repos: map[string]store.RepoState{}},
			Release: func() { releasedB = true },
		},
	}

	outcomes := MultiOwnerRun(context.Background(), 2, false, owners, os.Stderr)
	if len(outcomes) != 2 {
		t.Fatalf("got %d outcomes, want 2", len(outcomes))
	}
	for _, oc := range outcomes {
		if len(oc.Results) != 1 || oc.Results[0].Err == nil {
			// Cloning a fake repo with no real clone URL is expected to
			// fail -- the point of this test is independence, not a real
			// clone -- but it still must produce exactly one Result.
			t.Fatalf("owner %q: got %+v", oc.Owner, oc.Results)
		}
	}
	if !releasedA || !releasedB {
		t.Fatalf("both owners should have had Release called: a=%v b=%v", releasedA, releasedB)
	}
}

// TestMultiOwnerRunReleasesOwnersWithNoTasks pins a real bug found by manual
// smoke testing: an owner with zero tasks (e.g. under --tracked-only, when
// that owner has nothing explicitly tracked outside its own skipped
// listing) must still have its state saved and its lock released, even
// though it never produces a Result for the remaining-count loop to
// decrement.
func TestMultiOwnerRunReleasesOwnersWithNoTasks(t *testing.T) {
	root := t.TempDir()
	env := testEnv(t)
	env.Owner = "owner-empty"
	env.Settings.Root = root

	var released bool
	saveErr := os.MkdirAll(env.ReposDir(), 0o700)
	if saveErr != nil {
		t.Fatal(saveErr)
	}
	owners := []OwnerWork{
		{
			Owner:   "owner-empty",
			Env:     env,
			Tasks:   nil,
			State:   store.State{Repos: map[string]store.RepoState{}},
			Release: func() { released = true },
		},
	}

	outcomes := MultiOwnerRun(context.Background(), 2, false, owners, os.Stderr)
	if len(outcomes) != 1 {
		t.Fatalf("got %d outcomes, want 1", len(outcomes))
	}
	if outcomes[0].SaveErr != nil {
		t.Fatalf("SaveErr = %v, want nil", outcomes[0].SaveErr)
	}
	if !released {
		t.Fatal("Release was never called for a zero-task owner")
	}
	if _, err := os.Stat(env.StatePath()); err != nil {
		t.Fatalf("state.json should have been written: %v", err)
	}
}

// TestApplyResultsToStateSkipsFailedAndCarriesTrackedForward confirms
// applyResultsToState's two documented rules: a failed result leaves its
// repo's existing state entry untouched, and Tracked always carries
// forward from whatever was already recorded, matching the single-owner
// Run() path's own result-processing loop.
func TestApplyResultsToStateSkipsFailedAndCarriesTrackedForward(t *testing.T) {
	st := store.State{Repos: map[string]store.RepoState{
		"ok":     {ID: "R1", Tracked: true, Status: store.StatusCloned},
		"failed": {ID: "R2", Tracked: true, Status: store.StatusCloned, PushedAt: time.Now().Add(-time.Hour)},
	}}
	staleFailedPushedAt := st.Repos["failed"].PushedAt

	results := []Result{
		{Name: "ok", Status: store.StatusCloned, PushedAt: time.Now()},
		{Name: "failed", Err: errFakeFailure},
	}
	applyResultsToState(&st, results, map[string]Task{
		"ok":     {Repo: ghcli.Repo{ID: "R1-fresh"}},
		"failed": {Repo: ghcli.Repo{ID: "R2-fresh"}},
	})

	if !st.Repos["ok"].Tracked {
		t.Fatalf("ok: Tracked should carry forward as true")
	}
	if st.Repos["ok"].ID != "R1-fresh" {
		t.Fatalf("ok: ID should take the fresh value, got %q", st.Repos["ok"].ID)
	}
	if !st.Repos["failed"].PushedAt.Equal(staleFailedPushedAt) {
		t.Fatalf("failed: entry should be untouched, got %+v", st.Repos["failed"])
	}
}

// requireJJ skips a test that drives the real jj binary when jj is not
// installed, and pins jj's configuration away from the developer's own.
// Duplicated from the same-shaped helpers in internal/jjcli and
// internal/vcs rather than shared across package boundaries, matching how
// initTestRepo above is handled.
func requireJJ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	cfg := filepath.Join(t.TempDir(), "jj.toml")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	t.Setenv("JJ_USER", "Test")
	t.Setenv("JJ_EMAIL", "test@example.com")
}

// TestResolveVCS covers the one piece of policy engine adds on top of
// vcs.Resolve: adoption is only planned for an action that leaves a live
// clone behind. Archiving a repo must not convert it on the way out.
func TestResolveVCS(t *testing.T) {
	root := t.TempDir()
	gitClone := filepath.Join(root, "git")
	if err := os.MkdirAll(filepath.Join(gitClone, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing")

	tests := []struct {
		name       string
		dir        string
		configured string
		act        plan.Action
		wantKind   string
		wantAdopt  bool
	}{
		{"skip adopts", gitClone, "jj", plan.Skip, "jj", true},
		{"fetch adopts", gitClone, "jj", plan.Fetch, "jj", true},
		{"archive does not adopt", gitClone, "jj", plan.Archive, "jj", false},
		{"adopting an existing archive does not adopt", gitClone, "jj", plan.AdoptArchived, "jj", false},
		{"unarchive does not adopt", gitClone, "jj", plan.Unarchive, "jj", false},
		{"configured git never adopts", gitClone, "git", plan.Fetch, "git", false},
		{"unset means git", gitClone, "", plan.Fetch, "git", false},
		{"nothing on disk is cloned as jj", missing, "jj", plan.Clone, "jj", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, adopt := resolveVCS(tt.dir, tt.configured, tt.act)
			if kind != tt.wantKind || adopt != tt.wantAdopt {
				t.Errorf("resolveVCS = (%q, %v), want (%q, %v)", kind, adopt, tt.wantKind, tt.wantAdopt)
			}
		})
	}
}

func TestSyncOneClonesWithJJWhenConfigured(t *testing.T) {
	requireJJ(t)
	origin := initTestRepo(t)
	env := testEnv(t)
	env.Settings.VCS = "jj"
	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file://" + origin, PushedAt: time.Now(), DefaultBranch: &ghcli.RefName{Name: "main"},
	}

	res, err := SyncOne(context.Background(), env, repo, true, os.Stderr)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if res.Action != plan.Clone {
		t.Fatalf("Action = %q, want %q", res.Action, plan.Clone)
	}
	dir := filepath.Join(env.ReposDir(), "repo1")
	for _, name := range []string{".jj", ".git"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("clone is missing %s: %v", name, err)
		}
	}
}

// A repo unchanged upstream is skipped, which normally means zero work --
// but a configured "vcs": "jj" has to take effect anyway, or the setting
// would appear to do nothing until the repo next changed upstream.
func TestSyncOneAdoptsAnExistingGitCloneOnSkip(t *testing.T) {
	requireJJ(t)
	origin := initTestRepo(t)
	env := testEnv(t)
	pushedAt := time.Now()
	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file://" + origin, PushedAt: pushedAt, DefaultBranch: &ghcli.RefName{Name: "main"},
	}

	if _, err := SyncOne(context.Background(), env, repo, true, os.Stderr); err != nil {
		t.Fatalf("SyncOne (git): %v", err)
	}
	dir := filepath.Join(env.ReposDir(), "repo1")
	if _, err := os.Stat(filepath.Join(dir, ".jj")); !os.IsNotExist(err) {
		t.Fatalf("precondition: clone should not be jj yet, stat err = %v", err)
	}

	env.Settings.VCS = "jj"
	res, err := SyncOne(context.Background(), env, repo, true, os.Stderr)
	if err != nil {
		t.Fatalf("SyncOne (jj): %v", err)
	}
	if res.Action != plan.Skip {
		t.Fatalf("Action = %q, want %q: adoption must not change the planned action", res.Action, plan.Skip)
	}
	if _, err := os.Stat(filepath.Join(dir, ".jj")); err != nil {
		t.Errorf("clone was not adopted into jj: %v", err)
	}
}

// "A dirty working tree is never touched" covers adoption too: jj git init
// would snapshot the uncommitted work into @ and stage it in git's index.
func TestSyncOneDoesNotAdoptADirtyClone(t *testing.T) {
	requireJJ(t)
	origin := initTestRepo(t)
	env := testEnv(t)
	pushedAt := time.Now()
	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file://" + origin, PushedAt: pushedAt, DefaultBranch: &ghcli.RefName{Name: "main"},
	}

	if _, err := SyncOne(context.Background(), env, repo, true, os.Stderr); err != nil {
		t.Fatalf("SyncOne (git): %v", err)
	}
	dir := filepath.Join(env.ReposDir(), "repo1")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("local work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env.Settings.VCS = "jj"
	res, err := SyncOne(context.Background(), env, repo, true, os.Stderr)
	if err != nil {
		t.Fatalf("SyncOne (jj): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".jj")); !os.IsNotExist(err) {
		t.Errorf("a dirty clone must not be adopted, stat err = %v", err)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "not adopting") {
		t.Errorf("expected a note explaining the skip, got %q", res.Notes)
	}
}

// A dry run must report a planned adoption rather than letting it happen
// unannounced on the next real run.
func TestBuildTasksReportsAPlannedAdoptionInTheReason(t *testing.T) {
	env := testEnv(t)
	env.Settings.VCS = "jj"
	env.Settings.DryRun = true

	dir := filepath.Join(env.ReposDir(), "repo1")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	pushedAt := time.Now()
	st := store.State{Repos: map[string]store.RepoState{
		"repo1": {ID: "R1", PushedAt: pushedAt, Status: store.StatusCloned},
	}}
	repo := ghcli.Repo{ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1", PushedAt: pushedAt}

	tasks, _, failed := BuildTasks(context.Background(), env, []ghcli.Repo{repo}, st, os.Stderr)
	if failed != 0 || len(tasks) != 1 {
		t.Fatalf("got %d tasks, %d failed", len(tasks), failed)
	}
	if !tasks[0].Adopt {
		t.Error("Adopt = false, want true")
	}
	if !strings.Contains(tasks[0].Reason, "adopt the existing git clone into jj") {
		t.Errorf("reason does not mention adoption: %q", tasks[0].Reason)
	}
}

// IncludeArchived gates an upstream-archived repo exactly like IncludeForks
// gates a fork: excluded (and absent from "seen") by default, included and
// planned normally once set.
func TestBuildTasksFiltersArchivedReposUnlessIncludeArchived(t *testing.T) {
	env := testEnv(t)
	repo := ghcli.Repo{ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1", IsArchived: true, PushedAt: time.Now()}

	tasks, seen, failed := BuildTasks(context.Background(), env, []ghcli.Repo{repo}, store.State{}, os.Stderr)
	if failed != 0 || len(tasks) != 0 || seen["repo1"] {
		t.Fatalf("got %d tasks, seen=%v, want the archived repo excluded", len(tasks), seen)
	}

	env.Settings.IncludeArchived = true
	tasks, seen, failed = BuildTasks(context.Background(), env, []ghcli.Repo{repo}, store.State{}, os.Stderr)
	if failed != 0 || len(tasks) != 1 || !seen["repo1"] {
		t.Fatalf("got %d tasks, seen=%v, want the archived repo included", len(tasks), seen)
	}
}

// A per-repo "vcs" override has to reach the task, since a workspace-wide
// sync is the main way an individually configured repo gets synced.
func TestBuildExplicitTasksUsesThePerRepoVCS(t *testing.T) {
	env := testEnv(t) // VCS unset, i.e. git
	repo := ghcli.Repo{ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1", PushedAt: time.Now()}

	tasks := BuildExplicitTasks(env, []ghcli.RepoResult{
		{Owner: env.Owner, Name: "repo1", Repo: &repo},
	}, store.State{}, func(string) string { return "jj" })

	if len(tasks) != 1 || tasks[0].VCS != "jj" {
		t.Fatalf("got %+v, want one task with VCS \"jj\"", tasks)
	}
}

// A repo renamed upstream moves its clone's directory, which breaks every
// working copy attached to it. The rename is matched by repo ID, so this
// drives the whole BuildTasks path rather than fixupRename directly.
func TestBuildTasksRenameRepairsLinkedWorktrees(t *testing.T) {
	requireJJ(t)
	for _, kind := range []string{"git", "jj"} {
		t.Run(kind, func(t *testing.T) {
			origin := initTestRepo(t)
			env := testEnv(t)
			env.Settings.VCS = kind
			ctx := context.Background()

			// Clone under the old name, with a working copy attached.
			old := ghcli.Repo{
				ID: "R1", Name: "oldname", NameWithOwner: "shimorg/oldname",
				URL: "file://" + origin, PushedAt: time.Now(), DefaultBranch: &ghcli.RefName{Name: "main"},
			}
			if _, err := SyncOne(ctx, env, old, true, os.Stderr); err != nil {
				t.Fatalf("SyncOne: %v", err)
			}
			oldDir := filepath.Join(env.ReposDir(), "oldname")
			wt := filepath.Join(t.TempDir(), "wt")
			// A new branch, not "main": the central clone already has the
			// default branch checked out, and git won't hand the same
			// branch to two worktrees.
			if _, err := vcs.WorktreeAdd(ctx, env.Exec, testTimeout, oldDir, wt, "feature", "main"); err != nil {
				t.Fatalf("WorktreeAdd: %v", err)
			}

			// Same ID, new name: that is how a rename is recognized.
			renamed := old
			renamed.Name = "newname"
			renamed.NameWithOwner = "shimorg/newname"
			st := store.LoadState(env.StatePath(), env.Owner, os.Stderr)
			if _, _, failed := BuildTasks(ctx, env, []ghcli.Repo{renamed}, st, os.Stderr); failed != 0 {
				t.Fatalf("%d repos failed to plan", failed)
			}

			newDir := filepath.Join(env.ReposDir(), "newname")
			if _, err := os.Stat(newDir); err != nil {
				t.Fatalf("clone was not moved to the new name: %v", err)
			}
			if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
				t.Errorf("old directory should be gone, stat err = %v", err)
			}
			// The point of the phase: the worktree still works, and was
			// neither deleted nor re-cloned.
			if _, err := vcs.IsDirty(ctx, env.Exec, testTimeout, wt); err != nil {
				t.Errorf("worktree was not repaired after the rename: %v", err)
			}
			got, err := vcs.Worktrees(ctx, env.Exec, testTimeout, newDir)
			if err != nil {
				t.Fatalf("Worktrees: %v", err)
			}
			if len(got) != 1 {
				t.Errorf("got %v, want the one worktree still attached", got)
			}
		})
	}
}

// cloneForMissing puts a real clone and a state entry in place for name,
// as a previous sync would have left them.
func cloneForMissing(t *testing.T, env Env, name string, prev store.RepoState) store.State {
	t.Helper()
	origin := initTestRepo(t)
	repo := ghcli.Repo{
		ID: prev.ID, Name: name, NameWithOwner: env.Owner + "/" + name,
		URL: "file://" + origin, PushedAt: prev.PushedAt, DefaultBranch: &ghcli.RefName{Name: "main"},
	}
	if _, err := SyncOne(context.Background(), env, repo, true, os.Stderr); err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	st := store.LoadState(env.StatePath(), env.Owner, os.Stderr)
	rs := st.Repos[name]
	rs.MissingSince = prev.MissingSince
	st.Repos[name] = rs
	return st
}

func TestMissingTasksReportsWithoutArchiveMissing(t *testing.T) {
	env := testEnv(t)
	st := cloneForMissing(t, env, "repo1", store.RepoState{ID: "R1", PushedAt: time.Now()})

	tasks := MissingTasks(env, st, map[string]bool{}, nil)
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].Action != plan.MissingUpstream {
		t.Errorf("Action = %q, want %q", tasks[0].Action, plan.MissingUpstream)
	}
	if !strings.Contains(tasks[0].Reason, "first seen missing on this run") {
		t.Errorf("reason = %q", tasks[0].Reason)
	}

	// Running it records the date without touching the clone.
	res := RunTasks(context.Background(), env, tasks, os.Stderr)
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("RunTasks: %+v", res)
	}
	if res[0].MissingSince.IsZero() {
		t.Error("MissingSince was not stamped")
	}
	if _, err := os.Stat(filepath.Join(env.ReposDir(), "repo1", ".git")); err != nil {
		t.Errorf("the clone must be left alone: %v", err)
	}
}

// On a later run the report says how long, which is the whole reason the
// date is recorded: GitHub can't say *why* a repo stopped resolving.
func TestMissingTasksReportsHowLongWhenAlreadyKnown(t *testing.T) {
	env := testEnv(t)
	since := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
	st := cloneForMissing(t, env, "repo1", store.RepoState{ID: "R1", PushedAt: time.Now(), MissingSince: since})

	tasks := MissingTasks(env, st, map[string]bool{}, nil)
	if len(tasks) != 1 || !strings.Contains(tasks[0].Reason, "missing since 2026-03-04") {
		t.Fatalf("reason = %q", tasks[0].Reason)
	}

	res := RunTasks(context.Background(), env, tasks, os.Stderr)
	if len(res) != 1 || !res[0].MissingSince.Equal(since) {
		t.Fatalf("MissingSince = %v, want it preserved as %v", res[0].MissingSince, since)
	}
}

func TestMissingTasksArchivesTheCloneWhenAsked(t *testing.T) {
	env := testEnv(t)
	env.Settings.ArchiveMissing = true
	since := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
	st := cloneForMissing(t, env, "repo1", store.RepoState{ID: "R1", PushedAt: time.Now(), MissingSince: since})

	tasks := MissingTasks(env, st, map[string]bool{}, nil)
	if len(tasks) != 1 || tasks[0].Action != plan.Archive {
		t.Fatalf("got %+v, want one Archive task", tasks)
	}
	if !strings.Contains(tasks[0].Reason, "archiving the local clone") {
		t.Errorf("reason = %q", tasks[0].Reason)
	}

	res := RunTasks(context.Background(), env, tasks, os.Stderr)
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("RunTasks: %+v", res)
	}
	// Data preserved as an archive, working tree reclaimed, nothing
	// deleted outright.
	if _, err := os.Stat(archive.TarballPath(env.ArchivesDir(), "repo1")); err != nil {
		t.Errorf("tarball missing: %v", err)
	}
	m, ok := archive.ReadManifest(archive.ManifestPath(env.ArchivesDir(), "repo1"))
	if !ok {
		t.Fatal("manifest missing")
	}
	if m.NameWithOwner != "shimorg/repo1" {
		t.Errorf("manifest.NameWithOwner = %q", m.NameWithOwner)
	}
	// No default branch was known, so the head commit came from HEAD --
	// the manifest still records real history rather than nothing.
	if m.HeadSHA == "" {
		t.Error("manifest recorded no head commit")
	}
	if _, err := os.Stat(filepath.Join(env.ReposDir(), "repo1")); !os.IsNotExist(err) {
		t.Errorf("clone should be gone after archiving, stat err = %v", err)
	}
	if res[0].Status != store.StatusArchived {
		t.Errorf("Status = %q, want archived", res[0].Status)
	}
	if !res[0].MissingSince.Equal(since) {
		t.Errorf("MissingSince = %v, want it kept as %v", res[0].MissingSince, since)
	}
}

// With no local clone there is nothing to archive, so the repo is only
// ever reported -- even with the flag.
func TestMissingTasksOnlyReportsWhenThereIsNoClone(t *testing.T) {
	env := testEnv(t)
	env.Settings.ArchiveMissing = true
	st := store.State{Repos: map[string]store.RepoState{
		"repo1": {ID: "R1", Status: store.StatusArchived},
	}}

	tasks := MissingTasks(env, st, map[string]bool{}, nil)
	if len(tasks) != 1 || tasks[0].Action != plan.MissingUpstream {
		t.Fatalf("got %+v, want one MissingUpstream task", tasks)
	}
}

// The trap this function exists to avoid: a fork excluded by
// includeForks, an invalid name and a case-collision are all absent from
// BuildTasks' "seen" even though GitHub returned them. Keying off the raw
// listing instead means --archive-missing can never tarball them.
func TestMissingTasksIgnoresReposTheListingReturned(t *testing.T) {
	env := testEnv(t)
	env.Settings.ArchiveMissing = true
	st := store.State{Repos: map[string]store.RepoState{
		"afork":    {ID: "R1", Status: store.StatusCloned},
		"explicit": {ID: "R2", Status: store.StatusCloned},
		"gone":     {ID: "R3", Status: store.StatusCloned},
	}}

	tasks := MissingTasks(env, st,
		map[string]bool{"afork": true},    // returned, then filtered out
		map[string]bool{"explicit": true}) // getting its own batched lookup
	if len(tasks) != 1 || tasks[0].Repo.Name != "gone" {
		t.Fatalf("got %+v, want only \"gone\"", tasks)
	}
}

// A repo that resolves again has the record cleared by the same state
// write that records its sync -- no separate bookkeeping.
func TestSyncOneClearsMissingSinceWhenARepoComesBack(t *testing.T) {
	env := testEnv(t)
	origin := initTestRepo(t)
	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file://" + origin, PushedAt: time.Now(), DefaultBranch: &ghcli.RefName{Name: "main"},
	}
	if _, err := SyncOne(context.Background(), env, repo, true, os.Stderr); err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	st := store.LoadState(env.StatePath(), env.Owner, os.Stderr)
	rs := st.Repos["repo1"]
	rs.MissingSince = time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
	rs.PushedAt = time.Time{} // force a fetch rather than a skip
	st.Repos["repo1"] = rs
	if err := store.SaveState(env.StatePath(), st); err != nil {
		t.Fatal(err)
	}

	if _, err := SyncOne(context.Background(), env, repo, true, os.Stderr); err != nil {
		t.Fatalf("second SyncOne: %v", err)
	}
	after := store.LoadState(env.StatePath(), env.Owner, os.Stderr)
	if !after.Repos["repo1"].MissingSince.IsZero() {
		t.Errorf("MissingSince = %v, want it cleared", after.Repos["repo1"].MissingSince)
	}
}

// TestRunTasksReportsProgressPerRepo confirms every non-skip task is shown
// by owner/repo, a skip is not, and verbose adds a line as each finishes.
func TestRunTasksReportsProgressPerRepo(t *testing.T) {
	origin := initTestRepo(t)
	env := testEnv(t)
	env.Settings.Verbose = true
	repo := ghcli.Repo{
		ID: "R1", Name: "repo1", NameWithOwner: "shimorg/repo1",
		URL: "file://" + origin, PushedAt: time.Now(), DefaultBranch: &ghcli.RefName{Name: "main"},
	}

	var stderr bytes.Buffer
	results := RunTasks(context.Background(), env, []Task{
		{Repo: repo, Action: plan.Clone},
		{Repo: ghcli.Repo{Name: "unchanged"}, Action: plan.Skip},
	}, &stderr)
	if len(results) != 2 {
		t.Fatalf("RunTasks: %+v", results)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 2 || lines[0] != "[1/1] shimorg/repo1: cloning" || !strings.HasPrefix(lines[1], "[1/1] shimorg/repo1: cloning done in ") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestPausingWrapsConfirm confirms the progress-pausing wrapper still asks
// the original prompt and returns its answer.
func TestPausingWrapsConfirm(t *testing.T) {
	asked := ""
	env := Env{Confirm: func(repoName string, _ []archive.WorktreeStatus) (bool, error) {
		asked = repoName
		return true, nil
	}}
	tr := progress.New(io.Discard, 1, false)
	defer tr.Stop()
	ok, err := env.pausing(tr).Confirm("repo1", nil)
	if !ok || err != nil || asked != "repo1" {
		t.Fatalf("Confirm = %v, %v; asked %q", ok, err, asked)
	}
	if (Env{}).pausing(tr).Confirm != nil {
		t.Fatal("a nil Confirm should stay nil")
	}
}
