// Package ghcli is the one place that calls the gh CLI. It takes an
// execx.Exec and plain arguments rather than this repo's config struct, so
// it has no notion of org-vs-repo scope and no dependency on anything else
// in this module.
package ghcli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/swanysimon/gh-org-clone/internal/execx"
)

// JSONFields must stay in sync with the fields Repo decodes. --limit is
// mandatory on every ListRepos call: gh defaults to 30 and would silently
// truncate large orgs.
const JSONFields = "id,name,nameWithOwner,url,sshUrl,isArchived,archivedAt,isEmpty,isFork,isPrivate,visibility,pushedAt,defaultBranchRef"

type Repo struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	NameWithOwner string    `json:"nameWithOwner"`
	URL           string    `json:"url"`
	SSHURL        string    `json:"sshUrl"`
	IsArchived    bool      `json:"isArchived"`
	IsEmpty       bool      `json:"isEmpty"`
	IsFork        bool      `json:"isFork"`
	IsPrivate     bool      `json:"isPrivate"`
	Visibility    string    `json:"visibility"`
	PushedAt      time.Time `json:"pushedAt"`
	ArchivedAt    time.Time `json:"archivedAt"`
	DefaultBranch *RefName  `json:"defaultBranchRef"`
}

type RefName struct {
	Name string `json:"name"`
}

// ListRepos does not pass --no-archived (archived repos are the point of
// this tool) or --source (forks are filtered by the caller, so it can
// report why a repo was skipped).
func ListRepos(ctx context.Context, run execx.Exec, owner string, limit int) ([]Repo, error) {
	out, err := run(ctx, "", "gh", "repo", "list", owner,
		"--limit", strconv.Itoa(limit),
		"--json", JSONFields,
	)
	if err != nil {
		return nil, fmt.Errorf("listing repos for org %q: %w", owner, err)
	}

	var repos []Repo
	if err := json.Unmarshal(out, &repos); err != nil {
		return nil, fmt.Errorf("parsing gh repo list output: %w", err)
	}
	return repos, nil
}

// ViewRepo looks up a single repo by "<owner>/<repo>", for callers (worktree
// add) that operate on one repo instead of an owner's entire listing.
// Fields match JSONFields exactly so the two call sites decode identically.
func ViewRepo(ctx context.Context, run execx.Exec, nameWithOwner string) (Repo, error) {
	out, err := run(ctx, "", "gh", "repo", "view", nameWithOwner, "--json", JSONFields)
	if err != nil {
		return Repo{}, fmt.Errorf("looking up repo %q: %w", nameWithOwner, err)
	}

	var repo Repo
	if err := json.Unmarshal(out, &repo); err != nil {
		return Repo{}, fmt.Errorf("parsing gh repo view output: %w", err)
	}
	return repo, nil
}

// CloneURL never returns an empty string silently; an empty result must be
// treated by the caller as a repo-level failure.
func CloneURL(repo Repo, protocol string) string {
	if protocol == "ssh" {
		return repo.SSHURL
	}
	return repo.URL
}
