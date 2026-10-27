package vcs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swanysimon/gh-workspace/internal/execx"
)

const testTimeout = 60 * time.Second

// requireJJ skips a test that drives the real jj binary when jj is not
// installed, and pins jj's configuration so a developer's own settings
// (advance-bookmarks in particular) can't change what is being tested.
// Duplicated from jjcli's own test helper rather than exported from there:
// a test helper is not part of that package's API.
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

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := execx.Run(context.Background(), dir, "git", args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func jj(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := execx.Run(context.Background(), dir, "jj", args...)
	if err != nil {
		t.Fatalf("jj %v: %v", args, err)
	}
	return string(out)
}

// initOrigin creates a git repo with one commit on "main" plus a second
// branch, to clone from.
func initOrigin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "-b", "main")
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "config", "user.email", "test@example.com")
	writeFile(t, filepath.Join(dir, "file.txt"), "v1\n")
	git(t, dir, "add", "file.txt")
	git(t, dir, "commit", "--quiet", "-m", "initial")
	git(t, dir, "branch", "existing")
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// clone makes a clone of a fresh origin with kind, and returns its path.
func clone(t *testing.T, kind Kind) string {
	t.Helper()
	root := t.TempDir()
	dest := filepath.Join(root, "repo")
	err := Clone(context.Background(), execx.Run, testTimeout, kind, "file://"+initOrigin(t), dest, filepath.Join(root, ".tmp-repo"))
	if err != nil {
		t.Fatalf("Clone(%s): %v", kind, err)
	}
	return dest
}

func TestDetect(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, markers ...string) string {
		dir := filepath.Join(root, name)
		for _, m := range markers {
			if err := os.MkdirAll(filepath.Join(dir, m), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	tests := []struct {
		name string
		dir  string
		want Kind
	}{
		{"colocated jj clone", mk("both", ".git", ".jj"), JJ},
		{"jj workspace", mk("wsonly", ".jj"), JJ},
		{"git clone", mk("gitonly", ".git"), Git},
		{"not a repo", mk("empty"), Git},
		{"missing", filepath.Join(root, "nope"), Git},
	}
	for _, tt := range tests {
		if got := Detect(tt.dir); got != tt.want {
			t.Errorf("%s: Detect = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestResolve(t *testing.T) {
	root := t.TempDir()
	jjClone := filepath.Join(root, "jj")
	gitClone := filepath.Join(root, "git")
	for dir, marker := range map[string]string{jjClone: ".jj", gitClone: ".git"} {
		if err := os.MkdirAll(filepath.Join(dir, marker), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(root, "missing")

	tests := []struct {
		name       string
		dir        string
		configured Kind
		wantKind   Kind
		wantAdopt  bool
	}{
		// Detection beats configuration, so a "vcs" change can never
		// invalidate what is already on disk.
		{"jj clone under configured git", jjClone, Git, JJ, false},
		{"jj clone under configured jj", jjClone, JJ, JJ, false},
		{"git clone under configured git", gitClone, Git, Git, false},
		{"git clone under configured jj adopts", gitClone, JJ, JJ, true},
		{"nothing on disk under configured jj", missing, JJ, JJ, false},
		{"nothing on disk under configured git", missing, Git, Git, false},
	}
	for _, tt := range tests {
		kind, adopt := Resolve(tt.dir, tt.configured)
		if kind != tt.wantKind || adopt != tt.wantAdopt {
			t.Errorf("%s: Resolve = (%q, %v), want (%q, %v)", tt.name, kind, adopt, tt.wantKind, tt.wantAdopt)
		}
	}
}

func TestRepoRootFindsTheCloneAWorktreeBelongsTo(t *testing.T) {
	dir := clone(t, Git)
	path := filepath.Join(t.TempDir(), "wt")
	if _, err := WorktreeAdd(context.Background(), execx.Run, testTimeout, dir, path, "existing", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	got, err := RepoRoot(path)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	if resolvePath(got) != resolvePath(dir) {
		t.Errorf("RepoRoot = %q, want %q", got, dir)
	}
}

func TestRepoRootFindsTheCloneAJJWorkspaceBelongsTo(t *testing.T) {
	requireJJ(t)
	dir := clone(t, JJ)
	path := filepath.Join(t.TempDir(), "wt")
	if _, err := WorktreeAdd(context.Background(), execx.Run, testTimeout, dir, path, "feature", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	// The workspace's .jj/repo is a *relative* path, so this is the case
	// that would break if it were read as-is instead of resolved against
	// the workspace's own .jj.
	got, err := RepoRoot(path)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	if resolvePath(got) != resolvePath(dir) {
		t.Errorf("RepoRoot = %q, want %q", got, dir)
	}
}

func TestRepoRootRejectsACloneAndAPlainDirectory(t *testing.T) {
	requireJJ(t)
	for _, kind := range []Kind{Git, JJ} {
		dir := clone(t, kind)
		_, err := RepoRoot(dir)
		if err == nil || !strings.Contains(err.Error(), "not a worktree") {
			t.Errorf("%s clone: got err = %v, want a not-a-worktree error", kind, err)
		}
	}
	if _, err := RepoRoot(t.TempDir()); err == nil {
		t.Error("a plain directory should not resolve to a repo")
	}
}

func TestWorktreeAddChecksOutAnExistingBranch(t *testing.T) {
	dir := clone(t, Git)
	path := filepath.Join(t.TempDir(), "wt")

	note, err := WorktreeAdd(context.Background(), execx.Run, testTimeout, dir, path, "existing", "main")
	if err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if note != "" {
		t.Errorf("an existing branch should report nothing, got %q", note)
	}
	if got := strings.TrimSpace(git(t, path, "symbolic-ref", "--short", "HEAD")); got != "existing" {
		t.Errorf("worktree is on %q, want \"existing\"", got)
	}
}

func TestWorktreeAddCreatesANewBranchFromTheDefault(t *testing.T) {
	dir := clone(t, Git)
	path := filepath.Join(t.TempDir(), "wt")

	note, err := WorktreeAdd(context.Background(), execx.Run, testTimeout, dir, path, "brand-new", "main")
	if err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if note != `created new branch "brand-new" from origin/main` {
		t.Errorf("note = %q", note)
	}
	// --no-track: the first "git push -u" is what decides the upstream.
	if out, err := execx.Run(context.Background(), path, "git", "rev-parse", "--abbrev-ref", "brand-new@{upstream}"); err == nil {
		t.Errorf("new branch should have no upstream, got %q", strings.TrimSpace(string(out)))
	}
}

func TestWorktreeAddRejectsAnInvalidBranchName(t *testing.T) {
	dir := clone(t, Git)
	_, err := WorktreeAdd(context.Background(), execx.Run, testTimeout, dir, filepath.Join(t.TempDir(), "wt"), "bad branch", "main")
	if err == nil || !strings.Contains(err.Error(), "not a valid branch name") {
		t.Fatalf("got err = %v, want an invalid-branch-name error", err)
	}
}

func TestWorktreeAddOnJJCreatesNoBookmark(t *testing.T) {
	requireJJ(t)
	dir := clone(t, JJ)
	path := filepath.Join(t.TempDir(), "wt")

	note, err := WorktreeAdd(context.Background(), execx.Run, testTimeout, dir, path, "feature/x", "main")
	if err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	// The whole point of the jj semantics: a branch argument picks where
	// the work starts, and naming a bookmark stays the user's call.
	if !strings.Contains(note, `jj bookmark create feature/x -r @`) {
		t.Errorf("note should name the bookmark command, got %q", note)
	}
	if !strings.Contains(note, "main@origin") {
		t.Errorf("note should say where the workspace starts, got %q", note)
	}
	if got := jj(t, dir, "bookmark", "list", "--all-remotes", "-T", `name ++ "\n"`); strings.Contains(got, "feature/x") {
		t.Errorf("no bookmark should have been created, bookmarks are:\n%s", got)
	}
	// "/" is not a workspace-name separator, so it is flattened the same
	// way the worktree path template flattens it.
	if got := jj(t, dir, "workspace", "list"); !strings.Contains(got, "feature-x") {
		t.Errorf("workspace should be named feature-x, got:\n%s", got)
	}
}

func TestWorktreeAddOnJJStartsAtAnExistingBookmark(t *testing.T) {
	requireJJ(t)
	dir := clone(t, JJ)
	path := filepath.Join(t.TempDir(), "wt")

	note, err := WorktreeAdd(context.Background(), execx.Run, testTimeout, dir, path, "existing", "main")
	if err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if !strings.Contains(note, "existing@origin") {
		t.Errorf("note should name the branch's own bookmark, got %q", note)
	}
}

func TestWorktreesReportsPathsAndLiveDirtiness(t *testing.T) {
	requireJJ(t)
	for _, kind := range []Kind{Git, JJ} {
		t.Run(string(kind), func(t *testing.T) {
			dir := clone(t, kind)
			ctx := context.Background()

			got, err := Worktrees(ctx, execx.Run, testTimeout, dir)
			if err != nil {
				t.Fatalf("Worktrees: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("a fresh clone has no other worktrees, got %v", got)
			}

			path := filepath.Join(t.TempDir(), "wt")
			if _, err := WorktreeAdd(ctx, execx.Run, testTimeout, dir, path, "existing", "main"); err != nil {
				t.Fatalf("WorktreeAdd: %v", err)
			}

			got, err = Worktrees(ctx, execx.Run, testTimeout, dir)
			if err != nil {
				t.Fatalf("Worktrees after add: %v", err)
			}
			if len(got) != 1 || resolvePath(got[0].Path) != resolvePath(path) {
				t.Fatalf("got %v, want one worktree at %s", got, path)
			}
			if got[0].Dirty {
				t.Error("a fresh worktree should not be dirty")
			}

			paths, err := WorktreePaths(ctx, execx.Run, testTimeout, dir)
			if err != nil {
				t.Fatalf("WorktreePaths: %v", err)
			}
			if len(paths) != 1 || paths[0].Path != got[0].Path || paths[0].Name != got[0].Name {
				t.Fatalf("WorktreePaths = %v, want the same worktree Worktrees reported, %v", paths, got)
			}

			// Dirtiness is measured in the worktree itself, not read off
			// whatever the repo last recorded.
			writeFile(t, filepath.Join(path, "scratch.txt"), "work\n")
			got, err = Worktrees(ctx, execx.Run, testTimeout, dir)
			if err != nil {
				t.Fatalf("Worktrees after edit: %v", err)
			}
			if len(got) != 1 || !got[0].Dirty {
				t.Fatalf("got %v, want one dirty worktree", got)
			}
		})
	}
}

func TestWorktreeRemove(t *testing.T) {
	requireJJ(t)
	for _, kind := range []Kind{Git, JJ} {
		t.Run(string(kind), func(t *testing.T) {
			dir := clone(t, kind)
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "wt")
			if _, err := WorktreeAdd(ctx, execx.Run, testTimeout, dir, path, "existing", "main"); err != nil {
				t.Fatalf("WorktreeAdd: %v", err)
			}

			if err := WorktreeRemove(ctx, execx.Run, testTimeout, dir, path, false); err != nil {
				t.Fatalf("WorktreeRemove: %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("worktree directory should be gone, stat err = %v", err)
			}
			got, err := Worktrees(ctx, execx.Run, testTimeout, dir)
			if err != nil {
				t.Fatalf("Worktrees: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("worktree still attached: %v", got)
			}
		})
	}
}

func TestWorktreeRemoveRefusesADirtyWorktreeWithoutForce(t *testing.T) {
	requireJJ(t)
	for _, kind := range []Kind{Git, JJ} {
		t.Run(string(kind), func(t *testing.T) {
			dir := clone(t, kind)
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "wt")
			if _, err := WorktreeAdd(ctx, execx.Run, testTimeout, dir, path, "existing", "main"); err != nil {
				t.Fatalf("WorktreeAdd: %v", err)
			}
			writeFile(t, filepath.Join(path, "file.txt"), "uncommitted\n")

			if err := WorktreeRemove(ctx, execx.Run, testTimeout, dir, path, false); err == nil {
				t.Fatal("expected a refusal for a dirty worktree")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("a refused removal must leave the worktree alone: %v", err)
			}

			if err := WorktreeRemove(ctx, execx.Run, testTimeout, dir, path, true); err != nil {
				t.Fatalf("WorktreeRemove --force: %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("worktree directory should be gone, stat err = %v", err)
			}
		})
	}
}

// The jj path does the directory removal itself, so it must never act on a
// path that isn't one of this clone's own workspaces.
func TestWorktreeRemoveRejectsAPathThatIsNotAWorkspace(t *testing.T) {
	requireJJ(t)
	dir := clone(t, JJ)
	bystander := filepath.Join(t.TempDir(), "not-a-workspace")
	if err := os.MkdirAll(bystander, 0o700); err != nil {
		t.Fatal(err)
	}

	err := WorktreeRemove(context.Background(), execx.Run, testTimeout, dir, bystander, true)
	if err == nil || !strings.Contains(err.Error(), "not a jj workspace") {
		t.Fatalf("got err = %v, want a not-a-workspace error", err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("the directory must be left alone: %v", err)
	}
}

func TestAdoptMakesAGitCloneJJ(t *testing.T) {
	requireJJ(t)
	dir := clone(t, Git)

	if Detect(dir) != Git {
		t.Fatalf("precondition: Detect = %q", Detect(dir))
	}
	if _, err := Adopt(context.Background(), execx.Run, testTimeout, dir, "main"); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if Detect(dir) != JJ {
		t.Error("an adopted clone should detect as jj")
	}
	// Adoption must not cost the clone its git half: the read-only git
	// helpers and every ".git exists" check still run against it.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf("adopted clone lost .git: %v", err)
	}
}

func TestFetchAndUpdateWorkingCopyDispatch(t *testing.T) {
	requireJJ(t)
	for _, kind := range []Kind{Git, JJ} {
		t.Run(string(kind), func(t *testing.T) {
			origin := initOrigin(t)
			root := t.TempDir()
			dir := filepath.Join(root, "repo")
			ctx := context.Background()
			if err := Clone(ctx, execx.Run, testTimeout, kind, "file://"+origin, dir, filepath.Join(root, ".tmp")); err != nil {
				t.Fatalf("Clone: %v", err)
			}

			writeFile(t, filepath.Join(origin, "file.txt"), "v2\n")
			git(t, origin, "commit", "--quiet", "-am", "second")

			if err := Fetch(ctx, execx.Run, testTimeout, dir); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			warning, dirty, err := UpdateWorkingCopy(ctx, execx.Run, testTimeout, dir, "main")
			if err != nil {
				t.Fatalf("UpdateWorkingCopy: %v", err)
			}
			if warning != "" || dirty {
				t.Fatalf("warning = %q, dirty = %v", warning, dirty)
			}
			b, err := os.ReadFile(filepath.Join(dir, "file.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != "v2\n" {
				t.Errorf("working copy not updated: file.txt = %q", b)
			}
		})
	}
}

// Moving a clone breaks every working copy attached to it, which is what a
// renamed-upstream repo does. Both kinds are repairable without touching
// the user's work, so neither has to be destroyed to survive a rename.
func TestRepairWorktreesAfterTheCloneMoves(t *testing.T) {
	requireJJ(t)
	for _, kind := range []Kind{Git, JJ} {
		t.Run(string(kind), func(t *testing.T) {
			dir := clone(t, kind)
			ctx := context.Background()
			wt := filepath.Join(t.TempDir(), "wt")
			if _, err := WorktreeAdd(ctx, execx.Run, testTimeout, dir, wt, "existing", "main"); err != nil {
				t.Fatalf("WorktreeAdd: %v", err)
			}

			moved := filepath.Join(filepath.Dir(dir), "renamed")
			if err := os.Rename(dir, moved); err != nil {
				t.Fatal(err)
			}

			// Precondition: the working copy really is broken by the move,
			// so a passing repair below isn't vacuous. Both kinds fail the
			// same way -- the VCS can't find its repo from the worktree.
			if _, err := IsDirty(ctx, execx.Run, testTimeout, wt); err == nil {
				t.Fatal("precondition: the working copy should be broken after the move")
			}

			if err := RepairWorktrees(ctx, execx.Run, testTimeout, moved); err != nil {
				t.Fatalf("RepairWorktrees: %v", err)
			}

			// The working copy works again, and the clone still knows it.
			if _, err := IsDirty(ctx, execx.Run, testTimeout, wt); err != nil {
				t.Errorf("working copy still broken after repair: %v", err)
			}
			got, err := Worktrees(ctx, execx.Run, testTimeout, moved)
			if err != nil {
				t.Fatalf("Worktrees: %v", err)
			}
			if len(got) != 1 || resolvePath(got[0].Path) != resolvePath(wt) {
				t.Errorf("got %v, want one worktree at %s", got, wt)
			}
		})
	}
}
