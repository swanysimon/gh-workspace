package archive

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
	"time"

	"github.com/swanysimon/gh-workspace/internal/execx"
	"github.com/swanysimon/gh-workspace/internal/gitcli"
	"github.com/swanysimon/gh-workspace/internal/store"
)

const testTimeout = 30 * time.Second

// initTestRepo creates a real git repo in a fresh temp dir with one commit
// on "main" and returns its path. Duplicated from internal/gitcli's own
// test helper of the same name and shape, rather than shared across
// package boundaries for a test-only fixture.
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

// setupArchiveRepo clones a fresh real repo and adds a subdirectory, a
// symlink and an executable file (for the tarball entry-set assertions),
// committing them so the working tree is clean, which Archive requires.
func setupArchiveRepo(t *testing.T) (repo Repo, dir, archivesDir string, clone func(context.Context) error) {
	t.Helper()
	origin := initTestRepo(t)
	root := t.TempDir()
	reposD := filepath.Join(root, "repos")
	archivesD := filepath.Join(root, "archives")
	if err := os.MkdirAll(reposD, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archivesD, 0o700); err != nil {
		t.Fatal(err)
	}

	dir = filepath.Join(reposD, "repo1")
	repo = Repo{
		Name:          "repo1",
		Owner:         "testorg",
		NameWithOwner: "testorg/repo1",
		URL:           "file://" + origin,
		DefaultBranch: "main",
	}
	clone = func(ctx context.Context) error {
		return gitcli.Clone(ctx, execx.Run, testTimeout, repo.URL, dir, filepath.Join(reposD, ".tmp-repo1"))
	}
	if err := clone(context.Background()); err != nil {
		t.Fatalf("clone: %v", err)
	}

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
	for _, args := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"add", "-A"},
		{"commit", "--quiet", "-m", "add fixtures"},
	} {
		if _, err := execx.Run(context.Background(), dir, "git", args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return repo, dir, archivesD, clone
}

// noConfirm is the ConfirmFunc Archive never needs to call in these tests
// (there's no linked worktree), so a call reaching it at all is itself a
// bug worth failing loudly on.
func noConfirm(repoName string, worktrees []WorktreeStatus) (bool, error) {
	panic("confirm should not be called: no linked worktrees in this test")
}

func TestArchive(t *testing.T) {
	repo, dir, archivesDir, clone := setupArchiveRepo(t)

	got, notes, err := Archive(context.Background(), execx.Run, testTimeout, noConfirm, repo, dir, archivesDir, clone)
	if err != nil {
		t.Fatalf("Archive: %v (notes=%v)", err, notes)
	}
	if got.Status != store.StatusArchived {
		t.Fatalf("status = %q, want archived", got.Status)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected clone dir to be gone, stat err = %v", err)
	}

	tb := TarballPath(archivesDir, "repo1")
	mp := ManifestPath(archivesDir, "repo1")
	m, ok := ReadManifest(mp)
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
	repo, dir, archivesDir, clone := setupArchiveRepo(t)

	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("dirty\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, err := Archive(context.Background(), execx.Run, testTimeout, noConfirm, repo, dir, archivesDir, clone)
	if err == nil {
		t.Fatalf("expected an error for a dirty repo")
	}

	if _, err := os.Stat(TarballPath(archivesDir, "repo1")); !os.IsNotExist(err) {
		t.Fatalf("tarball should not exist, stat err = %v", err)
	}
	if _, err := os.Stat(ManifestPath(archivesDir, "repo1")); !os.IsNotExist(err) {
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
	repo, dir, archivesDir, clone := setupArchiveRepo(t)

	if _, _, err := Archive(context.Background(), execx.Run, testTimeout, noConfirm, repo, dir, archivesDir, clone); err != nil {
		t.Fatalf("first Archive: %v", err)
	}

	tb := TarballPath(archivesDir, "repo1")
	before, err := os.Stat(tb)
	if err != nil {
		t.Fatal(err)
	}

	got, _, err := Archive(context.Background(), execx.Run, testTimeout, noConfirm, repo, dir, archivesDir, clone)
	if err != nil {
		t.Fatalf("second Archive: %v", err)
	}
	if got.Status != store.StatusArchived {
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
	repo, dir, archivesDir, clone := setupArchiveRepo(t)

	if _, _, err := Archive(context.Background(), execx.Run, testTimeout, noConfirm, repo, dir, archivesDir, clone); err != nil {
		t.Fatalf("initial Archive: %v", err)
	}

	invoked := false
	spyExec := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		invoked = true
		return execx.Run(ctx, dir, name, args...)
	}

	got, _, err := Archive(context.Background(), spyExec, testTimeout, noConfirm, repo, dir, archivesDir, clone)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if got.Status != store.StatusArchived {
		t.Fatalf("status = %q, want archived", got.Status)
	}
	if invoked {
		t.Fatalf("Archive invoked git when adopting an existing archive")
	}
}

func TestArchiveWithWorktreeRefusesByDefault(t *testing.T) {
	repo, dir, archivesDir, clone := setupArchiveRepo(t)

	// Simulate "no terminal, yes=false" without going through the real
	// confirm function: stub it to answer exactly what
	// DefaultConfirm(false) would answer in that situation.
	confirm := func(repoName string, wt []WorktreeStatus) (bool, error) {
		return false, nil
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execx.Run(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	_, _, err := Archive(context.Background(), execx.Run, testTimeout, confirm, repo, dir, archivesDir, clone)
	if err == nil {
		t.Fatalf("expected an error when the worktree removal is declined")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("clone dir should still exist: %v", err)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree should still exist: %v", err)
	}
	if _, err := os.Stat(TarballPath(archivesDir, "repo1")); !os.IsNotExist(err) {
		t.Fatalf("tarball should not exist, stat err = %v", err)
	}
}

func TestArchiveWithWorktreeRemovesOnConfirm(t *testing.T) {
	repo, dir, archivesDir, clone := setupArchiveRepo(t)

	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execx.Run(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	var gotRepoName string
	var gotCount int
	confirm := func(repoName string, wt []WorktreeStatus) (bool, error) {
		gotRepoName = repoName
		gotCount = len(wt)
		return true, nil
	}

	if _, _, err := Archive(context.Background(), execx.Run, testTimeout, confirm, repo, dir, archivesDir, clone); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if gotRepoName != "repo1" || gotCount != 1 {
		t.Fatalf("confirm called with repoName=%q count=%d", gotRepoName, gotCount)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("clone dir should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(TarballPath(archivesDir, "repo1")); err != nil {
		t.Fatalf("tarball missing: %v", err)
	}
}

func TestArchiveWithWorktreeYesSkipsPrompt(t *testing.T) {
	repo, dir, archivesDir, clone := setupArchiveRepo(t)

	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := execx.Run(context.Background(), dir, "git", "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	// DefaultConfirm(true) short-circuits above os.Stdin, so this exercises
	// the real production seam end to end, same as the yes=false case
	// above exercises a stub standing in for DefaultConfirm(false).
	if _, _, err := Archive(context.Background(), execx.Run, testTimeout, DefaultConfirm(true), repo, dir, archivesDir, clone); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("clone dir should be gone, stat err = %v", err)
	}
}
