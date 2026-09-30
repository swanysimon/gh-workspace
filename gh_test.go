package main

import (
	"context"
	"encoding/json"
	"errors"
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
	var repos []ghRepo
	if err := json.Unmarshal([]byte(canonicalRepoListPayload), &repos); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(repos) != 5 {
		t.Fatalf("got %d repos, want 5", len(repos))
	}

	byName := map[string]ghRepo{}
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

	cfg := defaultConfig()
	cfg.Org = "myorg"
	cfg.MaxRepos = 500
	cfg.Deps.exec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = args
		return []byte("[]"), nil
	}

	if _, err := listRepos(context.Background(), cfg); err != nil {
		t.Fatalf("listRepos: %v", err)
	}

	if gotName != "gh" {
		t.Fatalf("expected gh, got %q", gotName)
	}
	want := []string{"repo", "list", "myorg", "--limit", "500", "--json", ghJSONFields}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("got argv %v, want %v", gotArgs, want)
	}
}

func TestListReposError(t *testing.T) {
	cfg := defaultConfig()
	cfg.Org = "myorg"
	cfg.Deps.exec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return nil, errors.New("gh repo list myorg: exit status 4: HTTP 401: Requires authentication")
	}

	_, err := listRepos(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "HTTP 401: Requires authentication") {
		t.Fatalf("error %q does not contain gh's stderr text", err)
	}
}
