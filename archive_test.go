package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func setupArchiveRepo(t *testing.T) (cfg config, repo ghRepo, dir string) {
	t.Helper()
	origin := initTestRepo(t)
	cfg = testConfig(t, t.TempDir())
	if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archivesDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.Protocol = "https"

	repo = ghRepo{
		Name:          "repo1",
		NameWithOwner: "testorg/repo1",
		URL:           "file://" + origin,
		IsArchived:    true,
		DefaultBranch: &ghRefName{Name: "main"},
	}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatalf("cloneRepo: %v", err)
	}
	dir = filepath.Join(reposDir(cfg), "repo1")

	// Add a subdirectory, a symlink and an executable file for the tarball
	// entry-set assertions.
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested.txt"), []byte("nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("run.sh", filepath.Join(dir, "run-link.sh")); err != nil {
		t.Fatal(err)
	}
	// Commit them so the working tree is clean, which archiveRepo requires.
	for _, args := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"add", "-A"},
		{"commit", "--quiet", "-m", "add fixtures"},
	} {
		if _, err := execCommand(context.Background(), dir, "git", args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return cfg, repo, dir
}

func TestArchiveRepo(t *testing.T) {
	cfg, repo, dir := setupArchiveRepo(t)

	got, notes, err := archiveRepo(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("archiveRepo: %v (notes=%v)", err, notes)
	}
	if got.Status != statusArchived {
		t.Fatalf("status = %q, want archived", got.Status)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected clone dir to be gone, stat err = %v", err)
	}

	tb := tarballPath(cfg, "repo1")
	mp := manifestPath(cfg, "repo1")
	m, ok := readManifest(mp)
	if !ok {
		t.Fatalf("could not read manifest at %s", mp)
	}

	f, err := os.Open(tb)
	if err != nil {
		t.Fatalf("opening tarball: %v", err)
	}
	defer f.Close()

	hash := sha256.New()
	gz, err := gzip.NewReader(io.TeeReader(f, hash))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	tr := tar.NewReader(gz)

	entries := map[string]*tar.Header{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		entries[hdr.Name] = hdr
	}

	if _, ok := entries["repo1/.git/"]; !ok {
		if _, ok := entries["repo1/.git"]; !ok {
			t.Fatalf(".git entry missing from tarball, entries: %v", keys(entries))
		}
	}
	link, ok := entries["repo1/run-link.sh"]
	if !ok {
		t.Fatalf("symlink entry missing from tarball, entries: %v", keys(entries))
	}
	if link.Typeflag != tar.TypeSymlink || link.Linkname != "run.sh" {
		t.Fatalf("symlink entry wrong: typeflag=%v linkname=%q", link.Typeflag, link.Linkname)
	}
	script, ok := entries["repo1/run.sh"]
	if !ok {
		t.Fatalf("run.sh entry missing from tarball")
	}
	if script.Mode&0o100 == 0 {
		t.Fatalf("executable bit lost on run.sh: mode=%o", script.Mode)
	}
	if _, ok := entries["repo1/sub/nested.txt"]; !ok {
		t.Fatalf("nested file missing from tarball")
	}

	// Drain remaining gzip bytes so the hash covers the whole file.
	io.Copy(io.Discard, f)
	sum := hex.EncodeToString(hash.Sum(nil))
	if sum != m.TarballSHA256 {
		t.Fatalf("recomputed sha256 %q != manifest %q", sum, m.TarballSHA256)
	}

	info, err := os.Stat(tb)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != m.TarballBytes {
		t.Fatalf("tarball size %d != manifest TarballBytes %d", info.Size(), m.TarballBytes)
	}
}

func keys(m map[string]*tar.Header) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestArchiveRefusesDirty(t *testing.T) {
	cfg, repo, dir := setupArchiveRepo(t)

	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("dirty\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, err := archiveRepo(context.Background(), cfg, repo)
	if err == nil {
		t.Fatalf("expected an error for a dirty repo")
	}

	if _, err := os.Stat(tarballPath(cfg, "repo1")); !os.IsNotExist(err) {
		t.Fatalf("tarball should not exist, stat err = %v", err)
	}
	if _, err := os.Stat(manifestPath(cfg, "repo1")); !os.IsNotExist(err) {
		t.Fatalf("manifest should not exist, stat err = %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("clone dir should still exist: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "dirty\n" {
		t.Fatalf("dirty edit was lost: got %q", got)
	}
}

func TestArchiveResume(t *testing.T) {
	cfg, repo, _ := setupArchiveRepo(t)

	if _, _, err := archiveRepo(context.Background(), cfg, repo); err != nil {
		t.Fatalf("first archiveRepo: %v", err)
	}

	tb := tarballPath(cfg, "repo1")
	before, err := os.Stat(tb)
	if err != nil {
		t.Fatal(err)
	}

	got, _, err := archiveRepo(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("second archiveRepo: %v", err)
	}
	if got.Status != statusArchived {
		t.Fatalf("status = %q, want archived", got.Status)
	}

	after, err := os.Stat(tb)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("tarball was rewritten: before=%v after=%v", before.ModTime(), after.ModTime())
	}
}

func TestArchiveAdoptsExisting(t *testing.T) {
	cfg, repo, _ := setupArchiveRepo(t)

	if _, _, err := archiveRepo(context.Background(), cfg, repo); err != nil {
		t.Fatalf("initial archiveRepo: %v", err)
	}

	real := cfg.Deps.exec
	invoked := false
	cfg.Deps.exec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		invoked = true
		return real(ctx, dir, name, args...)
	}

	got, _, err := archiveRepo(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("archiveRepo: %v", err)
	}
	if got.Status != statusArchived {
		t.Fatalf("status = %q, want archived", got.Status)
	}
	if invoked {
		t.Fatalf("archiveRepo invoked git when adopting an existing archive")
	}
}

func TestArchiveWithWorktreeRefusesByDefault(t *testing.T) {
	cfg, repo, dir := setupArchiveRepo(t)

	// Simulate "no terminal, cfg.Yes not set" without going through the real
	// confirm function: stub it to answer exactly what
	// defaultConfirmArchiveWithWorktrees would answer in that situation.
	cfg.Deps.confirm = func(cfg config, repoName string, wt []worktreeStatus) (bool, error) {
		return false, nil
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execCommand(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	_, _, err := archiveRepo(context.Background(), cfg, repo)
	if err == nil {
		t.Fatalf("expected an error when the worktree removal is declined")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("clone dir should still exist: %v", err)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree should still exist: %v", err)
	}
	if _, err := os.Stat(tarballPath(cfg, "repo1")); !os.IsNotExist(err) {
		t.Fatalf("tarball should not exist, stat err = %v", err)
	}
}

func TestArchiveWithWorktreeRemovesOnConfirm(t *testing.T) {
	cfg, repo, dir := setupArchiveRepo(t)

	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execCommand(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	var gotRepoName string
	var gotCount int
	cfg.Deps.confirm = func(cfg config, repoName string, wt []worktreeStatus) (bool, error) {
		gotRepoName = repoName
		gotCount = len(wt)
		return true, nil
	}

	if _, _, err := archiveRepo(context.Background(), cfg, repo); err != nil {
		t.Fatalf("archiveRepo: %v", err)
	}
	if gotRepoName != "repo1" || gotCount != 1 {
		t.Fatalf("confirmArchiveWithWorktrees called with repoName=%q count=%d", gotRepoName, gotCount)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("clone dir should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(tarballPath(cfg, "repo1")); err != nil {
		t.Fatalf("tarball missing: %v", err)
	}
}

func TestArchiveWithWorktreeCfgYesSkipsPrompt(t *testing.T) {
	cfg, repo, dir := setupArchiveRepo(t)
	cfg.Yes = true

	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execCommand(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	// cfg.Deps.confirm is already defaultConfirmArchiveWithWorktrees (the
	// zero-value config uses defaultDeps via setupArchiveRepo); cfg.Yes
	// short-circuits it above os.Stdin, so this is exercising the real
	// production seam end to end.

	if _, _, err := archiveRepo(context.Background(), cfg, repo); err != nil {
		t.Fatalf("archiveRepo: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("clone dir should be gone, stat err = %v", err)
	}
}
