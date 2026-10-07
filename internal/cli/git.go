package cli

import (
	"context"

	"github.com/swanysimon/gh-org-clone/internal/engine"
	"github.com/swanysimon/gh-org-clone/internal/execx"
	"github.com/swanysimon/gh-org-clone/internal/gitcli"
)

// execCommand delegates to internal/execx.Run, which is now the one real
// implementation of this behavior (GIT_TERMINAL_PROMPT=0 etc. included).
func execCommand(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return execx.Run(ctx, dir, name, args...)
}

// runGit runs one git command bounded by cfg.Timeout. It is still a real
// shim (not dead code): worktree.go's worktreeAddArgs/cmdWorktreeAdd/
// cmdWorktreeList call it directly for one-off git invocations (worktree
// add/list) that have nothing to do with engine's task orchestration.
func runGit(ctx context.Context, cfg config, dir string, args ...string) ([]byte, error) {
	return gitcli.Run(ctx, cfg.Deps.exec, cfg.Timeout, dir, args...)
}

// cloneRepo clones into a temp sibling directory and renames on success, so
// an interrupted clone can never be mistaken for a complete one. Deliberately
// no --depth, --filter, --single-branch, --bare or --mirror: this tool exists
// to give coding agents a readable offline working tree.
//
// This is now test-only: engine.CloneInto is the real implementation, and
// every production caller (a full sync's clone action, worktree add's
// ensureClonedForWorktree) calls it directly. This shim survives only
// because a large number of tests (worktree_test.go, main_test.go) use it
// as a convenient fixture-setup helper; rewriting all of them to build an
// engine.Env and call engine.CloneInto directly wasn't judged worth the
// churn for a one-line forward with no logic of its own left to protect.
func cloneRepo(ctx context.Context, cfg config, repo ghRepo) error {
	return engine.CloneInto(ctx, buildEnv(cfg), repo)
}

// fetchRepo mirrors upstream refs exactly: --prune and --prune-tags make
// deleted branches and tags disappear locally too. Still a real shim:
// worktree add fetches an already-cloned repo before adding a worktree, a
// path engine's task orchestration doesn't cover.
func fetchRepo(ctx context.Context, cfg config, dir string) error {
	return gitcli.Fetch(ctx, cfg.Deps.exec, cfg.Timeout, dir)
}
