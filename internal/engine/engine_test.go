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
