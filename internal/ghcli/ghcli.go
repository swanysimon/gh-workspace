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
	"strings"
	"time"

	"github.com/swanysimon/gh-workspace/internal/execx"
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

// graphQLRepoFields is ViewRepos' per-alias selection set. Field names are
// identical to JSONFields' REST names -- GitHub's GraphQL schema and gh's
// --json flag happen to use the same names for every field here -- except
// that this is not independently verified against the real API in this
// environment (no network access); see AIDEV.md. One field is worth flagging
// specifically: GraphQL's `visibility` serializes as an upper-case enum
// value ("PUBLIC"/"PRIVATE"/"INTERNAL") where gh's REST-backed --json
// output uses lower case. Nothing in this module branches on Repo.Visibility
// today, so the casing difference is currently harmless, but a future
// caller that does compare it needs to know ViewRepos' value may be
// differently cased than ListRepos'/ViewRepo's.
const graphQLRepoFields = `id name nameWithOwner url sshUrl isArchived archivedAt isEmpty isFork isPrivate visibility pushedAt defaultBranchRef { name }`

// graphQLBatchSize caps how many aliased repository(owner:, name:) lookups
// go into one gh api graphql call -- see AIDEV.md's "about 100 per query."
const graphQLBatchSize = 100

// OwnerRepo identifies one repo for ViewRepos to look up.
type OwnerRepo struct {
	Owner string
	Name  string
}

// RepoResult is one aliased lookup's outcome. Repo is nil, not an error,
// when upstream reports the repo missing for that alias -- gone, renamed
// without a redirect, or no longer visible to the authenticated user. The
// caller decides what a nil Repo means (report it, never delete -- see
// AIDEV.md); only a response with no usable data at all is a ViewRepos
// error.
type RepoResult struct {
	Owner string
	Name  string
	Repo  *Repo
}

// graphQLResponse is gh api graphql's JSON response shape: Data's values
// are nil for an alias GraphQL reports as missing, and Errors is non-empty
// when at least one alias failed (which does not by itself mean Data is
// unusable -- a GraphQL response is commonly partial success, not
// all-or-nothing).
type graphQLResponse struct {
	Data   map[string]*Repo `json:"data"`
	Errors []graphQLError   `json:"errors"`
}

type graphQLError struct {
	Message string `json:"message"`
}

// ViewRepos looks up many repos in as few gh api graphql calls as possible,
// for callers that need to refresh a flat list of explicitly tracked repos
// without paying one gh repo view per repo -- see AIDEV.md's invariant that
// a no-op sync must stay cheap. Order of the returned slice matches repos.
func ViewRepos(ctx context.Context, run execx.Exec, repos []OwnerRepo) ([]RepoResult, error) {
	results := make([]RepoResult, 0, len(repos))
	for start := 0; start < len(repos); start += graphQLBatchSize {
		end := start + graphQLBatchSize
		if end > len(repos) {
			end = len(repos)
		}
		batch, err := viewReposBatch(ctx, run, repos[start:end])
		if err != nil {
			return nil, err
		}
		results = append(results, batch...)
	}
	return results, nil
}

func viewReposBatch(ctx context.Context, run execx.Exec, repos []OwnerRepo) ([]RepoResult, error) {
	var q strings.Builder
	q.WriteString("query {\n")
	for i, r := range repos {
		fmt.Fprintf(&q, "  r%d: repository(owner: %s, name: %s) { %s }\n", i, graphQLQuote(r.Owner), graphQLQuote(r.Name), graphQLRepoFields)
	}
	q.WriteString("}")

	// out may be non-nil even when err != nil -- see execx.Run's doc
	// comment -- which matters here: a GraphQL-level error for one alias
	// (e.g. NOT_FOUND) can come back as a non-zero gh exit alongside a
	// perfectly usable partial "data" object for every other alias. Whether
	// gh always behaves this way, versus sometimes exiting 0 with "errors"
	// present instead, is not verified against the real API in this
	// environment -- see AIDEV.md.
	out, runErr := run(ctx, "", "gh", "api", "graphql", "-f", "query="+q.String())

	if len(out) == 0 {
		if runErr != nil {
			return nil, fmt.Errorf("batch repo lookup: %w", runErr)
		}
		return nil, fmt.Errorf("batch repo lookup: empty response")
	}
	var resp graphQLResponse
	if jsonErr := json.Unmarshal(out, &resp); jsonErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("batch repo lookup: %w", runErr)
		}
		return nil, fmt.Errorf("batch repo lookup: parsing response: %w", jsonErr)
	}
	if resp.Data == nil {
		if runErr != nil {
			return nil, fmt.Errorf("batch repo lookup: %w", runErr)
		}
		return nil, fmt.Errorf("batch repo lookup: response had no data")
	}

	results := make([]RepoResult, len(repos))
	for i, r := range repos {
		results[i] = RepoResult{Owner: r.Owner, Name: r.Name, Repo: resp.Data[fmt.Sprintf("r%d", i)]}
	}
	return results, nil
}

// graphQLQuote renders a GraphQL string literal. Owner and repo names are
// already restricted to a safe charset (settings.ValidOwnerName,
// store.ValidRepoName) by every caller before reaching here, but quoting
// defensively costs nothing.
func graphQLQuote(s string) string {
	return strconv.Quote(s)
}
