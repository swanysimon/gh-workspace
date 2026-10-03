package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/swanysimon/gh-org-clone/internal/execx"
	"github.com/swanysimon/gh-org-clone/internal/gitcli"
)

// execCommand delegates to internal/execx.Run, which is now the one real
// implementation of this behavior (GIT_TERMINAL_PROMPT=0 etc. included).
func execCommand(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return execx.Run(ctx, dir, name, args...)
}

// archiveTag is a type alias for gitcli.Tag, kept so archive.go's existing
// archiveManifest.Tags field and every test building an archiveTag{...}
// literal keep compiling unchanged.
type archiveTag = gitcli.Tag

// Every function below is a thin shim over internal/gitcli, which now holds
// the real implementation (git.go's old content, moved verbatim, taking an
// execx.Exec and a timeout explicitly instead of cfg). They exist so every
// existing caller (archive.go, worktree.go, every test file) keeps
// compiling and passing with zero changes; they go away once those callers
// themselves move onto gitcli directly (see AIDEV.md Phase 2).

// runGit runs one git command bounded by cfg.Timeout.
func runGit(ctx context.Context, cfg config, dir string, args ...string) ([]byte, error) {
	return gitcli.Run(ctx, cfg.Deps.exec, cfg.Timeout, dir, args...)
}

// cloneRepo clones into a temp sibling directory and renames on success, so
// an interrupted clone can never be mistaken for a complete one. Deliberately
// no --depth, --filter, --single-branch, --bare or --mirror: this tool exists
// to give coding agents a readable offline working tree.
func cloneRepo(ctx context.Context, cfg config, repo ghRepo) error {
	url := cloneURL(repo, cfg)
	if url == "" {
		return fmt.Errorf("repo %q has no clone URL for protocol %q", repo.Name, cfg.Protocol)
	}

	dest := filepath.Join(reposDir(cfg), repo.Name)
	tmp := filepath.Join(reposDir(cfg), ".tmp-"+repo.Name+"-"+strconv.Itoa(os.Getpid()))

	return gitcli.Clone(ctx, cfg.Deps.exec, cfg.Timeout, url, dest, tmp)
}

// fetchRepo mirrors upstream refs exactly: --prune and --prune-tags make
// deleted branches and tags disappear locally too.
func fetchRepo(ctx context.Context, cfg config, dir string) error {
	return gitcli.Fetch(ctx, cfg.Deps.exec, cfg.Timeout, dir)
}

func isDirty(ctx context.Context, cfg config, dir string) (bool, error) {
	return gitcli.IsDirty(ctx, cfg.Deps.exec, cfg.Timeout, dir)
}

// updateWorktree never runs a destructive git command; see
// gitcli.UpdateWorktree for the full behavior.
func updateWorktree(ctx context.Context, cfg config, dir, defaultBranch string) (warning string, dirty bool, err error) {
	return gitcli.UpdateWorktree(ctx, cfg.Deps.exec, cfg.Timeout, dir, defaultBranch)
}

// headInfo resolves the default branch's remote-tracking commit, falling
// back to HEAD. A repo with no commits returns zero values and no error.
func headInfo(ctx context.Context, cfg config, dir, defaultBranch string) (sha string, committedAt time.Time, subject string, err error) {
	return gitcli.HeadInfo(ctx, cfg.Deps.exec, cfg.Timeout, dir, defaultBranch)
}

// tags returns an empty slice and no error when the repo has no tags.
func tags(ctx context.Context, cfg config, dir string) ([]archiveTag, error) {
	return gitcli.Tags(ctx, cfg.Deps.exec, cfg.Timeout, dir)
}

// linkedWorktrees reports every worktree attached to dir other than dir's
// own primary working tree.
func linkedWorktrees(ctx context.Context, cfg config, dir string) ([]string, error) {
	return gitcli.LinkedWorktrees(ctx, cfg.Deps.exec, cfg.Timeout, dir)
}

func setRemoteURL(ctx context.Context, cfg config, dir, url string) error {
	return gitcli.SetRemoteURL(ctx, cfg.Deps.exec, cfg.Timeout, dir, url)
}
