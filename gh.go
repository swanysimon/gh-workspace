package main

import (
	"context"

	"github.com/swanysimon/gh-org-clone/internal/ghcli"
)

// ghRepo, ghRefName, ghJSONFields and the three functions below are thin
// aliases/shims over internal/ghcli, which now holds the real
// implementation (gh.go's old content, moved verbatim). Keeping them means
// every existing caller and every existing test that builds a ghRepo{...}
// literal or calls listRepos/getRepo/cloneURL keeps compiling with zero
// changes; they go away once engine/archive/worktree.go themselves move
// onto ghcli directly (see AIDEV.md Phase 2).
type ghRepo = ghcli.Repo
type ghRefName = ghcli.RefName

const ghJSONFields = ghcli.JSONFields

// listRepos does not pass --no-archived (archived repos are the point of
// this tool) or --source (forks are filtered client-side so -v can report
// why a repo was skipped).
func listRepos(ctx context.Context, cfg config) ([]ghRepo, error) {
	return ghcli.ListRepos(ctx, cfg.Deps.exec, cfg.Org, cfg.MaxRepos)
}

// getRepo looks up a single repo by "<org>/<repo>", for commands (worktree
// add) that operate on one repo instead of an org's entire listing.
func getRepo(ctx context.Context, cfg config, nameWithOwner string) (ghRepo, error) {
	return ghcli.ViewRepo(ctx, cfg.Deps.exec, nameWithOwner)
}

// cloneURL never returns an empty string silently; an empty result must be
// treated by the caller as a repo-level failure.
func cloneURL(repo ghRepo, cfg config) string {
	return ghcli.CloneURL(repo, cfg.Protocol)
}
