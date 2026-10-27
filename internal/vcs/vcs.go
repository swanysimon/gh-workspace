// Package vcs is the seam between git and jj. Every operation that
// *mutates* a clone or a worktree goes through here; read-only helpers do
// not, because a jj clone is colocated (see internal/jjcli) and therefore
// also a valid git repo, so gitcli.HeadInfo, gitcli.Tags and every
// ".git exists" check keep working against both kinds unchanged.
//
// The dispatch rule that keeps this small: every function operating on a
// clone that already exists calls Detect(dir) itself, one os.Stat of
// dir/.jj. Nothing above this package has to thread a VCS choice through
// its own types. Only two things need the *configured* kind — creating a
// clone that does not exist yet (Clone), and deciding whether to convert
// an existing git clone (Resolve) — and those take it as an argument.
package vcs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swanysimon/gh-workspace/internal/execx"
	"github.com/swanysimon/gh-workspace/internal/gitcli"
	"github.com/swanysimon/gh-workspace/internal/jjcli"
)

// Kind is which VCS manages a clone. The string values are the config
// file's own "vcs" values, so settings can carry one as a plain validated
// string (exactly as it carries "protocol") and convert here.
type Kind string

const (
	Git Kind = "git"
	JJ  Kind = "jj"
)

// Detect reports which VCS manages the clone at dir. A jj clone is
// colocated, so it has both .jj and .git and the .jj is what distinguishes
// it; a directory that is neither (or does not exist) reports Git, which
// is what every pre-existing clone in a workspace is.
func Detect(dir string) Kind {
	if _, err := os.Stat(filepath.Join(dir, ".jj")); err == nil {
		return JJ
	}
	return Git
}

// Resolve is the whole VCS policy in one function: what to use for this
// repo, and whether the existing clone has to be converted first.
//
// A clone already managed by jj stays with jj whatever the config says —
// detection beats configuration, so changing "vcs" can never invalidate
// what is already on disk, and a workspace holding both kinds keeps
// working. A git clone under a configured jj is the one case that needs
// action: adopt reports that `jj git init --colocate` should run before
// anything else touches it. Everything else just uses the configured kind,
// which for a directory that does not exist yet is the only input there
// is.
func Resolve(dir string, configured Kind) (kind Kind, adopt bool) {
	if Detect(dir) == JJ {
		return JJ, false
	}
	if configured != JJ {
		return Git, false
	}
	if _, err := os.Stat(dir); err != nil {
		return JJ, false
	}
	return JJ, true
}

// Clone clones url into tmp and renames it to dest on success. kind is the
// configured kind: there is no clone on disk yet to detect anything from.
func Clone(ctx context.Context, run execx.Exec, timeout time.Duration, kind Kind, url, dest, tmp string) error {
	if kind == JJ {
		return jjcli.Clone(ctx, run, timeout, url, dest, tmp)
	}
	return gitcli.Clone(ctx, run, timeout, url, dest, tmp)
}

// Adopt converts an existing git clone into a colocated jj repo in place,
// for the case Resolve flagged. The caller must have established that dir
// is not dirty — adopting snapshots uncommitted work into @ and stages it
// in git's index, which "a dirty working tree is never touched" forbids.
func Adopt(ctx context.Context, run execx.Exec, timeout time.Duration, dir, defaultBranch string) (warning string, err error) {
	return jjcli.Adopt(ctx, run, timeout, dir, defaultBranch)
}

func Fetch(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) error {
	if Detect(dir) == JJ {
		return jjcli.Fetch(ctx, run, timeout, dir)
	}
	return gitcli.Fetch(ctx, run, timeout, dir)
}

func IsDirty(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) (bool, error) {
	if Detect(dir) == JJ {
		return jjcli.IsDirty(ctx, run, timeout, dir)
	}
	return gitcli.IsDirty(ctx, run, timeout, dir)
}

// UpdateWorkingCopy brings a clone's checkout up to the default branch,
// and never runs a destructive command in either implementation: anything
// other than a clean checkout sitting on the default branch is left alone
// and reported as a warning.
func UpdateWorkingCopy(ctx context.Context, run execx.Exec, timeout time.Duration, dir, defaultBranch string) (warning string, dirty bool, err error) {
	if Detect(dir) == JJ {
		return jjcli.UpdateWorkingCopy(ctx, run, timeout, dir, defaultBranch)
	}
	return gitcli.UpdateWorktree(ctx, run, timeout, dir, defaultBranch)
}

func SetRemoteURL(ctx context.Context, run execx.Exec, timeout time.Duration, dir, url string) error {
	if Detect(dir) == JJ {
		return jjcli.SetRemoteURL(ctx, run, timeout, dir, url)
	}
	return gitcli.SetRemoteURL(ctx, run, timeout, dir, url)
}

// Worktree is one working copy attached to a clone other than the clone's
// own: a git linked worktree, or a jj workspace. Name is empty for git,
// which identifies a worktree only by its path; jj needs the name to
// forget one.
type Worktree struct {
	Name  string
	Path  string
	Dirty bool
}

// Worktrees reports every working copy attached to the clone at dir other
// than dir itself. Dirtiness is measured in each worktree's own directory
// for both kinds — git status there, or jj's snapshot of that workspace —
// so it is live rather than whatever the repo last recorded.
func Worktrees(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) ([]Worktree, error) {
	out, err := WorktreePaths(ctx, run, timeout, dir)
	if err != nil {
		return nil, err
	}
	isDirty := gitcli.IsDirty
	if Detect(dir) == JJ {
		isDirty = jjcli.IsDirty
	}
	for i, w := range out {
		if out[i].Dirty, err = isDirty(ctx, run, timeout, w.Path); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// WorktreePaths is Worktrees without the per-worktree dirtiness check,
// which costs a subprocess (and for jj, a snapshot) per worktree.
func WorktreePaths(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) ([]Worktree, error) {
	if Detect(dir) == JJ {
		workspaces, err := jjcli.Workspaces(ctx, run, timeout, dir)
		if err != nil {
			return nil, err
		}
		out := make([]Worktree, 0, len(workspaces))
		for _, w := range workspaces {
			out = append(out, Worktree{Name: w.Name, Path: w.Path})
		}
		return out, nil
	}

	paths, err := gitcli.LinkedWorktrees(ctx, run, timeout, dir)
	if err != nil {
		return nil, err
	}
	out := make([]Worktree, 0, len(paths))
	for _, p := range paths {
		out = append(out, Worktree{Path: p})
	}
	return out, nil
}

// WorktreeAdd adds a working copy of the clone at dir, at path, for
// branch. The returned note is what to tell the user about what was
// decided for them, and differs by kind because the two VCSs mean
// different things by a branch:
//
// git checks the branch out, creating it from the default branch when it
// exists nowhere yet (with --no-track, so the first "git push -u" decides
// the upstream). jj does not check out branches at all — bookmarks are
// named when work is pushed, not when it starts — so branch selects the
// starting revision and nothing more, no bookmark is created, and the note
// says so along with the command that creates one.
func WorktreeAdd(ctx context.Context, run execx.Exec, timeout time.Duration, dir, path, branch, defaultBranch string) (note string, err error) {
	// Both kinds validate the branch with git: a GitHub branch is a git
	// ref whichever VCS is used locally, and this is the check the git
	// path already trusted before jj existed here.
	if _, err := gitcli.Run(ctx, run, timeout, dir, "check-ref-format", "--branch", branch); err != nil {
		return "", fmt.Errorf("%q is not a valid branch name", branch)
	}

	if Detect(dir) == JJ {
		return jjWorktreeAdd(ctx, run, timeout, dir, path, branch, defaultBranch)
	}
	return gitWorktreeAdd(ctx, run, timeout, dir, path, branch, defaultBranch)
}

func gitWorktreeAdd(ctx context.Context, run execx.Exec, timeout time.Duration, dir, path, branch, defaultBranch string) (note string, err error) {
	refExists := func(ref string) bool {
		_, err := gitcli.Run(ctx, run, timeout, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		return err == nil
	}

	args := []string{"worktree", "add", "--", path, branch}
	if !refExists("refs/heads/"+branch) && !refExists("refs/remotes/origin/"+branch) {
		base := "HEAD"
		if defaultBranch != "" && refExists("refs/remotes/origin/"+defaultBranch) {
			base = "origin/" + defaultBranch
		}
		args = []string{"worktree", "add", "--no-track", "-b", branch, "--", path, base}
		note = fmt.Sprintf("created new branch %q from %s", branch, base)
	}

	if _, err := gitcli.Run(ctx, run, timeout, dir, args...); err != nil {
		return "", err
	}
	return note, nil
}

func jjWorktreeAdd(ctx context.Context, run execx.Exec, timeout time.Duration, dir, path, branch, defaultBranch string) (note string, err error) {
	// First match wins: the branch's own bookmark, local or remote, then
	// the default branch as the place new work starts from. Each is
	// resolved to a commit id, so what lands in "jj workspace add -r" is
	// never a name that could be read as a revset expression.
	type candidate struct{ revset, label string }
	candidates := []candidate{
		{jjcli.LocalBookmark(branch), branch},
		{jjcli.RemoteBookmark(branch), branch + "@origin"},
	}
	if defaultBranch != "" {
		candidates = append(candidates, candidate{jjcli.RemoteBookmark(defaultBranch), defaultBranch + "@origin"})
	}

	// Falling through every candidate leaves the revset empty, which lets
	// jj pick (the new workspace shares the current one's parents) — the
	// jj-native equivalent of the git path's "HEAD" fallback.
	revset, from := "", "the working copy's own parents"
	for _, c := range candidates {
		ids, err := jjcli.Resolve(ctx, run, timeout, dir, c.revset)
		if err != nil {
			return "", err
		}
		if len(ids) == 1 {
			revset, from = ids[0], c.label
			break
		}
	}

	name := WorkspaceName(branch)
	if err := jjcli.WorkspaceAdd(ctx, run, timeout, dir, name, path, revset); err != nil {
		return "", err
	}
	return fmt.Sprintf("workspace %q starts on top of %s; no bookmark was created -- run \"jj bookmark create %s -r @\" when you're ready to push", name, from, branch), nil
}

// WorkspaceName is the jj workspace name derived from a branch. jj
// workspace names are a flat namespace per repo, so "/" is replaced the
// same way the worktree path template replaces it.
func WorkspaceName(branch string) string {
	return strings.ReplaceAll(branch, "/", "-")
}

// WorktreeRemove spins down the working copy at path. git's own command
// does the whole job, including refusing a dirty worktree without force.
// jj has no such command: "jj workspace forget" deliberately leaves the
// directory on disk, so this removes it — which is why path must be one
// Worktrees reported for *this* clone, never a path the caller was handed.
// The dirty check is this package's own for jj, mirroring what git refuses
// on its own.
func WorktreeRemove(ctx context.Context, run execx.Exec, timeout time.Duration, dir, path string, force bool) error {
	if Detect(dir) != JJ {
		args := []string{"worktree", "remove"}
		if force {
			args = append(args, "--force")
		}
		args = append(args, "--", path)
		_, err := gitcli.Run(ctx, run, timeout, dir, args...)
		return err
	}

	worktrees, err := Worktrees(ctx, run, timeout, dir)
	if err != nil {
		return err
	}
	want := resolvePath(path)
	for _, w := range worktrees {
		if resolvePath(w.Path) != want {
			continue
		}
		if w.Dirty && !force {
			return fmt.Errorf("jj workspace %q at %s has changes in its working copy; pass --force to remove it anyway", w.Name, w.Path)
		}
		if err := jjcli.WorkspaceForget(ctx, run, timeout, dir, w.Name); err != nil {
			return err
		}
		if err := os.RemoveAll(w.Path); err != nil {
			return fmt.Errorf("removing jj workspace directory %s: %w", w.Path, err)
		}
		return nil
	}
	return fmt.Errorf("%s is not a jj workspace of %s", path, dir)
}

// RepairWorktrees re-points every working copy attached to the clone at
// dir after dir itself has moved, which breaks all of them: a git
// worktree's ".git" file names an absolute path inside the clone, and a jj
// workspace's ".jj/repo" names a relative one. Neither kind needs its
// paths captured before the move -- the moved clone still knows its own
// working copies -- so this takes only the clone's new location.
//
// A renamed repo is the one case that moves a clone, and it does not need
// to destroy anything to do it, unlike archiving (where the clone really
// does go away, which is why that path prompts instead).
func RepairWorktrees(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) error {
	if Detect(dir) != JJ {
		return gitcli.RepairWorktrees(ctx, run, timeout, dir)
	}
	workspaces, err := jjcli.Workspaces(ctx, run, timeout, dir)
	if err != nil {
		return err
	}
	for _, w := range workspaces {
		if err := jjcli.RepairWorkspace(w.Path, dir); err != nil {
			return err
		}
	}
	return nil
}

// WorktreeListText is the VCS's own listing output, passed through rather
// than reformatted: a user asking to list worktrees is asking what their
// VCS says about them.
func WorktreeListText(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) ([]byte, error) {
	if Detect(dir) == JJ {
		return jjcli.Run(ctx, run, timeout, dir, "workspace", "list")
	}
	return gitcli.Run(ctx, run, timeout, dir, "worktree", "list")
}

// RepoRoot finds the clone a working copy at path belongs to, for the
// path-only form of "worktree remove". Neither kind needs the caller to
// already know the repo: a git worktree's ".git" is a text file naming
// "<clone>/.git/worktrees/<name>", and a jj workspace's ".jj/repo" is a
// text file naming "<clone>/.jj/repo" (relative to the workspace's own
// ".jj"). In both cases the clone's own copy of that entry is a directory
// instead, which is how "this is a clone, not a working copy of one" is
// recognized.
func RepoRoot(path string) (string, error) {
	if info, err := os.Stat(filepath.Join(path, ".jj")); err == nil && info.IsDir() {
		repo := filepath.Join(path, ".jj", "repo")
		info, err := os.Stat(repo)
		if err != nil {
			return "", fmt.Errorf("%s: not a jj workspace: %w", path, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s is a repository's own clone, not a worktree (its .jj/repo is a directory, not a file)", path)
		}
		raw, err := os.ReadFile(repo)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", repo, err)
		}
		target := strings.TrimSpace(string(raw))
		if target == "" {
			return "", fmt.Errorf("%s does not name a repository", repo)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(path, ".jj", target)
		}
		// target is "<clone>/.jj/repo"; strip that two-level suffix.
		return filepath.Dir(filepath.Dir(target)), nil
	}

	dotGit := filepath.Join(path, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", fmt.Errorf("%s: not a git worktree: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a repository's own clone, not a worktree (its .git is a directory, not a file)", path)
	}
	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", dotGit, err)
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: ")
	if !ok {
		return "", fmt.Errorf("%s does not look like a worktree's .git file", dotGit)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	// gitDir is "<clone>/.git/worktrees/<name>"; strip that three-level
	// suffix.
	return filepath.Dir(filepath.Dir(filepath.Dir(gitDir))), nil
}

// resolvePath makes two paths comparable despite symlinked ancestors (on
// macOS, /tmp -> /private/tmp): a VCS reports a resolved path, while the
// other side is whatever string a caller built or a user typed.
func resolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}
