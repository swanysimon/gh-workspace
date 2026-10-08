package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOwnerRepo(t *testing.T) {
	tests := []struct {
		in        string
		wantOwner string
		wantRep   string
		wantErr   bool
	}{
		{"myorg/myrepo", "myorg", "myrepo", false},
		{"myorg/my-repo.thing", "myorg", "my-repo.thing", false},
		{"myrepo", "", "", true},
		{"myorg/", "", "", true},
		{"/myrepo", "", "", true},
		{"my org/myrepo", "", "", true},
		{"myorg/-myrepo", "", "", true},
	}
	for _, tc := range tests {
		owner, repo, err := parseOwnerRepo(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseOwnerRepo(%q): expected an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseOwnerRepo(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if owner != tc.wantOwner || repo != tc.wantRep {
			t.Errorf("parseOwnerRepo(%q) = (%q, %q), want (%q, %q)", tc.in, owner, repo, tc.wantOwner, tc.wantRep)
		}
	}
}

func TestWorktreeAddClonesAndAddsWorktree(t *testing.T) {
	origin := initTestRepo(t)
	if _, err := execCommand(context.Background(), origin, "git", "branch", "feature"); err != nil {
		t.Fatal(err)
	}

	repo := ghRepo{
		ID:            "R_repo1",
		Name:          "repo1",
		NameWithOwner: "myorg/repo1",
		URL:           "file://" + origin,
		SSHURL:        "file://" + origin,
		DefaultBranch: &ghRefName{Name: "main"},
	}
	repoJSON, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}

	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			return repoJSON, nil
		}
		return old(ctx, dir, name, args...)
	}

	root := t.TempDir()
	wtPath := filepath.Join(t.TempDir(), "repo1-feature")

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", root, "--protocol", "https", "myorg/repo1", "feature", wtPath}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeAdd = %d, stderr=%s", code, stderr.String())
	}

	if _, err := os.Stat(filepath.Join(root, "myorg", "repos", "repo1", ".git")); err != nil {
		t.Fatalf("repo should have been cloned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "file.txt")); err != nil {
		t.Fatalf("worktree should have been created: %v", err)
	}

	stateRaw, err := os.ReadFile(filepath.Join(root, "myorg", "state.json"))
	if err != nil {
		t.Fatalf("state.json should have been written: %v", err)
	}
	var st state
	if err := json.Unmarshal(stateRaw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Repos["repo1"].Status != statusCloned {
		t.Fatalf("state status = %q, want %q", st.Repos["repo1"].Status, statusCloned)
	}
}

func TestWorktreeAddRefusesArchivedAfterCloning(t *testing.T) {
	origin := initTestRepo(t)

	repo := ghRepo{
		ID:            "R_repo1",
		Name:          "repo1",
		NameWithOwner: "myorg/repo1",
		URL:           "file://" + origin,
		SSHURL:        "file://" + origin,
		IsArchived:    true,
		DefaultBranch: &ghRefName{Name: "main"},
	}
	repoJSON, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}

	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			return repoJSON, nil
		}
		return old(ctx, dir, name, args...)
	}

	root := t.TempDir()
	wtPath := filepath.Join(t.TempDir(), "repo1-main")

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", root, "--protocol", "https", "myorg/repo1", "main", wtPath}, &stdout, &stderr)
	if code == exitSuccess {
		t.Fatalf("expected failure for an archived repo")
	}
	if !strings.Contains(stderr.String(), "archived") {
		t.Fatalf("stderr does not mention archived: %s", stderr.String())
	}
	// The clone must still have happened even though the worktree add is refused.
	if _, err := os.Stat(filepath.Join(root, "myorg", "repos", "repo1", ".git")); err != nil {
		t.Fatalf("repo should still have been cloned: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree should not have been created, stat err = %v", err)
	}
}

func TestWorktreeAddSkipsCloneWhenAlreadyPresent(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := ghRepo{ID: "R_repo1", Name: "repo1", NameWithOwner: "testorg/repo1", URL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := execCommand(context.Background(), filepath.Join(reposDir(cfg), "repo1"), "git", "branch", "feature"); err != nil {
		t.Fatal(err)
	}

	repoJSON, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	var ghCalls int
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			ghCalls++
			return repoJSON, nil
		}
		return old(ctx, dir, name, args...)
	}

	wtPath := filepath.Join(t.TempDir(), "repo1-main")
	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", cfg.Root, "--protocol", "https", "testorg/repo1", "feature", wtPath}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeAdd = %d, stderr=%s", code, stderr.String())
	}
	// No state.json should have been written: we never went through the
	// clone-and-record path.
	if _, err := os.Stat(statePath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("state.json should not exist when the clone already existed, stat err = %v", err)
	}
}

func TestWorktreeRemove(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := ghRepo{Name: "repo1", URL: "file://" + origin}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(reposDir(cfg), "repo1")
	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execCommand(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeRemove(context.Background(), []string{"--root", cfg.Root, "testorg/repo1", wtPath}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeRemove = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree should be gone, stat err = %v", err)
	}
}

func TestWorktreeRemovePathOnlyFormFindsOwningRepo(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := ghRepo{Name: "repo1", URL: "file://" + origin}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(reposDir(cfg), "repo1")
	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execCommand(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeRemove(context.Background(), []string{"--root", cfg.Root, wtPath}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeRemove = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree should be gone, stat err = %v", err)
	}
}

func TestWorktreeRemovePathOnlyRejectsNonWorktreePath(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	notAWorktree := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeRemove(context.Background(), []string{"--root", cfg.Root, notAWorktree}, &stdout, &stderr)
	if code != exitRuntimeFail {
		t.Fatalf("cmdWorktreeRemove = %d, want %d; stderr=%s", code, exitRuntimeFail, stderr.String())
	}
}

func TestWorktreeRemovePathOnlyRejectsRepoCloneItself(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := ghRepo{Name: "repo1", URL: "file://" + origin}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(reposDir(cfg), "repo1")

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeRemove(context.Background(), []string{"--root", cfg.Root, dir}, &stdout, &stderr)
	if code != exitRuntimeFail {
		t.Fatalf("cmdWorktreeRemove = %d, want %d (the central clone itself, not a worktree); stderr=%s", code, exitRuntimeFail, stderr.String())
	}
}

func TestWorktreeRemoveRefusesDirtyWithoutForce(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := ghRepo{Name: "repo1", URL: "file://" + origin}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(reposDir(cfg), "repo1")
	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execCommand(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "file.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeRemove(context.Background(), []string{"--root", cfg.Root, "testorg/repo1", wtPath}, &stdout, &stderr)
	if code == exitSuccess {
		t.Fatalf("expected failure removing a dirty worktree without --force")
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree should still exist: %v", err)
	}

	var stdout2, stderr2 bytes.Buffer
	// Flags after the positionals must be honoured too.
	code2 := cmdWorktreeRemove(context.Background(), []string{"--root", cfg.Root, "testorg/repo1", wtPath, "--force"}, &stdout2, &stderr2)
	if code2 != exitSuccess {
		t.Fatalf("cmdWorktreeRemove --force = %d, stderr=%s", code2, stderr2.String())
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree should be gone after --force, stat err = %v", err)
	}
}

func TestWorktreeList(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := ghRepo{Name: "repo1", URL: "file://" + origin}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(reposDir(cfg), "repo1")
	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execCommand(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeList(context.Background(), []string{"--root", cfg.Root, "testorg/repo1"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeList = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), wtPath) {
		t.Fatalf("output missing worktree path: %s", stdout.String())
	}

	var stdout2, stderr2 bytes.Buffer
	code2 := cmdWorktreeList(context.Background(), []string{"--root", cfg.Root, "testorg"}, &stdout2, &stderr2)
	if code2 != exitSuccess {
		t.Fatalf("cmdWorktreeList (org) = %d, stderr=%s", code2, stderr2.String())
	}
	if !strings.Contains(stdout2.String(), "repo1:") || !strings.Contains(stdout2.String(), wtPath) {
		t.Fatalf("org-wide output missing repo1 or worktree path: %s", stdout2.String())
	}
}

func TestRunWorktreeUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runWorktree(context.Background(), []string{"frobnicate"}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("runWorktree(unknown) = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "frobnicate") {
		t.Fatalf("stderr does not name the bad subcommand: %s", stderr.String())
	}
}

func TestRunDispatchesToWorktree(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := ghRepo{Name: "repo1", URL: "file://" + origin}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(reposDir(cfg), "repo1")
	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execCommand(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"worktree", "list", "--root", cfg.Root, "testorg/repo1"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run(worktree list ...) = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), wtPath) {
		t.Fatalf("output missing worktree path: %s", stdout.String())
	}
}

// A relative worktree path must resolve against the caller's cwd, not the
// central clone that git is run from.
func TestWorktreeRelativePathResolvesAgainstCwd(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := ghRepo{ID: "R_repo1", Name: "repo1", NameWithOwner: "testorg/repo1", URL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(reposDir(cfg), "repo1")
	if _, err := execCommand(context.Background(), dir, "git", "branch", "feature"); err != nil {
		t.Fatal(err)
	}

	repoJSON, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			return repoJSON, nil
		}
		return old(ctx, dir, name, args...)
	}

	cwd := t.TempDir()
	t.Chdir(cwd)

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", cfg.Root, "--protocol", "https", "testorg/repo1", "feature", "wt"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeAdd = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(cwd, "wt", "file.txt")); err != nil {
		t.Fatalf("worktree should have been created under cwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wt")); !os.IsNotExist(err) {
		t.Fatalf("worktree must not land inside the central clone, stat err = %v", err)
	}

	var stdout2, stderr2 bytes.Buffer
	code = cmdWorktreeRemove(context.Background(), []string{"--root", cfg.Root, "testorg/repo1", "wt"}, &stdout2, &stderr2)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeRemove = %d, stderr=%s", code, stderr2.String())
	}
	if _, err := os.Stat(filepath.Join(cwd, "wt")); !os.IsNotExist(err) {
		t.Fatalf("worktree should be gone, stat err = %v", err)
	}
}

// worktree add must respect a held org lock even when no clone is needed:
// a concurrent sync may be about to archive (and delete) this clone.
func TestWorktreeAddRefusesWhileLockHeld(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)
	repo := ghRepo{ID: "R_repo1", Name: "repo1", NameWithOwner: "testorg/repo1", URL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath(cfg), []byte("999 sometime\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repoJSON, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			return repoJSON, nil
		}
		return old(ctx, dir, name, args...)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", cfg.Root, "--protocol", "https", "testorg/repo1", "main", wtPath}, &stdout, &stderr)
	if code == exitSuccess {
		t.Fatalf("worktree add succeeded despite a held lock")
	}
	if !strings.Contains(stderr.String(), lockPath(cfg)) {
		t.Fatalf("stderr does not name the lock file: %s", stderr.String())
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree should not have been created, stat err = %v", err)
	}
	if _, err := os.Stat(lockPath(cfg)); err != nil {
		t.Fatalf("someone else's lock must not be removed: %v", err)
	}
}

// A branch pushed upstream after the central clone was made must still be
// usable: worktree add fetches before resolving the branch.
func TestWorktreeAddFetchesBranchCreatedAfterClone(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)
	repo := ghRepo{ID: "R_repo1", Name: "repo1", NameWithOwner: "testorg/repo1", URL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := execCommand(context.Background(), origin, "git", "branch", "late"); err != nil {
		t.Fatal(err)
	}

	stubGhRepoView(t, repo)
	wtPath := filepath.Join(t.TempDir(), "wt")
	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", cfg.Root, "--protocol", "https", "testorg/repo1", "late", wtPath}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeAdd = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "created new branch") {
		t.Fatalf("an upstream branch should be checked out, not created: %s", stdout.String())
	}
	out, err := execCommand(context.Background(), wtPath, "git", "rev-parse", "--abbrev-ref", "late@{upstream}")
	if err != nil || strings.TrimSpace(string(out)) != "origin/late" {
		t.Fatalf("worktree branch should track origin/late, got %q (%v)", out, err)
	}
}

// A branch that exists nowhere is created from origin/<default> without
// taking the default branch as its upstream.
func TestWorktreeAddCreatesMissingBranch(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)
	repo := ghRepo{ID: "R_repo1", Name: "repo1", NameWithOwner: "testorg/repo1", URL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}

	stubGhRepoView(t, repo)
	wtPath := filepath.Join(t.TempDir(), "wt")
	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", cfg.Root, "--protocol", "https", "testorg/repo1", "brand-new", wtPath}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeAdd = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `created new branch "brand-new" from origin/main`) {
		t.Fatalf("stdout should report the new branch: %s", stdout.String())
	}
	out, err := execCommand(context.Background(), wtPath, "git", "symbolic-ref", "--short", "HEAD")
	if err != nil || strings.TrimSpace(string(out)) != "brand-new" {
		t.Fatalf("worktree should be on brand-new, got %q (%v)", out, err)
	}
	if _, err := execCommand(context.Background(), wtPath, "git", "rev-parse", "--abbrev-ref", "brand-new@{upstream}"); err == nil {
		t.Fatalf("new branch must not track the default branch")
	}
}

func TestWorktreeAddRejectsInvalidBranchName(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)
	repo := ghRepo{ID: "R_repo1", Name: "repo1", NameWithOwner: "testorg/repo1", URL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}

	stubGhRepoView(t, repo)
	wtPath := filepath.Join(t.TempDir(), "wt")
	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", cfg.Root, "--protocol", "https", "testorg/repo1", "bad..name", wtPath}, &stdout, &stderr)
	if code == exitSuccess {
		t.Fatalf("expected failure for an invalid branch name")
	}
	if !strings.Contains(stderr.String(), "not a valid branch name") {
		t.Fatalf("stderr should explain the bad branch name: %s", stderr.String())
	}
}

// stubGhRepoView answers every gh call with repo's JSON and passes git
// through to the real binary.
func stubGhRepoView(t *testing.T, repo ghRepo) {
	t.Helper()
	repoJSON, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			return repoJSON, nil
		}
		return old(ctx, dir, name, args...)
	}
}

func TestWorktreeAddDoesNotRecloneLocallyArchivedRepo(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)
	repo := ghRepo{ID: "R_repo1", Name: "repo1", NameWithOwner: "testorg/repo1", URL: "file://" + origin, IsArchived: true, DefaultBranch: &ghRefName{Name: "main"}}
	if err := os.MkdirAll(archivesDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{manifestPath(cfg, "repo1"), tarballPath(cfg, "repo1")} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stubGhRepoView(t, repo)
	wtPath := filepath.Join(t.TempDir(), "wt")
	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", cfg.Root, "--protocol", "https", "testorg/repo1", "main", wtPath}, &stdout, &stderr)
	if code == exitSuccess {
		t.Fatalf("expected failure for an archived repo")
	}
	if !strings.Contains(stderr.String(), "already archived locally") {
		t.Fatalf("stderr should point at the local archive: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(reposDir(cfg), "repo1")); !os.IsNotExist(err) {
		t.Fatalf("locally archived repo must not be re-cloned, stat err = %v", err)
	}
	if _, err := os.Stat(statePath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("state must not be rewritten, stat err = %v", err)
	}
}

// Worktree commands don't accept --concurrency and friends, so a bad value
// in the matching environment variable must not make them fail.
func TestWorktreeIgnoresUnrelatedEnv(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)
	if err := cloneRepo(context.Background(), cfg, ghRepo{Name: "repo1", URL: "file://" + origin}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_ORG_CLONE_CONCURRENCY", "lots")
	t.Setenv("GH_ORG_CLONE_ARCHIVE", "maybe")

	var stdout, stderr bytes.Buffer
	if code := cmdWorktreeList(context.Background(), []string{"--root", cfg.Root, "testorg/repo1"}, &stdout, &stderr); code != exitSuccess {
		t.Fatalf("cmdWorktreeList = %d, stderr=%s", code, stderr.String())
	}

	// A worktree setting with a bad value still fails.
	t.Setenv("GH_ORG_CLONE_TIMEOUT", "soon")
	stderr.Reset()
	if code := cmdWorktreeList(context.Background(), []string{"--root", cfg.Root, "testorg/repo1"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("cmdWorktreeList with bad GH_ORG_CLONE_TIMEOUT = %d, want %d", code, exitUsage)
	}
}

func TestWorktreeAddAppliesTimeoutToGhLookup(t *testing.T) {
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			<-ctx.Done() // a hung gh must be cut off by --timeout
			return nil, ctx.Err()
		}
		return old(ctx, dir, name, args...)
	}

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", t.TempDir(), "--timeout", "50ms", "testorg/repo1", "main", t.TempDir()}, &stdout, &stderr)
	if code != exitRuntimeFail {
		t.Fatalf("cmdWorktreeAdd = %d, want %d; stderr=%s", code, exitRuntimeFail, stderr.String())
	}
	if !strings.Contains(stderr.String(), "deadline exceeded") {
		t.Fatalf("stderr should report the timeout: %s", stderr.String())
	}
}

// Worktree help must list exactly the subcommand's flags, in gh's
// double-dash style with real (config-resolved) defaults.
func TestWorktreeHelpUsesGhFlagStyle(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"worktree", "remove", "--help"}, &stdout, &stderr); code != exitSuccess {
		t.Fatalf("exit = %d", code)
	}
	help := stderr.String()
	for _, want := range []string{"--root string", "--protocol string", `(default "ssh")`, "--timeout duration", "(default 30m0s)", "--config string", "--force"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
	for _, unwanted := range []string{"  -root", "  -force", "--concurrency", "--yes"} {
		if strings.Contains(help, unwanted) {
			t.Fatalf("help should not contain %q:\n%s", unwanted, help)
		}
	}
}

// resolveSubcommandConfig shares expandConfigPaths with resolveConfig; this
// pins that the shared helper actually runs on this path too, not just
// sync's.
func TestResolveSubcommandConfigExpandsHomeInRoot(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	fs, help := newSubcommandFlagSet("gh org-clone worktree add", "usage", io.Discard)
	cfg, _, err := resolveSubcommandConfig(fs, help, cmdWorktree, []string{"--root", "~/src/.workspace", "myorg/repo", "branch", "/tmp/x"})
	if err != nil {
		t.Fatalf("resolveSubcommandConfig: %v", err)
	}
	want := filepath.Join(home, "src/.workspace")
	if cfg.Root != want {
		t.Errorf("cfg.Root = %q, want %q", cfg.Root, want)
	}
}

// TestWorktreeAddDefaultsToCurrentDirectoryAndRepoName confirms the
// documented default (worktreeRoot = current directory, worktreePath =
// "{repo}") actually applies end to end through cmdWorktreeAdd when path
// is omitted -- no owner segment in the resulting path.
func TestWorktreeAddDefaultsToCurrentDirectoryAndRepoName(t *testing.T) {
	origin := initTestRepo(t)
	if _, err := execCommand(context.Background(), origin, "git", "branch", "feature"); err != nil {
		t.Fatal(err)
	}
	stubGhRepoView(t, ghRepo{
		ID: "R1", Name: "repo1", NameWithOwner: "myorg/repo1",
		URL: "file://" + origin, SSHURL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"},
	})

	root := t.TempDir()
	cwd := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", root, "--protocol", "https", "myorg/repo1", "feature"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeAdd = %d, stderr=%s", code, stderr.String())
	}

	want := filepath.Join(cwd, "repo1")
	if _, err := os.Stat(filepath.Join(want, "file.txt")); err != nil {
		t.Fatalf("worktree should have landed at <cwd>/<repo> (%s): %v", want, err)
	}
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("stdout should report the resolved path %q: %q", want, stdout.String())
	}
}

// TestWorktreeAddSecondBranchSamePathFailsWithHint confirms a second
// worktree computed to the same default path (two branches of the same
// repo, no explicit path, default "{repo}" template with no {branch})
// fails with the documented hint, not a renamed/alternate path.
func TestWorktreeAddSecondBranchSamePathFailsWithHint(t *testing.T) {
	origin := initTestRepo(t)
	for _, b := range []string{"feature", "feature2"} {
		if _, err := execCommand(context.Background(), origin, "git", "branch", b); err != nil {
			t.Fatal(err)
		}
	}
	stubGhRepoView(t, ghRepo{
		ID: "R1", Name: "repo1", NameWithOwner: "myorg/repo1",
		URL: "file://" + origin, SSHURL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"},
	})

	root := t.TempDir()
	cwd := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	var stdout1, stderr1 bytes.Buffer
	if code := cmdWorktreeAdd(context.Background(), []string{"--root", root, "--protocol", "https", "myorg/repo1", "feature"}, &stdout1, &stderr1); code != exitSuccess {
		t.Fatalf("first cmdWorktreeAdd = %d, stderr=%s", code, stderr1.String())
	}

	var stdout2, stderr2 bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{"--root", root, "--protocol", "https", "myorg/repo1", "feature2"}, &stdout2, &stderr2)
	if code != exitRuntimeFail {
		t.Fatalf("second cmdWorktreeAdd = %d, want %d; stderr=%s", code, exitRuntimeFail, stderr2.String())
	}
	if !strings.Contains(stderr2.String(), "already exists") || !strings.Contains(stderr2.String(), "{branch}") {
		t.Fatalf("stderr should hint at an explicit path or a {branch} template: %q", stderr2.String())
	}
}

// TestWorktreeAddConfiguredTemplateWithBranchSlash confirms a configured
// --worktree-path containing {branch}, given a branch name with a "/",
// lands the worktree with the slash replaced by a dash, not an extra
// directory level.
func TestWorktreeAddConfiguredTemplateWithBranchSlash(t *testing.T) {
	origin := initTestRepo(t)
	if _, err := execCommand(context.Background(), origin, "git", "branch", "feat/x"); err != nil {
		t.Fatal(err)
	}
	stubGhRepoView(t, ghRepo{
		ID: "R1", Name: "repo1", NameWithOwner: "myorg/repo1",
		URL: "file://" + origin, SSHURL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"},
	})

	root := t.TempDir()
	wtRoot := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{
		"--root", root, "--protocol", "https",
		"--worktree-root", wtRoot, "--worktree-path", "{repo}/{branch}",
		"myorg/repo1", "feat/x",
	}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeAdd = %d, stderr=%s", code, stderr.String())
	}

	want := filepath.Join(wtRoot, "repo1", "feat-x")
	if _, err := os.Stat(filepath.Join(want, "file.txt")); err != nil {
		t.Fatalf("worktree should have landed at %s: %v", want, err)
	}
}

// TestWorktreeAddRejectsRelativeWorktreeRoot confirms a relative
// --worktree-root is rejected the same way a relative --root already is.
func TestWorktreeAddRejectsRelativeWorktreeRoot(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{
		"--root", t.TempDir(), "--worktree-root", "relative/path",
		"myorg/repo1", "main",
	}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("cmdWorktreeAdd = %d, want %d; stderr=%s", code, exitUsage, stderr.String())
	}
}

// TestWorktreeAddExplicitPathOutsideWorktreeRootAllowed confirms an
// explicit path argument is never subject to the under-worktree-root
// check -- "the user always specifies where a worktree lives."
func TestWorktreeAddExplicitPathOutsideWorktreeRootAllowed(t *testing.T) {
	origin := initTestRepo(t)
	if _, err := execCommand(context.Background(), origin, "git", "branch", "feature"); err != nil {
		t.Fatal(err)
	}
	stubGhRepoView(t, ghRepo{
		ID: "R1", Name: "repo1", NameWithOwner: "myorg/repo1",
		URL: "file://" + origin, SSHURL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"},
	})

	root := t.TempDir()
	wtRoot := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeAdd(context.Background(), []string{
		"--root", root, "--protocol", "https", "--worktree-root", wtRoot,
		"myorg/repo1", "feature", elsewhere,
	}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeAdd = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "file.txt")); err != nil {
		t.Fatalf("worktree should have landed at the explicit path %s: %v", elsewhere, err)
	}
}

// TestWorktreeListWorkspaceWideNoArg confirms "worktree list" with no
// positional at all lists every worktree across every owner directory
// under cfg.Root, each repo's header qualified with its owner so output
// across owners is never ambiguous.
func TestWorktreeListWorkspaceWideNoArg(t *testing.T) {
	originA := initTestRepo(t)
	originB := initTestRepo(t)
	root := t.TempDir()

	cfgA := testConfig(t, root)
	cfgA.Owner = "owner-a"
	cfgA.Protocol = "https"
	if err := os.MkdirAll(reposDir(cfgA), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cloneRepo(context.Background(), cfgA, ghRepo{Name: "repoa", URL: "file://" + originA}); err != nil {
		t.Fatal(err)
	}
	wtA := filepath.Join(t.TempDir(), "wta")
	if _, err := execCommand(context.Background(), filepath.Join(reposDir(cfgA), "repoa"), "git", "worktree", "add", "-b", "feature-a", wtA, "main"); err != nil {
		t.Fatal(err)
	}

	cfgB := testConfig(t, root)
	cfgB.Owner = "owner-b"
	cfgB.Protocol = "https"
	if err := os.MkdirAll(reposDir(cfgB), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cloneRepo(context.Background(), cfgB, ghRepo{Name: "repob", URL: "file://" + originB}); err != nil {
		t.Fatal(err)
	}
	wtB := filepath.Join(t.TempDir(), "wtb")
	if _, err := execCommand(context.Background(), filepath.Join(reposDir(cfgB), "repob"), "git", "worktree", "add", "-b", "feature-b", wtB, "main"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeList(context.Background(), []string{"--root", root}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeList = %d, stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"owner-a/repoa:", wtA, "owner-b/repob:", wtB} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// TestWorktreeListWorkspaceWideSkipsOwnersWithNothingCloned confirms an
// owner directory with no "repos/" subdirectory at all (nothing ever
// cloned for it) is not an error for the workspace-wide form.
func TestWorktreeListWorkspaceWideSkipsOwnersWithNothingCloned(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "empty-owner"), 0o700); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdWorktreeList(context.Background(), []string{"--root", root}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdWorktreeList = %d, stderr=%s", code, stderr.String())
	}
}
