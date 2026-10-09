package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCmdCloneClonesAndTracks(t *testing.T) {
	origin := initTestRepo(t)
	repo := ghRepo{
		ID: "R1", Name: "repo1", NameWithOwner: "myorg/repo1",
		URL: "file://" + origin, SSHURL: "file://" + origin,
		DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now(),
	}

	stubGhRepoView(t, repo)

	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := cmdClone(context.Background(), []string{"--root", root, "--protocol", "https", "myorg/repo1"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdClone = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "myorg/repo1: clone") {
		t.Fatalf("stdout missing clone action: %q", stdout.String())
	}

	if _, err := os.Stat(filepath.Join(root, "myorg", "repos", "repo1", ".git")); err != nil {
		t.Fatalf("repo should have been cloned: %v", err)
	}

	cfg := testConfig(t, root)
	cfg.Owner = "myorg"
	st := loadState(statePath(cfg), "myorg", &stderr)
	rs, ok := st.Repos["repo1"]
	if !ok {
		t.Fatalf("no state entry for repo1")
	}
	if !rs.Tracked {
		t.Fatalf("Tracked = false, want true: %+v", rs)
	}
}

func TestCmdCloneSecondRunSkips(t *testing.T) {
	origin := initTestRepo(t)
	repo := ghRepo{
		ID: "R1", Name: "repo1", NameWithOwner: "myorg/repo1",
		URL: "file://" + origin, SSHURL: "file://" + origin,
		DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now(),
	}

	stubGhRepoView(t, repo)

	root := t.TempDir()
	var stdout1, stderr1 bytes.Buffer
	if code := cmdClone(context.Background(), []string{"--root", root, "--protocol", "https", "myorg/repo1"}, &stdout1, &stderr1); code != exitSuccess {
		t.Fatalf("first cmdClone = %d, stderr=%s", code, stderr1.String())
	}

	var stdout2, stderr2 bytes.Buffer
	code := cmdClone(context.Background(), []string{"--root", root, "--protocol", "https", "myorg/repo1"}, &stdout2, &stderr2)
	if code != exitSuccess {
		t.Fatalf("second cmdClone = %d, stderr=%s", code, stderr2.String())
	}
	if !strings.Contains(stdout2.String(), "myorg/repo1: skip") {
		t.Fatalf("second run should have skipped (pushedAt unchanged), got: %q", stdout2.String())
	}
}

func TestCmdCloneMultipleArgsContinuesPastFailure(t *testing.T) {
	origin := initTestRepo(t)
	repo := ghRepo{
		ID: "R1", Name: "repo1", NameWithOwner: "myorg/repo1",
		URL: "file://" + origin, SSHURL: "file://" + origin,
		DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now(),
	}

	stubGhRepoView(t, repo)

	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	// "not-valid" has no "/", so parseOwnerRepo rejects it before ever
	// reaching gh/git -- the real repo (myorg/repo1) must still succeed.
	code := cmdClone(context.Background(), []string{"--root", root, "--protocol", "https", "not-valid", "myorg/repo1"}, &stdout, &stderr)
	if code != exitRuntimeFail {
		t.Fatalf("cmdClone = %d, want %d (one bad arg); stderr=%s", code, exitRuntimeFail, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "myorg", "repos", "repo1", ".git")); err != nil {
		t.Fatalf("the valid repo should still have been cloned despite the other failing: %v", err)
	}
}

func TestCmdCloneHelpExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdClone(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdClone --help = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "USAGE") {
		t.Fatalf("expected usage text, got: %q", stderr.String())
	}
}

func TestCmdUntrackClearsTrackedBit(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root)
	cfg.Owner = "myorg"
	mustMkReposDir(t, cfg)
	st := state{Version: stateVersion, Org: "myorg", Repos: map[string]repoState{
		"repo1": {ID: "R1", Status: statusCloned, Tracked: true},
	}}
	if err := saveState(statePath(cfg), st); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdUntrack(context.Background(), []string{"--root", root, "myorg/repo1"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdUntrack = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "untracked myorg/repo1") {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}

	got := loadState(statePath(cfg), "myorg", &stderr)
	if got.Repos["repo1"].Tracked {
		t.Fatalf("Tracked should now be false")
	}
	// untrack must never delete anything -- the repoState entry itself
	// (and whatever ID/Status it had) must survive untouched apart from
	// Tracked.
	if got.Repos["repo1"].ID != "R1" || got.Repos["repo1"].Status != statusCloned {
		t.Fatalf("untrack altered more than Tracked: %+v", got.Repos["repo1"])
	}
}

func TestCmdUntrackRefusesWhenConfigTracksRepo(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"via owners", `{"owners": [{"name": "myorg"}]}`},
		{"via repos", `{"repos": ["myorg/repo1"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := testConfig(t, root)
			cfg.Owner = "myorg"
			mustMkReposDir(t, cfg)
			st := state{Version: stateVersion, Org: "myorg", Repos: map[string]repoState{
				"repo1": {ID: "R1", Status: statusCloned, Tracked: true},
			}}
			if err := saveState(statePath(cfg), st); err != nil {
				t.Fatal(err)
			}

			configFile := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(configFile, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}

			var stdout, stderr bytes.Buffer
			code := cmdUntrack(context.Background(), []string{"--root", root, "--config", configFile, "myorg/repo1"}, &stdout, &stderr)
			if code != exitRuntimeFail {
				t.Fatalf("cmdUntrack = %d, want %d; stderr=%s", code, exitRuntimeFail, stderr.String())
			}
			if !strings.Contains(stderr.String(), "still tracked by the config") {
				t.Fatalf("unexpected stderr: %q", stderr.String())
			}

			got := loadState(statePath(cfg), "myorg", &stderr)
			if !got.Repos["repo1"].Tracked {
				t.Fatalf("Tracked should still be true -- untrack must not have touched state on refusal")
			}
		})
	}
}

func TestCmdUntrackNoLocalDataIsAnError(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root)
	cfg.Owner = "myorg"
	mustMkReposDir(t, cfg)

	var stdout, stderr bytes.Buffer
	code := cmdUntrack(context.Background(), []string{"--root", root, "myorg/repo1"}, &stdout, &stderr)
	if code != exitRuntimeFail {
		t.Fatalf("cmdUntrack = %d, want %d; stderr=%s", code, exitRuntimeFail, stderr.String())
	}
	if !strings.Contains(stderr.String(), "nothing to untrack") {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestCmdUntrackAlreadyUntrackedIsANoOp(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root)
	cfg.Owner = "myorg"
	mustMkReposDir(t, cfg)
	st := state{Version: stateVersion, Org: "myorg", Repos: map[string]repoState{
		"repo1": {ID: "R1", Status: statusCloned, Tracked: false},
	}}
	if err := saveState(statePath(cfg), st); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdUntrack(context.Background(), []string{"--root", root, "myorg/repo1"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("cmdUntrack = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not explicitly tracked") {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
}

func TestCmdUntrackNeverDeletesLocalClone(t *testing.T) {
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
		"repo1": {ID: "R1", Status: statusCloned, Tracked: true},
	}}
	if err := saveState(statePath(cfg), st); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdUntrack(context.Background(), []string{"--root", root, "myorg/repo1"}, &stdout, &stderr); code != exitSuccess {
		t.Fatalf("cmdUntrack = %d, stderr=%s", code, stderr.String())
	}

	if _, err := os.Stat(filepath.Join(root, "myorg", "repos", "repo1", ".git")); err != nil {
		t.Fatalf("untrack must never delete the local clone: %v", err)
	}
}

func TestRunDispatchesToCloneAndUntrack(t *testing.T) {
	origin := initTestRepo(t)
	repo := ghRepo{
		ID: "R1", Name: "repo1", NameWithOwner: "myorg/repo1",
		URL: "file://" + origin, SSHURL: "file://" + origin,
		DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now(),
	}

	stubGhRepoView(t, repo)

	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"clone", "--root", root, "--protocol", "https", "myorg/repo1"}, &stdout, &stderr); code != exitSuccess {
		t.Fatalf("Run(clone ...) = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "myorg", "repos", "repo1", ".git")); err != nil {
		t.Fatalf("repo should have been cloned via Run() dispatch: %v", err)
	}

	var stdout2, stderr2 bytes.Buffer
	if code := Run(context.Background(), []string{"untrack", "--root", root, "myorg/repo1"}, &stdout2, &stderr2); code != exitSuccess {
		t.Fatalf("Run(untrack ...) = %d, stderr=%s", code, stderr2.String())
	}
	cfg := testConfig(t, root)
	cfg.Owner = "myorg"
	got := loadState(statePath(cfg), "myorg", &stderr2)
	if got.Repos["repo1"].Tracked {
		t.Fatalf("Tracked should be false after Run(untrack ...)")
	}
}
