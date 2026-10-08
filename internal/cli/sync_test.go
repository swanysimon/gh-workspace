package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGhForSync dispatches "gh repo list", "gh repo view", and
// "gh api graphql" differently, since runWorkspaceSync's planning can call
// any of the three depending on which owners are configured vs purely
// explicit. listingsByOwner maps an owner name to the JSON array gh repo
// list should return for it (an owner missing from this map is an error --
// a configured owner this test forgot to plan for). viewRepoByNameWithOwner
// and graphQLResponse are canned responses for the other two.
type fakeGhForSync struct {
	listingsByOwner map[string]string
	graphQLResponse string
	ghCalls         atomic.Int64
	listCalls       atomic.Int64
	gitCalls        atomic.Int64
	real            Exec
}

func (f *fakeGhForSync) exec(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	if name != "gh" {
		f.gitCalls.Add(1)
		return f.real(ctx, dir, name, args...)
	}
	f.ghCalls.Add(1)
	switch {
	case len(args) >= 2 && args[0] == "repo" && args[1] == "list":
		f.listCalls.Add(1)
		owner := args[2]
		body, ok := f.listingsByOwner[owner]
		if !ok {
			return nil, fmt.Errorf("fakeGhForSync: no listing configured for owner %q", owner)
		}
		return []byte(body), nil
	case len(args) >= 2 && args[0] == "repo" && args[1] == "view":
		return nil, fmt.Errorf("fakeGhForSync: gh repo view not expected in this test (got %v)", args)
	case len(args) >= 2 && args[0] == "api" && args[1] == "graphql":
		return []byte(f.graphQLResponse), nil
	}
	return nil, fmt.Errorf("fakeGhForSync: unexpected gh invocation: %v", args)
}

func newFakeGhForSync() *fakeGhForSync {
	return &fakeGhForSync{listingsByOwner: map[string]string{}, real: execDefault}
}

func repoListJSON(t *testing.T, repos []ghRepo) string {
	t.Helper()
	b, err := json.Marshal(repos)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRunSyncOwnerArgMatchesBareTopLevel confirms "sync <org>" and bare
// "<org>" are genuinely the same code path (runSingleOwnerSync), not two
// implementations that could drift -- both clone the same repo from the
// same fake listing with identical output shape.
func TestRunSyncOwnerArgMatchesBareTopLevel(t *testing.T) {
	origin := initTestRepo(t)
	fake := newFakeGhForSync()
	fake.listingsByOwner["myorg"] = repoListJSON(t, []ghRepo{
		{ID: "R1", Name: "repo1", NameWithOwner: "myorg/repo1", URL: "file://" + origin, SSHURL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now()},
	})
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = fake.exec

	root1 := t.TempDir()
	var stdout1, stderr1 bytes.Buffer
	code := Run(context.Background(), []string{"--root", root1, "--protocol", "https", "myorg"}, &stdout1, &stderr1)
	if code != exitSuccess {
		t.Fatalf("bare: Run() = %d, stderr=%s", code, stderr1.String())
	}

	root2 := t.TempDir()
	var stdout2, stderr2 bytes.Buffer
	code = Run(context.Background(), []string{"sync", "--root", root2, "--protocol", "https", "myorg"}, &stdout2, &stderr2)
	if code != exitSuccess {
		t.Fatalf("sync <org>: Run() = %d, stderr=%s", code, stderr2.String())
	}

	if stdout1.String() != stdout2.String() {
		t.Fatalf("bare and sync <org> produced different output:\nbare: %q\nsync: %q", stdout1.String(), stdout2.String())
	}
	if _, err := os.Stat(filepath.Join(root2, "myorg", "repos", "repo1", ".git")); err != nil {
		t.Fatalf("sync <org> should have cloned repo1: %v", err)
	}
}

// TestRunSyncSingleRepoArg confirms "sync <org>/<repo>" clones exactly one
// repo via engine.SyncOne, and -- unlike clone -- does not mark it Tracked.
func TestRunSyncSingleRepoArg(t *testing.T) {
	origin := initTestRepo(t)
	fake := newFakeGhForSync()
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
			fake.ghCalls.Add(1)
			repo := ghRepo{ID: "R1", Name: "repo1", NameWithOwner: "myorg/repo1", URL: "file://" + origin, SSHURL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now()}
			b, _ := json.Marshal(repo)
			return b, nil
		}
		return fake.exec(ctx, dir, name, args...)
	}

	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"sync", "--root", root, "--protocol", "https", "myorg/repo1"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "myorg/repo1: clone") {
		t.Fatalf("stdout missing clone action: %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, "myorg", "repos", "repo1", ".git")); err != nil {
		t.Fatalf("repo1 should have been cloned: %v", err)
	}

	cfg := testConfig(t, root)
	cfg.Owner = "myorg"
	st := loadState(statePath(cfg), "myorg", &stderr)
	if st.Repos["repo1"].Tracked {
		t.Fatalf("sync <org>/<repo> should not mark the repo Tracked (only clone/worktree add do)")
	}
}

// TestRunSyncWorkspaceWideAcrossOwners is this feature's central claim: a
// bare "sync" syncs every configured owner (its own full listing) and
// every explicitly tracked repo outside those owners (via one batched
// lookup, grouped back by owner), each owner's state.json saved
// independently.
func TestRunSyncWorkspaceWideAcrossOwners(t *testing.T) {
	configuredOrigin := initTestRepo(t)
	explicitOrigin := initTestRepo(t)

	fake := newFakeGhForSync()
	fake.listingsByOwner["configured-org"] = repoListJSON(t, []ghRepo{
		{ID: "R1", Name: "repo1", NameWithOwner: "configured-org/repo1", URL: "file://" + configuredOrigin, SSHURL: "file://" + configuredOrigin, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now()},
	})
	fake.graphQLResponse = fmt.Sprintf(
		`{"data":{"r0":{"id":"R2","name":"explicit-repo","nameWithOwner":"explicit-owner/explicit-repo","url":"file://%s","sshUrl":"file://%s","defaultBranchRef":{"name":"main"},"pushedAt":"2026-01-01T00:00:00Z"}}}`,
		explicitOrigin, explicitOrigin)

	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = fake.exec

	root := t.TempDir()
	configFile := filepath.Join(t.TempDir(), "config.json")
	body := `{"owners":[{"name":"configured-org"}],"repos":["explicit-owner/explicit-repo"]}`
	if err := os.WriteFile(configFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"sync", "--root", root, "--protocol", "https", "--config", configFile}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}

	if _, err := os.Stat(filepath.Join(root, "configured-org", "repos", "repo1", ".git")); err != nil {
		t.Fatalf("configured-org/repo1 should have been cloned via its listing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "explicit-owner", "repos", "explicit-repo", ".git")); err != nil {
		t.Fatalf("explicit-owner/explicit-repo should have been cloned via the batched lookup: %v", err)
	}

	cfg := testConfig(t, root)
	cfg.Owner = "configured-org"
	st1 := loadState(statePath(cfg), "configured-org", &stderr)
	if _, ok := st1.Repos["repo1"]; !ok {
		t.Fatalf("configured-org's state.json should have an entry for repo1")
	}
	cfg.Owner = "explicit-owner"
	st2 := loadState(statePath(cfg), "explicit-owner", &stderr)
	if _, ok := st2.Repos["explicit-repo"]; !ok {
		t.Fatalf("explicit-owner's state.json should have an entry for explicit-repo")
	}
}

// TestRunSyncWorkspaceWideLockContentionSkipsOnlyThatOwner confirms the
// documented "partial failure" rule: an owner whose lock is already held
// is reported and skipped, every other owner still finishes, and the run
// exits 1.
func TestRunSyncWorkspaceWideLockContentionSkipsOnlyThatOwner(t *testing.T) {
	originA := initTestRepo(t)
	originB := initTestRepo(t)

	fake := newFakeGhForSync()
	fake.listingsByOwner["owner-a"] = repoListJSON(t, []ghRepo{
		{ID: "RA", Name: "repoa", NameWithOwner: "owner-a/repoa", URL: "file://" + originA, SSHURL: "file://" + originA, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now()},
	})
	fake.listingsByOwner["owner-b"] = repoListJSON(t, []ghRepo{
		{ID: "RB", Name: "repob", NameWithOwner: "owner-b/repob", URL: "file://" + originB, SSHURL: "file://" + originB, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now()},
	})

	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = fake.exec

	root := t.TempDir()
	configFile := filepath.Join(t.TempDir(), "config.json")
	body := `{"owners":[{"name":"owner-a"},{"name":"owner-b"}]}`
	if err := os.WriteFile(configFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgA := testConfig(t, root)
	cfgA.Owner = "owner-a"
	mustMkReposDir(t, cfgA)
	releaseA, err := acquireLock(cfgA)
	if err != nil {
		t.Fatalf("pre-locking owner-a: %v", err)
	}
	defer releaseA()

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"sync", "--root", root, "--protocol", "https", "--config", configFile}, &stdout, &stderr)
	if code != exitRuntimeFail {
		t.Fatalf("Run() = %d, want %d (owner-a locked); stderr=%s", code, exitRuntimeFail, stderr.String())
	}
	if !strings.Contains(stderr.String(), "owner-a") {
		t.Fatalf("stderr should mention the locked owner: %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "owner-b", "repos", "repob", ".git")); err != nil {
		t.Fatalf("owner-b should still have been synced despite owner-a's lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "owner-a", "repos", "repoa", ".git")); err == nil {
		t.Fatalf("owner-a should NOT have been synced while locked")
	}
}

// TestRunTopLevelTrackedOnlySkipsListing is the specific regression this
// step found and fixed: --tracked-only was silently accepted (bindSettings
// registers it for every CmdSync caller, including the bare top-level
// entry point) but completely ignored by it. gh repo list must never be
// called when --tracked-only is set, even via the bare "<org>" form.
func TestRunTopLevelTrackedOnlySkipsListing(t *testing.T) {
	origin := initTestRepo(t)
	root := t.TempDir()

	cfg := testConfig(t, root)
	cfg.Owner = "myorg"
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)
	repo := ghRepo{Name: "repo1", URL: "file://" + origin}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	st := state{Version: stateVersion, Org: "myorg", Repos: map[string]repoState{
		"repo1": {ID: "R1", Status: statusCloned, Tracked: true, PushedAt: time.Now()},
	}}
	if err := saveState(statePath(cfg), st); err != nil {
		t.Fatal(err)
	}

	fake := newFakeGhForSync()
	fake.graphQLResponse = fmt.Sprintf(
		`{"data":{"r0":{"id":"R1","name":"repo1","nameWithOwner":"myorg/repo1","url":"file://%s","sshUrl":"file://%s","defaultBranchRef":{"name":"main"},"pushedAt":"2026-01-01T00:00:00Z"}}}`,
		origin, origin)
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = fake.exec

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--root", root, "--protocol", "https", "--tracked-only", "myorg"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}
	if fake.listCalls.Load() != 0 {
		t.Fatalf("gh repo list was called %d times; --tracked-only must never list, only batch-lookup already-tracked repos", fake.listCalls.Load())
	}
}

// TestRunSyncTrackedOnlyFlagSkipsListing is the same regression, exercised
// through the explicit "sync <org> --tracked-only" form instead of the
// bare top-level alias.
func TestRunSyncTrackedOnlyFlagSkipsListing(t *testing.T) {
	origin := initTestRepo(t)
	root := t.TempDir()

	cfg := testConfig(t, root)
	cfg.Owner = "myorg"
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)
	repo := ghRepo{Name: "repo1", URL: "file://" + origin}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	st := state{Version: stateVersion, Org: "myorg", Repos: map[string]repoState{
		"repo1": {ID: "R1", Status: statusCloned, Tracked: true, PushedAt: time.Now()},
	}}
	if err := saveState(statePath(cfg), st); err != nil {
		t.Fatal(err)
	}

	fake := newFakeGhForSync()
	fake.graphQLResponse = fmt.Sprintf(
		`{"data":{"r0":{"id":"R1","name":"repo1","nameWithOwner":"myorg/repo1","url":"file://%s","sshUrl":"file://%s","defaultBranchRef":{"name":"main"},"pushedAt":"2026-01-01T00:00:00Z"}}}`,
		origin, origin)
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = fake.exec

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"sync", "--root", root, "--protocol", "https", "--tracked-only", "myorg"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}
	if fake.listCalls.Load() != 0 {
		t.Fatalf("gh repo list was called %d times; --tracked-only must never list, only batch-lookup already-tracked repos", fake.listCalls.Load())
	}
}

func TestRunSyncHelpExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"sync", "--help"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "USAGE") {
		t.Fatalf("expected usage text, got %q", stderr.String())
	}
}

func TestRunSyncTooManyPositionalArgsIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"sync", "a", "b"}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("Run() = %d, want %d; stderr=%s", code, exitUsage, stderr.String())
	}
}

func TestRunSyncDryRunDoesNotLockOrWrite(t *testing.T) {
	origin := initTestRepo(t)
	fake := newFakeGhForSync()
	fake.listingsByOwner["myorg"] = repoListJSON(t, []ghRepo{
		{ID: "R1", Name: "repo1", NameWithOwner: "myorg/repo1", URL: "file://" + origin, SSHURL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now()},
	})
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = fake.exec

	root := t.TempDir()
	configFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configFile, []byte(`{"owners":[{"name":"myorg"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"sync", "--root", root, "--protocol", "https", "--config", configFile, "--dry-run"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "myorg/repo1: clone") {
		t.Fatalf("dry run should report the planned clone: %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, "myorg")); err == nil {
		t.Fatalf("dry run must not create any directories, found %q", filepath.Join(root, "myorg"))
	}
}

// TestRunSyncTrackedOnlyOnBrandNewOwnerIsANoOp confirms --tracked-only
// against an owner nothing has ever been synced for (no config, no local
// state) is a clean no-op, not an error -- there is simply nothing to
// refresh.
func TestRunSyncTrackedOnlyOnBrandNewOwnerIsANoOp(t *testing.T) {
	fake := newFakeGhForSync()
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = fake.exec

	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--root", root, "--protocol", "https", "--tracked-only", "brand-new-owner"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}
	if fake.ghCalls.Load() != 0 {
		t.Fatalf("gh should never be called for an owner with nothing tracked, got %d calls", fake.ghCalls.Load())
	}
	if !strings.Contains(stdout.String(), "owners=1") {
		t.Fatalf("stdout should still report the one owner processed: %q", stdout.String())
	}
}

// TestRunSyncWorkspaceInStateNotInListingNote confirms the single-owner
// path's "in state but not in listing" note still fires for a workspace-
// wide sync's configured owners, and -- the specific invariant AIDEV.md
// calls out -- does NOT fire for a repo the owner's own filter excluded
// (a fork, here) that is also explicitly tracked, since that repo gets a
// real decision via the batched lookup instead of just a note.
func TestRunSyncWorkspaceInStateNotInListingNote(t *testing.T) {
	trackedForkOrigin := initTestRepo(t)

	fake := newFakeGhForSync()
	fake.listingsByOwner["myorg"] = repoListJSON(t, []ghRepo{
		// The listing excludes the fork (includeForks defaults to false);
		// only an explicit track (simulated via state below) brings it
		// back into scope, through the batched lookup, not this listing.
	})
	fake.graphQLResponse = fmt.Sprintf(
		`{"data":{"r0":{"id":"RFORK","name":"forkrepo","nameWithOwner":"myorg/forkrepo","url":"file://%s","sshUrl":"file://%s","isFork":true,"defaultBranchRef":{"name":"main"},"pushedAt":"2026-01-01T00:00:00Z"}}}`,
		trackedForkOrigin, trackedForkOrigin)

	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = fake.exec

	root := t.TempDir()
	cfg := testConfig(t, root)
	cfg.Owner = "myorg"
	mustMkReposDir(t, cfg)
	st := state{Version: stateVersion, Org: "myorg", Repos: map[string]repoState{
		"forkrepo": {ID: "RFORK", Status: statusCloned, Tracked: true},
		"gone":     {ID: "RGONE", Status: statusCloned, Tracked: false},
	}}
	if err := saveState(statePath(cfg), st); err != nil {
		t.Fatal(err)
	}

	configFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configFile, []byte(`{"owners":[{"name":"myorg"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"sync", "--root", root, "--protocol", "https", "--config", configFile}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}

	if strings.Contains(stderr.String(), "myorg/forkrepo is in local state but was not returned") {
		t.Fatalf("forkrepo is explicitly tracked; it must get a real decision, not just the generic note: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "myorg/gone is in local state but was not returned by gh repo list") {
		t.Fatalf("gone (not tracked, not in listing) should get the generic note: %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "myorg", "repos", "forkrepo", ".git")); err != nil {
		t.Fatalf("forkrepo should have been cloned via the explicit path despite being a fork: %v", err)
	}
}

// TestRunSyncWorkspaceBatchFailureFailsOwnerEvenWithListingTasks is a real
// bug found by review: a configured owner that already has listing-derived
// tasks must still fail the run (not silently drop its explicit repos)
// when the cross-owner batch lookup itself fails.
func TestRunSyncWorkspaceBatchFailureFailsOwnerEvenWithListingTasks(t *testing.T) {
	listedOrigin := initTestRepo(t)

	fake := newFakeGhForSync()
	fake.listingsByOwner["myorg"] = repoListJSON(t, []ghRepo{
		{ID: "R1", Name: "listed", NameWithOwner: "myorg/listed", URL: "file://" + listedOrigin, SSHURL: "file://" + listedOrigin, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now()},
	})
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "api" && args[1] == "graphql" {
			return nil, fmt.Errorf("gh api graphql: exit status 1: network error")
		}
		return fake.exec(ctx, dir, name, args...)
	}

	root := t.TempDir()
	cfg := testConfig(t, root)
	cfg.Owner = "myorg"
	mustMkReposDir(t, cfg)
	st := state{Version: stateVersion, Org: "myorg", Repos: map[string]repoState{
		"explicit-only": {ID: "R2", Status: statusCloned, Tracked: true},
	}}
	if err := saveState(statePath(cfg), st); err != nil {
		t.Fatal(err)
	}

	configFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configFile, []byte(`{"owners":[{"name":"myorg"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"sync", "--root", root, "--protocol", "https", "--config", configFile}, &stdout, &stderr)
	if code != exitRuntimeFail {
		t.Fatalf("Run() = %d, want %d (batch lookup failed); stdout=%s stderr=%s", code, exitRuntimeFail, stdout.String(), stderr.String())
	}
}
