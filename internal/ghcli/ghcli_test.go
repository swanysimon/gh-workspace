package ghcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const canonicalRepoListPayload = `[
  {"id":"R_normal","name":"normal","nameWithOwner":"org/normal","url":"https://github.com/org/normal","sshUrl":"git@github.com:org/normal.git","isArchived":false,"archivedAt":null,"isEmpty":false,"isFork":false,"isPrivate":false,"visibility":"PUBLIC","pushedAt":"2026-01-01T00:00:00Z","defaultBranchRef":{"name":"main"}},
  {"id":"R_archived","name":"archived","nameWithOwner":"org/archived","url":"https://github.com/org/archived","sshUrl":"git@github.com:org/archived.git","isArchived":true,"archivedAt":"2025-06-01T00:00:00Z","isEmpty":false,"isFork":false,"isPrivate":false,"visibility":"PUBLIC","pushedAt":"2025-05-01T00:00:00Z","defaultBranchRef":{"name":"main"}},
  {"id":"R_fork","name":"fork","nameWithOwner":"org/fork","url":"https://github.com/org/fork","sshUrl":"git@github.com:org/fork.git","isArchived":false,"archivedAt":null,"isEmpty":false,"isFork":true,"isPrivate":false,"visibility":"PUBLIC","pushedAt":"2026-01-01T00:00:00Z","defaultBranchRef":{"name":"main"}},
  {"id":"R_private","name":"private","nameWithOwner":"org/private","url":"https://github.com/org/private","sshUrl":"git@github.com:org/private.git","isArchived":false,"archivedAt":null,"isEmpty":false,"isFork":false,"isPrivate":true,"visibility":"PRIVATE","pushedAt":"2026-01-01T00:00:00Z","defaultBranchRef":{"name":"main"}},
  {"id":"R_empty","name":"empty","nameWithOwner":"org/empty","url":"https://github.com/org/empty","sshUrl":"git@github.com:org/empty.git","isArchived":false,"archivedAt":null,"isEmpty":true,"isFork":false,"isPrivate":false,"visibility":"PUBLIC","pushedAt":null,"defaultBranchRef":null}
]`

func TestParseRepoList(t *testing.T) {
	var repos []Repo
	if err := json.Unmarshal([]byte(canonicalRepoListPayload), &repos); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(repos) != 5 {
		t.Fatalf("got %d repos, want 5", len(repos))
	}

	byName := map[string]Repo{}
	for _, r := range repos {
		byName[r.Name] = r
	}

	empty := byName["empty"]
	if !empty.PushedAt.IsZero() {
		t.Fatalf("empty repo pushedAt should be zero, got %v", empty.PushedAt)
	}
	if !empty.ArchivedAt.IsZero() {
		t.Fatalf("empty repo archivedAt should be zero, got %v", empty.ArchivedAt)
	}
	if empty.DefaultBranch != nil {
		t.Fatalf("empty repo defaultBranchRef should be nil, got %+v", empty.DefaultBranch)
	}

	archived := byName["archived"]
	if !archived.IsArchived {
		t.Fatalf("archived repo should have IsArchived true")
	}
	if archived.ArchivedAt.IsZero() {
		t.Fatalf("archived repo should have a non-zero archivedAt")
	}

	fork := byName["fork"]
	if !fork.IsFork {
		t.Fatalf("fork repo should have IsFork true")
	}

	private := byName["private"]
	if !private.IsPrivate {
		t.Fatalf("private repo should have IsPrivate true")
	}
}

func TestListReposArgs(t *testing.T) {
	var gotName string
	var gotArgs []string
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = args
		return []byte("[]"), nil
	}

	if _, err := ListRepos(context.Background(), fake, "myorg", 500); err != nil {
		t.Fatalf("ListRepos: %v", err)
	}

	if gotName != "gh" {
		t.Fatalf("expected gh, got %q", gotName)
	}
	want := []string{"repo", "list", "myorg", "--limit", "500", "--json", JSONFields}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("got argv %v, want %v", gotArgs, want)
	}
}

func TestListReposError(t *testing.T) {
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return nil, errors.New("gh repo list myorg: exit status 4: HTTP 401: Requires authentication")
	}

	_, err := ListRepos(context.Background(), fake, "myorg", 10000)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "HTTP 401: Requires authentication") {
		t.Fatalf("error %q does not contain gh's stderr text", err)
	}
}

func TestListReposParseError(t *testing.T) {
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte("not json"), nil
	}
	if _, err := ListRepos(context.Background(), fake, "myorg", 10); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestViewRepoArgs(t *testing.T) {
	var gotArgs []string
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"R1","name":"repo1","nameWithOwner":"myorg/repo1"}`), nil
	}

	repo, err := ViewRepo(context.Background(), fake, "myorg/repo1")
	if err != nil {
		t.Fatalf("ViewRepo: %v", err)
	}
	if repo.ID != "R1" || repo.Name != "repo1" {
		t.Fatalf("got %+v", repo)
	}
	want := []string{"repo", "view", "myorg/repo1", "--json", JSONFields}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("got argv %v, want %v", gotArgs, want)
	}
}

func TestViewRepoError(t *testing.T) {
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return nil, errors.New("gh repo view myorg/nope: exit status 1: GraphQL: Could not resolve")
	}
	if _, err := ViewRepo(context.Background(), fake, "myorg/nope"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCloneURL(t *testing.T) {
	repo := Repo{SSHURL: "git@example.invalid:org/repo.git", URL: "https://example.invalid/org/repo"}
	if got := CloneURL(repo, "ssh"); got != repo.SSHURL {
		t.Fatalf("ssh protocol: got %q, want %q", got, repo.SSHURL)
	}
	if got := CloneURL(repo, "https"); got != repo.URL {
		t.Fatalf("https protocol: got %q, want %q", got, repo.URL)
	}
}

func TestViewReposArgsAndQueryShape(t *testing.T) {
	var gotArgs []string
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"data":{"r0":{"id":"R1","name":"repo1","nameWithOwner":"myorg/repo1"},"r1":{"id":"R2","name":"repo2","nameWithOwner":"other/repo2"}}}`), nil
	}

	results, err := ViewRepos(context.Background(), fake, []OwnerRepo{
		{Owner: "myorg", Name: "repo1"},
		{Owner: "other", Name: "repo2"},
	})
	if err != nil {
		t.Fatalf("ViewRepos: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Repo == nil || results[0].Repo.ID != "R1" {
		t.Fatalf("results[0] = %+v", results[0])
	}
	if results[1].Repo == nil || results[1].Repo.ID != "R2" {
		t.Fatalf("results[1] = %+v", results[1])
	}

	if gotArgs[0] != "api" || gotArgs[1] != "graphql" || gotArgs[2] != "-f" {
		t.Fatalf("unexpected argv: %v", gotArgs)
	}
	query := strings.TrimPrefix(gotArgs[3], "query=")
	for _, want := range []string{
		`r0: repository(owner: "myorg", name: "repo1")`,
		`r1: repository(owner: "other", name: "repo2")`,
		"nameWithOwner", "defaultBranchRef { name }",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
}

func TestViewReposNullEntryIsMissingNotError(t *testing.T) {
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte(`{"data":{"r0":{"id":"R1","name":"repo1","nameWithOwner":"myorg/repo1"},"r1":null},"errors":[{"message":"Could not resolve to a Repository"}]}`), nil
	}

	results, err := ViewRepos(context.Background(), fake, []OwnerRepo{
		{Owner: "myorg", Name: "repo1"},
		{Owner: "myorg", Name: "gone"},
	})
	if err != nil {
		t.Fatalf("ViewRepos: %v", err)
	}
	if results[0].Repo == nil {
		t.Fatalf("results[0].Repo should be present")
	}
	if results[1].Repo != nil {
		t.Fatalf("results[1].Repo should be nil (missing upstream), got %+v", results[1].Repo)
	}
	if results[1].Owner != "myorg" || results[1].Name != "gone" {
		t.Fatalf("results[1] should still identify the requested repo: %+v", results[1])
	}
}

func TestViewReposNonZeroExitWithPartialDataStillParses(t *testing.T) {
	// Mirrors the documented, unverified-against-the-real-API edge case:
	// gh exits non-zero but still printed a usable partial response.
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte(`{"data":{"r0":null},"errors":[{"message":"Could not resolve to a Repository"}]}`),
			errors.New("gh api graphql: exit status 1: GraphQL: Could not resolve to a Repository")
	}
	results, err := ViewRepos(context.Background(), fake, []OwnerRepo{{Owner: "myorg", Name: "gone"}})
	if err != nil {
		t.Fatalf("ViewRepos: %v", err)
	}
	if results[0].Repo != nil {
		t.Fatalf("results[0].Repo should be nil, got %+v", results[0].Repo)
	}
}

func TestViewReposNoUsableDataIsAnError(t *testing.T) {
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte(`{"message":"Bad credentials"}`), errors.New("gh api graphql: exit status 1: HTTP 401: Bad credentials")
	}
	if _, err := ViewRepos(context.Background(), fake, []OwnerRepo{{Owner: "myorg", Name: "repo1"}}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestViewReposEmptyResponseIsAnError(t *testing.T) {
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return nil, errors.New("gh api graphql: exit status 1: network error")
	}
	if _, err := ViewRepos(context.Background(), fake, []OwnerRepo{{Owner: "myorg", Name: "repo1"}}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestViewReposBatchesAtGraphQLBatchSize(t *testing.T) {
	var calls int
	var batchSizes []int
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		calls++
		query := args[3]
		n := strings.Count(query, "repository(owner:")
		batchSizes = append(batchSizes, n)

		b := strings.Builder{}
		b.WriteString("{\"data\":{")
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "\"r%d\":{\"id\":\"R%d\"}", i, i)
		}
		b.WriteString("}}")
		return []byte(b.String()), nil
	}

	repos := make([]OwnerRepo, 150)
	for i := range repos {
		repos[i] = OwnerRepo{Owner: "myorg", Name: fmt.Sprintf("repo%d", i)}
	}
	results, err := ViewRepos(context.Background(), fake, repos)
	if err != nil {
		t.Fatalf("ViewRepos: %v", err)
	}
	if calls != 2 {
		t.Fatalf("got %d gh calls, want 2 (150 repos at batch size 100)", calls)
	}
	if len(batchSizes) != 2 || batchSizes[0] != 100 || batchSizes[1] != 50 {
		t.Fatalf("batch sizes = %v, want [100 50]", batchSizes)
	}
	if len(results) != 150 {
		t.Fatalf("got %d results, want 150", len(results))
	}
}
