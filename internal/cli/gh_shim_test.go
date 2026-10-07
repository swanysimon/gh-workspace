package cli

import (
	"context"
	"strings"
	"testing"
)

// TestListReposShimForwardsOrgAndMaxRepos exercises gh.go's listRepos shim
// directly (not internal/ghcli's own tests, which bypass gh.go entirely),
// confirming cfg.Owner and cfg.MaxRepos actually reach the gh invocation as
// arguments, not just that cfg.Deps.exec gets called at all.
func TestListReposShimForwardsOrgAndMaxRepos(t *testing.T) {
	var gotArgs []string
	cfg := defaultConfig()
	cfg.Owner = "shimorg"
	cfg.MaxRepos = 42
	cfg.Deps.exec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("[]"), nil
	}

	if _, err := listRepos(context.Background(), cfg); err != nil {
		t.Fatalf("listRepos: %v", err)
	}

	argv := strings.Join(gotArgs, " ")
	if !strings.Contains(argv, "shimorg") {
		t.Fatalf("cfg.Owner did not reach the gh invocation: %v", gotArgs)
	}
	if !strings.Contains(argv, "42") {
		t.Fatalf("cfg.MaxRepos did not reach the gh invocation: %v", gotArgs)
	}
}

// TestGetRepoShimForwardsNameWithOwner exercises gh.go's getRepo shim
// directly, confirming the nameWithOwner argument actually reaches the gh
// invocation.
func TestGetRepoShimForwardsNameWithOwner(t *testing.T) {
	var gotArgs []string
	cfg := defaultConfig()
	cfg.Deps.exec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"R1","name":"repo1"}`), nil
	}

	repo, err := getRepo(context.Background(), cfg, "shimorg/repo1")
	if err != nil {
		t.Fatalf("getRepo: %v", err)
	}
	if repo.ID != "R1" {
		t.Fatalf("got %+v", repo)
	}
	if argv := strings.Join(gotArgs, " "); !strings.Contains(argv, "shimorg/repo1") {
		t.Fatalf("nameWithOwner did not reach the gh invocation: %v", gotArgs)
	}
}
