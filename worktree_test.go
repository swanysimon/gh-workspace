package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOrgRepo(t *testing.T) {
	tests := []struct {
		in      string
		wantOrg string
		wantRep string
		wantErr bool
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
		org, repo, err := parseOrgRepo(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseOrgRepo(%q): expected an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseOrgRepo(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if org != tc.wantOrg || repo != tc.wantRep {
			t.Errorf("parseOrgRepo(%q) = (%q, %q), want (%q, %q)", tc.in, org, repo, tc.wantOrg, tc.wantRep)
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
	code := run(context.Background(), []string{"worktree", "list", "--root", cfg.Root, "testorg/repo1"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("run(worktree list ...) = %d, stderr=%s", code, stderr.String())
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
	if code := run(context.Background(), []string{"worktree", "remove", "--help"}, &stdout, &stderr); code != exitSuccess {
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
