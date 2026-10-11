package jjcli

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
// installed, and isolates it from the developer's own jj configuration.
// That isolation is load-bearing, not hygiene: settings like
// advance-bookmarks change what "jj new" does to bookmarks, so a test run
// on a configured machine would otherwise not be testing the same thing CI
// does.
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

// initOrigin creates a real git repo with one commit on "main", to clone
// from. jj clones it over a file:// URL, exactly as gitcli's own tests do.
func initOrigin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if _, err := execx.Run(context.Background(), dir, "git", args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	run("init", "--quiet", "-b", "main")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")
	writeFile(t, filepath.Join(dir, "file.txt"), "v1\n")
	run("add", "file.txt")
	run("commit", "--quiet", "-m", "initial")
	return dir
}

func commitToOrigin(t *testing.T, origin, content string) {
	t.Helper()
	writeFile(t, filepath.Join(origin, "file.txt"), content)
	for _, args := range [][]string{{"add", "file.txt"}, {"commit", "--quiet", "-m", "second"}} {
		if _, err := execx.Run(context.Background(), origin, "git", args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// cloneForTest clones origin into a fresh directory with the real jj.
func cloneForTest(t *testing.T, origin string) string {
	t.Helper()
	root := t.TempDir()
	dest := filepath.Join(root, "repo")
	if err := Clone(context.Background(), execx.Run, testTimeout, "file://"+origin, dest, filepath.Join(root, ".tmp-repo")); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	return dest
}

func TestCloneIsColocated(t *testing.T) {
	requireJJ(t)
	root := t.TempDir()
	dest := filepath.Join(root, "repo")
	tmp := filepath.Join(root, ".tmp-repo")

	if err := Clone(context.Background(), execx.Run, testTimeout, "file://"+initOrigin(t), dest, tmp); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	// Both halves matter: .jj is what Detect keys off, and .git is what
	// every read-only git helper and every ".git exists" check needs.
	for _, name := range []string{".jj", ".git"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Errorf("clone is missing %s: %v", name, err)
		}
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("tmp dir should be gone after a successful clone, stat err = %v", err)
	}
}

func TestCloneFailureCleansUpTmpAndDoesNotRename(t *testing.T) {
	requireJJ(t)
	root := t.TempDir()
	dest := filepath.Join(root, "repo")
	tmp := filepath.Join(root, ".tmp-repo")

	if err := Clone(context.Background(), execx.Run, testTimeout, "file:///does/not/exist", dest, tmp); err == nil {
		t.Fatal("expected an error cloning a nonexistent repo")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("tmp dir should be cleaned up after a failed clone, stat err = %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("dest should not exist after a failed clone, stat err = %v", err)
	}
}

func TestIsDirty(t *testing.T) {
	requireJJ(t)
	dir := cloneForTest(t, initOrigin(t))
	ctx := context.Background()

	dirty, err := IsDirty(ctx, execx.Run, testTimeout, dir)
	if err != nil {
		t.Fatalf("IsDirty: %v", err)
	}
	if dirty {
		t.Error("a fresh clone should not be dirty")
	}

	writeFile(t, filepath.Join(dir, "file.txt"), "edited\n")
	dirty, err = IsDirty(ctx, execx.Run, testTimeout, dir)
	if err != nil {
		t.Fatalf("IsDirty after edit: %v", err)
	}
	if !dirty {
		t.Error("an edited working copy should be dirty")
	}
}

// A described but empty @ is dirty too: nothing was changed yet, but
// someone has started a change here and moving the working copy would
// throw their description away.
func TestIsDirtyWithDescribedEmptyChange(t *testing.T) {
	requireJJ(t)
	dir := cloneForTest(t, initOrigin(t))
	ctx := context.Background()

	if _, err := Run(ctx, execx.Run, testTimeout, dir, "describe", "-m", "work in progress"); err != nil {
		t.Fatalf("jj describe: %v", err)
	}

	dirty, err := IsDirty(ctx, execx.Run, testTimeout, dir)
	if err != nil {
		t.Fatalf("IsDirty: %v", err)
	}
	if !dirty {
		t.Error("a described working-copy commit should be dirty")
	}
}

func TestUpdateWorkingCopyMovesOntoDefaultBranchThenIsIdempotent(t *testing.T) {
	requireJJ(t)
	origin := initOrigin(t)
	dir := cloneForTest(t, origin)
	ctx := context.Background()

	commitToOrigin(t, origin, "v2\n")
	if err := Fetch(ctx, execx.Run, testTimeout, dir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	warning, dirty, err := UpdateWorkingCopy(ctx, execx.Run, testTimeout, dir, "main")
	if err != nil {
		t.Fatalf("UpdateWorkingCopy: %v", err)
	}
	if warning != "" || dirty {
		t.Fatalf("UpdateWorkingCopy on a clean clone: warning = %q, dirty = %v", warning, dirty)
	}
	if got := readFile(t, filepath.Join(dir, "file.txt")); got != "v2\n" {
		t.Errorf("working copy was not updated: file.txt = %q", got)
	}

	before := resolveOne(t, dir, "@")
	warning, dirty, err = UpdateWorkingCopy(ctx, execx.Run, testTimeout, dir, "main")
	if err != nil {
		t.Fatalf("second UpdateWorkingCopy: %v", err)
	}
	if warning != "" || dirty {
		t.Fatalf("second UpdateWorkingCopy: warning = %q, dirty = %v", warning, dirty)
	}
	// Without the "@- is already the target" guard this would stack a
	// fresh empty change on every single sync run.
	if after := resolveOne(t, dir, "@"); after != before {
		t.Errorf("second call created a new working-copy commit: %s -> %s", before, after)
	}
}

func TestUpdateWorkingCopyLeavesADirtyWorkingCopyAlone(t *testing.T) {
	requireJJ(t)
	origin := initOrigin(t)
	dir := cloneForTest(t, origin)
	ctx := context.Background()

	commitToOrigin(t, origin, "v2\n")
	if err := Fetch(ctx, execx.Run, testTimeout, dir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	writeFile(t, filepath.Join(dir, "mine.txt"), "local work\n")

	warning, dirty, err := UpdateWorkingCopy(ctx, execx.Run, testTimeout, dir, "main")
	if err != nil {
		t.Fatalf("UpdateWorkingCopy: %v", err)
	}
	if !dirty || warning == "" {
		t.Fatalf("expected a dirty warning, got warning = %q, dirty = %v", warning, dirty)
	}
	if got := readFile(t, filepath.Join(dir, "file.txt")); got != "v1\n" {
		t.Errorf("a dirty working copy must not be updated: file.txt = %q", got)
	}
}

func TestUpdateWorkingCopyWithoutADefaultBranchDoesNothing(t *testing.T) {
	requireJJ(t)
	dir := cloneForTest(t, initOrigin(t))

	warning, dirty, err := UpdateWorkingCopy(context.Background(), execx.Run, testTimeout, dir, "")
	if err != nil || warning != "" || dirty {
		t.Fatalf("got warning = %q, dirty = %v, err = %v", warning, dirty, err)
	}
}

func TestUpdateWorkingCopyWarnsWhenTheRemoteBookmarkIsMissing(t *testing.T) {
	requireJJ(t)
	dir := cloneForTest(t, initOrigin(t))

	warning, dirty, err := UpdateWorkingCopy(context.Background(), execx.Run, testTimeout, dir, "nonexistent")
	if err != nil {
		t.Fatalf("UpdateWorkingCopy: %v", err)
	}
	if dirty || !strings.Contains(warning, "nonexistent@origin") {
		t.Fatalf("got warning = %q, dirty = %v", warning, dirty)
	}
}

func TestResolveMissingRevsetIsNotAnError(t *testing.T) {
	requireJJ(t)
	dir := cloneForTest(t, initOrigin(t))

	ids, err := Resolve(context.Background(), execx.Run, testTimeout, dir, "no-such-bookmark@origin")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected no commits, got %v", ids)
	}
}

func TestWorkspacesExcludesTheRepoItself(t *testing.T) {
	requireJJ(t)
	dir := cloneForTest(t, initOrigin(t))
	ctx := context.Background()

	got, err := Workspaces(ctx, execx.Run, testTimeout, dir)
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a fresh clone should report no other workspaces, got %v", got)
	}

	path := filepath.Join(t.TempDir(), "wt")
	if err := WorkspaceAdd(ctx, execx.Run, testTimeout, dir, "feature", path, "main@origin"); err != nil {
		t.Fatalf("WorkspaceAdd: %v", err)
	}

	got, err = Workspaces(ctx, execx.Run, testTimeout, dir)
	if err != nil {
		t.Fatalf("Workspaces after add: %v", err)
	}
	if len(got) != 1 || got[0].Name != "feature" {
		t.Fatalf("got %v, want one workspace named \"feature\"", got)
	}
	if resolvePath(got[0].Path) != resolvePath(path) {
		t.Errorf("got path %q, want %q", got[0].Path, path)
	}
	// A jj workspace is not a git repo: no .git, which is why the git
	// implementation cannot serve these.
	if _, err := os.Stat(filepath.Join(path, ".jj")); err != nil {
		t.Errorf("workspace is missing .jj: %v", err)
	}
}

func TestWorkspaceForgetLeavesTheDirectoryBehind(t *testing.T) {
	requireJJ(t)
	dir := cloneForTest(t, initOrigin(t))
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wt")

	if err := WorkspaceAdd(ctx, execx.Run, testTimeout, dir, "feature", path, ""); err != nil {
		t.Fatalf("WorkspaceAdd: %v", err)
	}
	if err := WorkspaceForget(ctx, execx.Run, testTimeout, dir, "feature"); err != nil {
		t.Fatalf("WorkspaceForget: %v", err)
	}

	got, err := Workspaces(ctx, execx.Run, testTimeout, dir)
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("forgotten workspace still listed: %v", got)
	}
	// Documented jj behavior the callers depend on: forgetting is not
	// deleting, so removing the directory stays the caller's explicit step.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("forget should leave the directory on disk: %v", err)
	}
}

func TestAdoptColocatesAnExistingGitClone(t *testing.T) {
	requireJJ(t)
	origin := initOrigin(t)
	dir := filepath.Join(t.TempDir(), "repo")
	if _, err := execx.Run(context.Background(), "", "git", "clone", "--quiet", "file://"+origin, dir); err != nil {
		t.Fatalf("git clone: %v", err)
	}

	warning, err := Adopt(context.Background(), execx.Run, testTimeout, dir, "main")
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if warning != "" {
		t.Errorf("unexpected warning: %q", warning)
	}
	if _, err := os.Stat(filepath.Join(dir, ".jj")); err != nil {
		t.Fatalf("adopted clone is missing .jj: %v", err)
	}

	// Tracking the default branch is what makes a later Fetch advance it,
	// exactly as it would in a cloned repo.
	commitToOrigin(t, origin, "v2\n")
	if err := Fetch(context.Background(), execx.Run, testTimeout, dir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := resolveOne(t, dir, "main@origin"); got != resolveOne(t, dir, "main") {
		t.Error("main did not follow main@origin after adoption, so the bookmark was not tracked")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func resolveOne(t *testing.T, dir, revset string) string {
	t.Helper()
	ids, err := Resolve(context.Background(), execx.Run, testTimeout, dir, revset)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", revset, err)
	}
	if len(ids) != 1 {
		t.Fatalf("Resolve(%q) = %v, want exactly one commit", revset, ids)
	}
	return ids[0]
}

// recorder is the fake execx.Exec used by the argv and parse-error tests
// below, which need no jj binary at all.
type recorder struct {
	calls [][]string
	out   map[string]string // joined args prefix -> stdout
	err   error
}

func (r *recorder) exec(_ context.Context, _, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.err != nil {
		return nil, r.err
	}
	for prefix, out := range r.out {
		if strings.HasPrefix(strings.Join(args, " "), prefix) {
			return []byte(out), nil
		}
	}
	return nil, nil
}

func TestArgv(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func(r *recorder) error
		want []string
	}{
		{
			name: "Fetch",
			call: func(r *recorder) error { return Fetch(ctx, r.exec, testTimeout, "/d") },
			want: []string{"jj", "git", "fetch", "--remote", "origin"},
		},
		{
			name: "WorkspaceAdd with a revset",
			call: func(r *recorder) error {
				return WorkspaceAdd(ctx, r.exec, testTimeout, "/d", "feat-x", "/w", "main@origin")
			},
			want: []string{"jj", "workspace", "add", "--name", "feat-x", "-r", "main@origin", "--", "/w"},
		},
		{
			name: "WorkspaceAdd without a revset",
			call: func(r *recorder) error {
				return WorkspaceAdd(ctx, r.exec, testTimeout, "/d", "feat-x", "/w", "")
			},
			want: []string{"jj", "workspace", "add", "--name", "feat-x", "--", "/w"},
		},
		{
			name: "WorkspaceForget",
			call: func(r *recorder) error { return WorkspaceForget(ctx, r.exec, testTimeout, "/d", "feat-x") },
			want: []string{"jj", "workspace", "forget", "feat-x"},
		},
		{
			name: "SetRemoteURL",
			call: func(r *recorder) error {
				return SetRemoteURL(ctx, r.exec, testTimeout, "/d", "git@github.com:o/r.git")
			},
			want: []string{"jj", "git", "remote", "set-url", "origin", "git@github.com:o/r.git"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recorder{}
			if err := tt.call(r); err != nil {
				t.Fatalf("call: %v", err)
			}
			if len(r.calls) != 1 {
				t.Fatalf("got %d calls, want 1: %v", len(r.calls), r.calls)
			}
			if strings.Join(r.calls[0], "\x1f") != strings.Join(tt.want, "\x1f") {
				t.Errorf("got %v, want %v", r.calls[0], tt.want)
			}
		})
	}
}

func TestAdoptArgv(t *testing.T) {
	r := &recorder{}
	if _, err := Adopt(context.Background(), r.exec, testTimeout, "/d", "trunk"); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	want := [][]string{
		{"jj", "git", "init", "--colocate"},
		{"jj", "bookmark", "track", "trunk@origin"},
	}
	if len(r.calls) != len(want) {
		t.Fatalf("got %v, want %v", r.calls, want)
	}
	for i := range want {
		if strings.Join(r.calls[i], "\x1f") != strings.Join(want[i], "\x1f") {
			t.Errorf("call %d: got %v, want %v", i, r.calls[i], want[i])
		}
	}
}

func TestWorkspacesRejectsAWorkspaceWithNoRecordedRoot(t *testing.T) {
	r := &recorder{out: map[string]string{
		"workspace list": "default\x00/d\nstale\x00\n",
	}}
	_, err := Workspaces(context.Background(), r.exec, testTimeout, "/d")
	if err == nil || !strings.Contains(err.Error(), "no recorded root path") {
		t.Fatalf("got err = %v, want a no-recorded-root error", err)
	}
}

func TestWorkspacesRejectsMalformedOutput(t *testing.T) {
	r := &recorder{out: map[string]string{"workspace list": "no-separator-here\n"}}
	_, err := Workspaces(context.Background(), r.exec, testTimeout, "/d")
	if err == nil || !strings.Contains(err.Error(), "unexpected jj workspace list output") {
		t.Fatalf("got err = %v, want a malformed-output error", err)
	}
}

func TestIsDirtyRejectsMalformedOutput(t *testing.T) {
	r := &recorder{out: map[string]string{"log": "true\n"}}
	_, err := IsDirty(context.Background(), r.exec, testTimeout, "/d")
	if err == nil || !strings.Contains(err.Error(), "unexpected jj log output") {
		t.Fatalf("got err = %v, want a malformed-output error", err)
	}
}

// A merge as the working copy's parent is left alone rather than being
// moved off: driven through the fake exec because building a merge with
// the real binary would test jj, not this guard.
func TestUpdateWorkingCopyLeavesAMergeAlone(t *testing.T) {
	r := &recorder{out: map[string]string{
		`log --no-graph -r @`: "true\x00",
		`log --no-graph --ignore-working-copy -r main@origin`: "aaa\n",
		`log --no-graph --ignore-working-copy -r @-`:          "bbb\nccc\n",
	}}
	warning, dirty, err := UpdateWorkingCopy(context.Background(), r.exec, testTimeout, "/d", "main")
	if err != nil {
		t.Fatalf("UpdateWorkingCopy: %v", err)
	}
	if dirty || !strings.Contains(warning, "merge") {
		t.Fatalf("got warning = %q, dirty = %v", warning, dirty)
	}
	for _, c := range r.calls {
		if len(c) > 1 && c[1] == "new" {
			t.Fatal("must not run jj new when the parent is a merge")
		}
	}
}
