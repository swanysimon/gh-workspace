package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// ghJSONFields must stay in sync with the fields ghRepo decodes. --limit is
// mandatory on every call: gh defaults to 30 and would silently truncate
// large orgs.
const ghJSONFields = "id,name,nameWithOwner,url,sshUrl,isArchived,archivedAt,isEmpty,isFork,isPrivate,visibility,pushedAt,defaultBranchRef"

type ghRepo struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	NameWithOwner string     `json:"nameWithOwner"`
	URL           string     `json:"url"`
	SSHURL        string     `json:"sshUrl"`
	IsArchived    bool       `json:"isArchived"`
	IsEmpty       bool       `json:"isEmpty"`
	IsFork        bool       `json:"isFork"`
	IsPrivate     bool       `json:"isPrivate"`
	Visibility    string     `json:"visibility"`
	PushedAt      time.Time  `json:"pushedAt"`
	ArchivedAt    time.Time  `json:"archivedAt"`
	DefaultBranch *ghRefName `json:"defaultBranchRef"`
}

type ghRefName struct {
	Name string `json:"name"`
}

// listRepos does not pass --no-archived (archived repos are the point of
// this tool) or --source (forks are filtered client-side so -v can report
// why a repo was skipped).
func listRepos(ctx context.Context, cfg config) ([]ghRepo, error) {
	out, err := cfg.Deps.exec(ctx, "", "gh", "repo", "list", cfg.Org,
		"--limit", strconv.Itoa(cfg.MaxRepos),
		"--json", ghJSONFields,
	)
	if err != nil {
		return nil, fmt.Errorf("listing repos for org %q: %w", cfg.Org, err)
	}

	var repos []ghRepo
	if err := json.Unmarshal(out, &repos); err != nil {
		return nil, fmt.Errorf("parsing gh repo list output: %w", err)
	}
	return repos, nil
}

// getRepo looks up a single repo by "<org>/<repo>", for commands (worktree
// add) that operate on one repo instead of an org's entire listing. Fields
// match ghJSONFields exactly so the two call sites decode identically.
func getRepo(ctx context.Context, cfg config, nameWithOwner string) (ghRepo, error) {
	out, err := cfg.Deps.exec(ctx, "", "gh", "repo", "view", nameWithOwner, "--json", ghJSONFields)
	if err != nil {
		return ghRepo{}, fmt.Errorf("looking up repo %q: %w", nameWithOwner, err)
	}

	var repo ghRepo
	if err := json.Unmarshal(out, &repo); err != nil {
		return ghRepo{}, fmt.Errorf("parsing gh repo view output: %w", err)
	}
	return repo, nil
}

// cloneURL never returns an empty string silently; an empty result must be
// treated by the caller as a repo-level failure.
func cloneURL(repo ghRepo, cfg config) string {
	if cfg.Protocol == "ssh" {
		return repo.SSHURL
	}
	return repo.URL
}
