package cli

import "github.com/swanysimon/gh-org-clone/internal/archive"

// defaultConfirmArchiveWithWorktrees is confirmDefault's production value
// (see deps.go), now a thin shim over archive.DefaultConfirm, which holds
// the real prompt implementation (confirm.go's old content, moved
// verbatim).
func defaultConfirmArchiveWithWorktrees(cfg config, repoName string, worktrees []worktreeStatus) (bool, error) {
	return archive.DefaultConfirm(cfg.Yes)(repoName, worktrees)
}
