package gitcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swanysimon/gh-org-clone/internal/execx"
)

// initTestRepo creates a real git repo in a fresh temp dir with one commit
// on "main" and returns its path.
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

const testTimeout = 30 * time.Second

func TestClone(t *testing.T) {
	origin := initTestRepo(t)
	root := t.TempDir()
	dest := filepath.Join(root, "repo1")
	tmp := filepath.Join(root, ".tmp-repo1")

	ctx := context.Background()
	if err := Clone(ctx, execx.Run, testTimeout, "file://"+origin, dest, tmp); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Fatalf("clone did not land at dest: %v", err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("tmp dir should be gone after a successful clone, stat err = %v", err)
	}
}

func TestCloneFailureCleansUpTmpAndDoesNotRename(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "repo1")
	tmp := filepath.Join(root, ".tmp-repo1")

	ctx := context.Background()
	err := Clone(ctx, execx.Run, testTimeout, "file:///does/not/exist", dest, tmp)
	if err == nil {
		t.Fatal("expected an error cloning a nonexistent repo")
	}
	if _, statErr := os.Stat(tmp); !os.IsNotExist(statErr) {
		t.Fatalf("tmp dir should be cleaned up after a failed clone, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("dest should not exist after a failed clone, stat err = %v", statErr)
	}
}

func TestFetchThenUpdateWorktree(t *testing.T) {
	origin := initTestRepo(t)
	root := t.TempDir()
	dir := filepath.Join(root, "repo1")
	ctx := context.Background()

	if err := Clone(ctx, execx.Run, testTimeout, "file://"+origin, dir, filepath.Join(root, ".tmp")); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	if _, err := execx.Run(ctx, origin, "git", "checkout", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "file.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := execx.Run(ctx, origin, "git", "add", "file.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := execx.Run(ctx, origin, "git", "commit", "--quiet", "-m", "second"); err != nil {
		t.Fatal(err)
	}

	if err := Fetch(ctx, execx.Run, testTimeout, dir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if warn, _, err := UpdateWorktree(ctx, execx.Run, testTimeout, dir, "main"); err != nil || warn != "" {
		t.Fatalf("UpdateWorktree: warn=%q err=%v", warn, err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2\n" {
		t.Fatalf("working tree file = %q, want %q", got, "v2\n")
	}
}

func TestUpdateWorktreeDirty(t *testing.T) {
	origin := initTestRepo(t)
	root := t.TempDir()
	dir := filepath.Join(root, "repo1")
	ctx := context.Background()

	if err := Clone(ctx, execx.Run, testTimeout, "file://"+origin, dir, filepath.Join(root, ".tmp")); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(origin, "file.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := execx.Run(ctx, origin, "git", "add", "file.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := execx.Run(ctx, origin, "git", "commit", "--quiet", "-m", "second"); err != nil {
		t.Fatal(err)
	}
	if err := Fetch(ctx, execx.Run, testTimeout, dir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	warn, _, err := UpdateWorktree(ctx, execx.Run, testTimeout, dir, "main")
	if err != nil {
		t.Fatalf("UpdateWorktree: %v", err)
	}
	if warn == "" {
		t.Fatalf("expected a warning for a dirty worktree")
	}

	got, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "dirty\n" {
		t.Fatalf("uncommitted edit was overwritten: got %q", got)
	}
}

func TestUpdateWorktreeDetached(t *testing.T) {
	origin := initTestRepo(t)
	root := t.TempDir()
	dir := filepath.Join(root, "repo1")
	ctx := context.Background()

	if err := Clone(ctx, execx.Run, testTimeout, "file://"+origin, dir, filepath.Join(root, ".tmp")); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	if _, err := execx.Run(ctx, dir, "git", "checkout", "--quiet", "--detach"); err != nil {
		t.Fatal(err)
	}

	warn, _, err := UpdateWorktree(ctx, execx.Run, testTimeout, dir, "main")
	if err != nil {
		t.Fatalf("UpdateWorktree: %v", err)
	}
	if warn == "" {
		t.Fatalf("expected a warning for a detached HEAD")
	}
}

func TestHeadInfoAndTags(t *testing.T) {
	origin := initTestRepo(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(origin, "file2.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := execx.Run(ctx, origin, "git", "add", "file2.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := execx.Run(ctx, origin, "git", "commit", "--quiet", "-m", "subject\twith\ttabs"); err != nil {
		t.Fatal(err)
	}
	if _, err := execx.Run(ctx, origin, "git", "tag", "v1-lightweight"); err != nil {
		t.Fatal(err)
	}
	if _, err := execx.Run(ctx, origin, "git", "tag", "-a", "v2-annotated", "-m", "release"); err != nil {
		t.Fatal(err)
	}

	sha, committedAt, subject, err := HeadInfo(ctx, execx.Run, testTimeout, origin, "main")
	if err != nil {
		t.Fatalf("HeadInfo: %v", err)
	}
	if sha == "" {
		t.Fatalf("expected a non-empty sha")
	}
	if subject != "subject\twith\ttabs" {
		t.Fatalf("subject = %q, want tab-containing subject intact", subject)
	}
	if committedAt.IsZero() {
		t.Fatalf("expected a non-zero committedAt")
	}

	tagList, err := Tags(ctx, execx.Run, testTimeout, origin)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tagList) != 2 {
		t.Fatalf("got %d tags, want 2: %+v", len(tagList), tagList)
	}
	names := map[string]bool{}
	for _, tg := range tagList {
		names[tg.Name] = true
		if tg.SHA == "" {
			t.Fatalf("tag %q has empty sha", tg.Name)
		}
		if tg.CreatedAt.IsZero() {
			t.Fatalf("tag %q has zero creation time", tg.Name)
		}
	}
	if !names["v1-lightweight"] || !names["v2-annotated"] {
		t.Fatalf("missing expected tags: %+v", names)
	}
}

func TestHeadInfoEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	if _, err := execx.Run(context.Background(), dir, "git", "init", "--quiet", "-b", "main"); err != nil {
		t.Fatal(err)
	}

	sha, committedAt, subject, err := HeadInfo(context.Background(), execx.Run, testTimeout, dir, "main")
	if err != nil {
		t.Fatalf("HeadInfo on empty repo should not error: %v", err)
	}
	if sha != "" || subject != "" || !committedAt.IsZero() {
		t.Fatalf("expected zero values for empty repo, got sha=%q subject=%q committedAt=%v", sha, subject, committedAt)
	}
}

func TestLinkedWorktrees(t *testing.T) {
	dir := initTestRepo(t)

	got, err := LinkedWorktrees(context.Background(), execx.Run, testTimeout, dir)
	if err != nil {
		t.Fatalf("LinkedWorktrees: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fresh repo should have no linked worktrees, got %v", got)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execx.Run(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	got, err = LinkedWorktrees(context.Background(), execx.Run, testTimeout, dir)
	if err != nil {
		t.Fatalf("LinkedWorktrees: %v", err)
	}
	wantPath, err := filepath.EvalSymlinks(wtPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != wantPath {
		t.Fatalf("LinkedWorktrees = %v, want [%s]", got, wantPath)
	}
}

func TestSetRemoteURL(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()
	if _, err := execx.Run(ctx, dir, "git", "remote", "add", "origin", "https://example.invalid/old.git"); err != nil {
		t.Fatal(err)
	}

	if err := SetRemoteURL(ctx, execx.Run, testTimeout, dir, "https://example.invalid/new.git"); err != nil {
		t.Fatalf("SetRemoteURL: %v", err)
	}

	out, err := execx.Run(ctx, dir, "git", "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "https://example.invalid/new.git\n" {
		t.Fatalf("remote url = %q, want the new url", got)
	}
}
