package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// initTestRepo creates a real git repo in a fresh temp dir with one commit
// on "main" and returns its path. Shared by archive_test.go, main_test.go
// and worktree_test.go, not just this file -- git plumbing behavior itself
// (clone/fetch/dirty-detection/head-info/tags/linked-worktrees) is now
// tested directly against internal/gitcli (see gitcli_test.go), since
// that's where the real implementation lives; this file keeps only the
// shared test fixtures and the root-level shim-wiring tests in
// git_shim_test.go.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		if _, err := execCommand(context.Background(), dir, "git", args...); err != nil {
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

func testConfig(t *testing.T, root string) config {
	cfg := defaultConfig()
	cfg.Root = root
	cfg.Org = "testorg"
	cfg.Timeout = 30 * time.Second
	return cfg
}

func mustMkReposDir(t *testing.T, cfg config) {
	t.Helper()
	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
}
