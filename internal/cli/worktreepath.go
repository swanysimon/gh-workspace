package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/swanysimon/gh-workspace/internal/settings"
	"github.com/swanysimon/gh-workspace/internal/vcs"
)

// resolveWorktreePath computes an "add"'s target directory when no
// explicit path argument was given: cfg.WorktreeRoot (or the current
// directory, if unset -- the dynamic default settings.Settings itself
// can't express) joined with cfg.WorktreePath's template, after expanding
// {owner}/{repo}/{branch} placeholders.
//
// {branch} has every "/" replaced with "-" before substitution, so a
// branch like "feat/x" can't create extra path levels the template didn't
// ask for. The joined, cleaned result must stay under worktreeRoot: a
// template or branch name that could otherwise escape it (via "..") is
// rejected, never silently clamped.
func resolveWorktreePath(cfg config, owner, repo, branch string) (string, error) {
	root := cfg.WorktreeRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolving current directory for worktree placement: %w", err)
		}
		root = wd
	}

	sanitizedBranch := strings.ReplaceAll(branch, "/", "-")
	rel := expandWorktreePathTemplate(cfg.WorktreePath, owner, repo, sanitizedBranch)

	joined := filepath.Join(root, rel)
	cleanedRoot := filepath.Clean(root)
	if joined != cleanedRoot && !strings.HasPrefix(joined, cleanedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("worktree-path %q escapes worktree-root %q", cfg.WorktreePath, root)
	}
	return joined, nil
}

// expandWorktreePathTemplate substitutes {owner}, {repo}, and {branch} in
// template. branch must already have "/" replaced -- see
// resolveWorktreePath's own doc comment for why.
func expandWorktreePathTemplate(template, owner, repo, branch string) string {
	out := strings.ReplaceAll(template, "{owner}", owner)
	out = strings.ReplaceAll(out, "{repo}", repo)
	out = strings.ReplaceAll(out, "{branch}", branch)
	return out
}

// resolveWorktreeOwnerRepo finds the owning repo of a worktree at path, for
// "worktree remove <path>"'s path-only form. vcs.RepoRoot follows the
// working copy back to its central clone (a git worktree's ".git" file or
// a jj workspace's ".jj/repo" file); this function's own job is the part
// that is store layout knowledge rather than VCS knowledge: checking the
// result sits at "<root>/<owner>/repos/<repo>", which recovers owner/repo
// without the caller having to already know them.
func resolveWorktreeOwnerRepo(cfg config, path string) (owner, repo string, err error) {
	central, err := vcs.RepoRoot(path)
	if err != nil {
		return "", "", err
	}

	// Both sides are resolved before comparing, because neither is
	// reliably resolved to begin with: cfg.Root is whatever string the
	// user/flag/env/config gave verbatim, and central is resolved for a
	// git worktree (git writes an absolute gitdir) but not for a jj
	// workspace, whose .jj/repo is relative and so inherits whatever path
	// the user typed. Without resolving both, a workspace under a
	// symlinked ancestor (macOS's /var -> /private/var) is spuriously
	// rejected -- which is exactly what happened the first time the jj
	// path-only removal was tested. Resolution failing (e.g. the root
	// doesn't exist yet) falls back to the unresolved value rather than
	// erroring out of what should be a plain lookup.
	root := resolveSymlinks(cfg.Root)
	rel, err := filepath.Rel(root, resolveSymlinks(central))
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", fmt.Errorf("%s is not a worktree of a repo under %s", path, cfg.Root)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[1] != "repos" {
		return "", "", fmt.Errorf("%s is not a worktree of a repo under %s", path, cfg.Root)
	}
	owner, repo = parts[0], parts[2]
	if !settings.ValidOwnerName(owner) || !validRepoName(repo) {
		return "", "", fmt.Errorf("%s is not a worktree of a repo under %s", path, cfg.Root)
	}
	return owner, repo, nil
}

func resolveSymlinks(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}
