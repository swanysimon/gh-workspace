package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/swanysimon/gh-org-clone/internal/settings"
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
// "worktree remove <path>"'s path-only form. A git worktree's ".git" is a
// plain text file (not a directory) of the form "gitdir:
// <central-clone>/.git/worktrees/<name>" -- not another repo's directory --
// so following it back to <central-clone> and checking that it sits at
// "<root>/<owner>/repos/<repo>" recovers owner/repo without needing the
// caller to already know them.
func resolveWorktreeOwnerRepo(cfg config, path string) (owner, repo string, err error) {
	dotGit := filepath.Join(path, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", "", fmt.Errorf("%s: not a git worktree: %w", path, err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("%s is a repository's own clone, not a worktree (its .git is a directory, not a file)", path)
	}

	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return "", "", fmt.Errorf("reading %s: %w", dotGit, err)
	}
	line := strings.TrimSpace(string(raw))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return "", "", fmt.Errorf("%s does not look like a worktree's .git file", dotGit)
	}
	gitDir := strings.TrimPrefix(line, prefix)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}

	// gitDir is "<central-clone>/.git/worktrees/<name>"; strip that
	// three-level suffix to recover <central-clone>.
	central := filepath.Dir(filepath.Dir(filepath.Dir(gitDir)))

	// git resolves symlinks when it writes gitDir (e.g. macOS's /tmp ->
	// /private/tmp), but cfg.Root is whatever string the user/flag/env/
	// config gave verbatim -- comparing them without resolving cfg.Root
	// the same way would spuriously reject every worktree under a
	// symlinked ancestor directory. Fall back to the unresolved root if
	// resolution fails (e.g. the root doesn't exist yet), rather than
	// erroring out of what should be a plain lookup.
	root := cfg.Root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	rel, err := filepath.Rel(root, central)
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
