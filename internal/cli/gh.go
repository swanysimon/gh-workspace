package cli

import (
	"context"

	"github.com/swanysimon/gh-org-clone/internal/ghcli"
)

// ghRepo, ghRefName, ghJSONFields and the two functions below are thin
// aliases/shims over internal/ghcli, which now holds the real
// implementation (gh.go's old content, moved verbatim). Keeping them means
// every existing caller and every existing test that builds a ghRepo{...}
// literal or calls listRepos/getRepo keeps compiling with zero changes.
// cloneURL was removed as dead code: engine.CloneInto (and fixupRename)
// call ghcli.CloneURL directly now, and nothing else called this shim
// outside its own test.
type ghRepo = ghcli.Repo
type ghRefName = ghcli.RefName

const ghJSONFields = ghcli.JSONFields

// listRepos does not pass --no-archived (archived repos are the point of
// this tool) or --source (forks are filtered client-side so -v can report
// why a repo was skipped).
func listRepos(ctx context.Context, cfg config) ([]ghRepo, error) {
	return ghcli.ListRepos(ctx, cfg.Deps.exec, cfg.Owner, cfg.MaxRepos)
}

// getRepo looks up a single repo by "<org>/<repo>", for commands (worktree
// add) that operate on one repo instead of an org's entire listing.
func getRepo(ctx context.Context, cfg config, nameWithOwner string) (ghRepo, error) {
	return ghcli.ViewRepo(ctx, cfg.Deps.exec, nameWithOwner)
}
