package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swanysimon/gh-org-clone/internal/archive"
	"github.com/swanysimon/gh-org-clone/internal/execx"
	"github.com/swanysimon/gh-org-clone/internal/ghcli"
	"github.com/swanysimon/gh-org-clone/internal/plan"
	"github.com/swanysimon/gh-org-clone/internal/settings"
	"github.com/swanysimon/gh-org-clone/internal/store"
)

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
