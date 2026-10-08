package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swanysimon/gh-org-clone/internal/settings"
)

func TestConfigVerboseAliases(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())

	for _, flag := range []string{"-v", "--verbose"} {
		t.Run(flag, func(t *testing.T) {
			cfg, _, err := resolveConfig([]string{flag, "myorg"}, os.Stderr)
			if err != nil {
				t.Fatalf("resolveConfig: %v", err)
			}
			if !cfg.Verbose {
				t.Fatalf("%s did not set Verbose", flag)
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())

	cfg, _, err := resolveConfig([]string{"myorg"}, os.Stderr)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}

	want := defaultConfig()
	want.Owner = "myorg"
	// config can no longer be compared whole with !=: Deps holds function
	// values (see deps.go), and a struct type with a function-typed field is
	// not a comparable type at all, regardless of the values inside it.
	// Compare every other field instead; Deps' own wiring is exercised by
	// the tests that set cfg.Deps.exec/confirm directly.
	if cfg.Owner != want.Owner ||
		cfg.Root != want.Root ||
		cfg.Concurrency != want.Concurrency ||
		cfg.Timeout != want.Timeout ||
		cfg.MaxRepos != want.MaxRepos ||
		cfg.Protocol != want.Protocol ||
		cfg.IncludeForks != want.IncludeForks ||
		cfg.Archive != want.Archive ||
		cfg.Force != want.Force ||
		cfg.DryRun != want.DryRun ||
		cfg.Verbose != want.Verbose ||
		cfg.Yes != want.Yes {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestConfigPrecedence(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())

	fileDir := t.TempDir()
	configPath := filepath.Join(fileDir, "config.json")
	fileCfg := `{"concurrency": 4, "protocol": "https", "maxRepos": 500}`
	if err := os.WriteFile(configPath, []byte(fileCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// (a) file beats default: concurrency, protocol, maxRepos come from file.
	cfg, _, err := resolveConfig([]string{"--config", configPath, "myorg"}, os.Stderr)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Concurrency != 4 || cfg.Protocol != "https" || cfg.MaxRepos != 500 {
		t.Fatalf("file did not win over default: %+v", cfg)
	}
	// Root was not set in the file, so it must remain the default.
	if cfg.Root != defaultRoot() {
		t.Fatalf("unset file field clobbered default root: got %q", cfg.Root)
	}

	// (b) env beats file: override concurrency and protocol via env.
	t.Setenv("GH_ORG_CLONE_CONCURRENCY", "6")
	t.Setenv("GH_ORG_CLONE_PROTOCOL", "ssh")
	cfg, _, err = resolveConfig([]string{"--config", configPath, "myorg"}, os.Stderr)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Concurrency != 6 || cfg.Protocol != "ssh" {
		t.Fatalf("env did not win over file: %+v", cfg)
	}
	// maxRepos was not overridden by env, so the file's value must survive.
	if cfg.MaxRepos != 500 {
		t.Fatalf("env overlay clobbered unrelated file field: got %d", cfg.MaxRepos)
	}

	// (c) explicit flag beats env.
	cfg, _, err = resolveConfig([]string{"--config", configPath, "--concurrency", "9", "myorg"}, os.Stderr)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Concurrency != 9 {
		t.Fatalf("flag did not win over env: %+v", cfg)
	}
	// protocol flag not passed: env value must survive untouched.
	if cfg.Protocol != "ssh" {
		t.Fatalf("unset flag clobbered env value: got %q", cfg.Protocol)
	}

	// (d) a flag not passed must not clobber the file's value (the fs.Visit regression).
	cfg, _, err = resolveConfig([]string{"--config", configPath, "--include-forks", "myorg"}, os.Stderr)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.MaxRepos != 500 {
		t.Fatalf("unrelated flag parse clobbered file value: got %d", cfg.MaxRepos)
	}
	if !cfg.IncludeForks {
		t.Fatalf("explicitly passed flag was not applied")
	}
}

func TestConfigRejects(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())

	badFile := filepath.Join(t.TempDir(), "bad.json")
	unknownKeyFile := filepath.Join(t.TempDir(), "unknown.json")
	badOwnerFile := filepath.Join(t.TempDir(), "bad-owner.json")
	badRepoFile := filepath.Join(t.TempDir(), "bad-repo.json")
	if err := os.WriteFile(badFile, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknownKeyFile, []byte(`{"nope": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badOwnerFile, []byte(`{"owners": [{"name": "not valid!"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badRepoFile, []byte(`{"repos": ["noslash"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"no positional arg", []string{}},
		{"two positional args", []string{"a", "b"}},
		{"bad concurrency", []string{"--concurrency", "0", "myorg"}},
		{"bad protocol", []string{"--protocol", "ftp", "myorg"}},
		{"bad timeout", []string{"--timeout", "banana", "myorg"}},
		{"negative max-repos", []string{"--max-repos", "-1", "myorg"}},
		{"malformed config file", []string{"--config", badFile, "myorg"}},
		{"unknown config key", []string{"--config", unknownKeyFile, "myorg"}},
		{"invalid owner in config owners list", []string{"--config", badOwnerFile, "myorg"}},
		{"malformed entry in config repos list", []string{"--config", badRepoFile, "myorg"}},
		{"~user is not expanded, so still relative", []string{"--root", "~someone/path", "myorg"}},
		{"$HOME is not expanded, so still relative", []string{"--root", "$HOME/path", "myorg"}},
		{"plain relative root is still rejected", []string{"--root", "relative/path", "myorg"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := resolveConfig(tc.args, os.Stderr); err == nil {
				t.Fatalf("expected an error, got none")
			}
		})
	}
}

func TestConfigExpandsHomeInRoot(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, _, err := resolveConfig([]string{"--root", "~/src/.workspace", "myorg"}, os.Stderr)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	want := filepath.Join(home, "src/.workspace")
	if cfg.Root != want {
		t.Errorf("cfg.Root = %q, want %q", cfg.Root, want)
	}
}

func TestConfigExpandsHomeInRootFromConfigFile(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	configFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configFile, []byte(`{"root": "~/from-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := resolveConfig([]string{"--config", configFile, "myorg"}, os.Stderr)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	want := filepath.Join(home, "from-file")
	if cfg.Root != want {
		t.Errorf("cfg.Root = %q, want %q", cfg.Root, want)
	}
}

func TestConfigAcceptsValidOwnersAndRepos(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())

	configFile := filepath.Join(t.TempDir(), "config.json")
	body := `{
		"owners": [{"name": "my-org"}, {"name": "other-org", "includeForks": true}],
		"repos": ["someone/useful-lib"]
	}`
	if err := os.WriteFile(configFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveConfig([]string{"--config", configFile, "myorg"}, os.Stderr); err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"XDG_DATA_HOME", "XDG_CONFIG_HOME", "GH_ORG_CLONE_CONFIG",
		"GH_ORG_CLONE_ROOT", "GH_ORG_CLONE_CONCURRENCY", "GH_ORG_CLONE_TIMEOUT",
		"GH_ORG_CLONE_MAX_REPOS", "GH_ORG_CLONE_PROTOCOL", "GH_ORG_CLONE_INCLUDE_FORKS",
		"GH_ORG_CLONE_ARCHIVE",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestRunEndToEnd(t *testing.T) {
	ctx := context.Background()

	normalOrigin := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(normalOrigin, "file.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := execCommand(ctx, normalOrigin, "git", "add", "file.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := execCommand(ctx, normalOrigin, "git", "commit", "--quiet", "-m", "second"); err != nil {
		t.Fatal(err)
	}

	archivedOrigin := initTestRepo(t)

	emptyOrigin := t.TempDir()
	if _, err := execCommand(ctx, emptyOrigin, "git", "init", "--quiet", "-b", "main"); err != nil {
		t.Fatal(err)
	}

	pushedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repos := []ghRepo{
		{ID: "R_normal", Name: "normal", NameWithOwner: "testorg/normal", URL: "file://" + normalOrigin, SSHURL: "file://" + normalOrigin, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: pushedAt},
		{ID: "R_archived", Name: "archived", NameWithOwner: "testorg/archived", URL: "file://" + archivedOrigin, SSHURL: "file://" + archivedOrigin, IsArchived: true, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: pushedAt, ArchivedAt: pushedAt},
		{ID: "R_empty", Name: "empty", NameWithOwner: "testorg/empty", URL: "file://" + emptyOrigin, SSHURL: "file://" + emptyOrigin, IsEmpty: true},
	}
	reposJSON, err := json.Marshal(repos)
	if err != nil {
		t.Fatal(err)
	}

	old := execDefault
	t.Cleanup(func() { execDefault = old })

	var ghCalls, gitCalls atomic.Int64
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			ghCalls.Add(1)
			return reposJSON, nil
		}
		gitCalls.Add(1)
		return old(ctx, dir, name, args...)
	}

	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"--root", root, "--protocol", "https", "testorg"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}

	normalDir := filepath.Join(root, "testorg", "repos", "normal")
	got, err := os.ReadFile(filepath.Join(normalDir, "file.txt"))
	if err != nil {
		t.Fatalf("normal repo missing: %v", err)
	}
	if string(got) != "v2\n" {
		t.Fatalf("normal repo file = %q, want %q", got, "v2\n")
	}

	archivedDir := filepath.Join(root, "testorg", "repos", "archived")
	if _, err := os.Stat(archivedDir); !os.IsNotExist(err) {
		t.Fatalf("archived repo clone should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "testorg", "archives", "archived.tar.gz")); err != nil {
		t.Fatalf("tarball missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "testorg", "archives", "archived.json")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}

	emptyDir := filepath.Join(root, "testorg", "repos", "empty")
	if _, err := os.Stat(emptyDir); err != nil {
		t.Fatalf("empty repo missing: %v", err)
	}

	stateRaw, err := os.ReadFile(filepath.Join(root, "testorg", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st state
	if err := json.Unmarshal(stateRaw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != stateVersion {
		t.Fatalf("state version = %d, want %d", st.Version, stateVersion)
	}
	if st.Repos["normal"].Status != statusCloned {
		t.Fatalf("normal status = %q, want %q", st.Repos["normal"].Status, statusCloned)
	}
	if st.Repos["archived"].Status != statusArchived {
		t.Fatalf("archived status = %q, want %q", st.Repos["archived"].Status, statusArchived)
	}
	if st.Repos["empty"].Status != statusCloned {
		t.Fatalf("empty status = %q, want %q", st.Repos["empty"].Status, statusCloned)
	}

	summary := stdout.String()
	if !strings.Contains(summary, "cloned=2") || !strings.Contains(summary, "archived=1") {
		t.Fatalf("unexpected summary: %q", summary)
	}
	if !strings.Contains(stderr.String(), "syncing 3 repos") {
		t.Fatalf("stderr missing progress summary line: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "normal: cloning") {
		t.Fatalf("stderr missing per-repo progress line: %q", stderr.String())
	}

	// Incremental assertion: a second run must issue exactly one gh call
	// and zero git calls, with every repo (live or already archived)
	// reported skipped.
	ghCalls.Store(0)
	gitCalls.Store(0)
	var stdout2, stderr2 bytes.Buffer
	code2 := Run(ctx, []string{"--root", root, "--protocol", "https", "testorg"}, &stdout2, &stderr2)
	if code2 != exitSuccess {
		t.Fatalf("second Run() = %d, stderr=%s", code2, stderr2.String())
	}
	if got := ghCalls.Load(); got != 1 {
		t.Fatalf("second run made %d gh calls, want 1", got)
	}
	if got := gitCalls.Load(); got != 0 {
		t.Fatalf("second run made %d git calls, want 0", got)
	}
	summary2 := stdout2.String()
	if !strings.Contains(summary2, "skipped=3") || !strings.Contains(summary2, "archived=0") {
		t.Fatalf("unexpected second-run summary: %q", summary2)
	}
}

// TestRunPreservesTrackedAcrossSync guards a real bug this phase's own
// work introduced and fixed before it ever shipped: Run()'s sync loop
// writes a brand new repoState literal for every repo on every run, and
// that literal did not carry the previous entry's Tracked bit forward --
// so a repo explicitly added via worktree add (or, later, a clone
// command) would have silently reverted to untracked the very next time
// an ordinary owner-wide sync touched it.
func TestRunPreservesTrackedAcrossSync(t *testing.T) {
	ctx := context.Background()
	origin := initTestRepo(t)
	pushedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repos := []ghRepo{
		{ID: "R1", Name: "repo1", NameWithOwner: "testorg/repo1", URL: "file://" + origin, SSHURL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: pushedAt},
	}
	reposJSON, err := json.Marshal(repos)
	if err != nil {
		t.Fatal(err)
	}

	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			return reposJSON, nil
		}
		return old(ctx, dir, name, args...)
	}

	root := t.TempDir()

	// Simulate a repo already explicitly tracked (as if by a prior
	// worktree add) before this sync run even starts: pre-seed state.json
	// with Tracked: true and a pushedAt already matching upstream, so the
	// sync plans a Skip for it -- the least favorable case for "carries
	// forward," since a Skip's state write path is the shortest one.
	cfg := defaultConfig()
	cfg.Root = root
	cfg.Owner = "testorg"
	if err := os.MkdirAll(ownerDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	preseeded := state{
		Version: stateVersion,
		Org:     "testorg",
		Repos: map[string]repoState{
			"repo1": {ID: "R1", PushedAt: pushedAt, SyncedAt: pushedAt, Status: statusCloned, Tracked: true},
		},
	}
	if err := saveState(statePath(cfg), preseeded); err != nil {
		t.Fatal(err)
	}
	if err := cloneRepo(ctx, cfg, repos[0]); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"--root", root, "--protocol", "https", "testorg"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}

	got := loadState(statePath(cfg), "testorg", &stderr)
	if !got.Repos["repo1"].Tracked {
		t.Fatalf("Tracked was not carried forward across the sync run: %+v", got.Repos["repo1"])
	}
}

func TestRunNoGh(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--root", t.TempDir(), "testorg"}, &stdout, &stderr)
	if code != exitRuntimeFail {
		t.Fatalf("Run() = %d, want %d; stderr=%s", code, exitRuntimeFail, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gh") {
		t.Fatalf("stderr does not mention gh: %s", stderr.String())
	}
}

func TestRunLockHeld(t *testing.T) {
	root := t.TempDir()
	cfg := config{Settings: settings.Settings{Root: root}, Owner: "testorg"}
	if err := os.MkdirAll(ownerDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath(cfg), []byte("999 sometime\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--root", root, "testorg"}, &stdout, &stderr)
	if code == exitSuccess {
		t.Fatalf("Run() succeeded despite a held lock")
	}
	if !strings.Contains(stderr.String(), lockPath(cfg)) {
		t.Fatalf("stderr does not name the lock file: %s", stderr.String())
	}
	entries, err := os.ReadDir(reposDir(cfg))
	if err == nil && len(entries) != 0 {
		t.Fatalf("repos dir should be untouched, got %v", entries)
	}
}

func TestRunDryRun(t *testing.T) {
	origin := initTestRepo(t)
	repos := []ghRepo{
		{ID: "R1", Name: "repo1", NameWithOwner: "testorg/repo1", URL: "file://" + origin, SSHURL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: time.Now()},
	}
	reposJSON, err := json.Marshal(repos)
	if err != nil {
		t.Fatal(err)
	}

	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			return reposJSON, nil
		}
		return old(ctx, dir, name, args...)
	}

	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--root", root, "--protocol", "https", "--dry-run", "testorg"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "repo1") {
		t.Fatalf("dry-run output missing repo1: %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, "testorg", "repos", "repo1")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create a clone, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "testorg")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create the org directory (or lock/state inside it), stat err = %v", err)
	}
}

func TestRunDryRunPlansRenameWithoutPerformingIt(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)

	pushedAt := time.Now().Truncate(time.Second)
	if err := cloneRepo(context.Background(), cfg, ghRepo{Name: "oldname", URL: "file://" + origin}); err != nil {
		t.Fatal(err)
	}
	st := state{Version: stateVersion, Org: cfg.Owner, Repos: map[string]repoState{
		"oldname": {ID: "R1", PushedAt: pushedAt, Status: statusCloned},
	}}
	if err := saveState(statePath(cfg), st); err != nil {
		t.Fatal(err)
	}
	// A stale temp clone must also survive a dry run.
	tmpClone := filepath.Join(reposDir(cfg), ".tmp-leftover")
	if err := os.Mkdir(tmpClone, 0o700); err != nil {
		t.Fatal(err)
	}

	repos := []ghRepo{
		{ID: "R1", Name: "newname", NameWithOwner: "testorg/newname", URL: "file://" + origin, SSHURL: "file://" + origin, DefaultBranch: &ghRefName{Name: "main"}, PushedAt: pushedAt},
	}
	reposJSON, err := json.Marshal(repos)
	if err != nil {
		t.Fatal(err)
	}
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			return reposJSON, nil
		}
		return old(ctx, dir, name, args...)
	}

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--root", cfg.Root, "--protocol", "https", "--dry-run", "testorg"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("Run() = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "newname: skip") || !strings.Contains(stdout.String(), `rename from "oldname"`) {
		t.Fatalf("dry-run output should plan the rename and then skip: %s", stdout.String())
	}
	if strings.Contains(stderr.String(), "not returned by gh repo list") {
		t.Fatalf("renamed repo should not be reported as missing: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(reposDir(cfg), "oldname", ".git")); err != nil {
		t.Fatalf("dry-run must not move the old directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reposDir(cfg), "newname")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not create the new directory, stat err = %v", err)
	}
	if _, err := os.Stat(tmpClone); err != nil {
		t.Fatalf("dry-run must not sweep temp clones: %v", err)
	}
	if _, err := os.Stat(lockPath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not leave or take a lock, stat err = %v", err)
	}
	if got := loadState(statePath(cfg), cfg.Owner, &stderr); got.Repos["oldname"].ID != "R1" {
		t.Fatalf("dry-run must not rewrite state, got %+v", got.Repos)
	}
}

func stubGhRepoList(t *testing.T, repos []ghRepo) {
	t.Helper()
	reposJSON, err := json.Marshal(repos)
	if err != nil {
		t.Fatal(err)
	}
	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			return reposJSON, nil
		}
		return old(ctx, dir, name, args...)
	}
}

func TestRunPrepassErrorsFailTheRun(t *testing.T) {
	stubGhRepoList(t, []ghRepo{
		{ID: "R1", Name: "Foo", URL: "file:///nonexistent"},
		{ID: "R2", Name: "foo", URL: "file:///nonexistent"},
		{ID: "R3", Name: "-bad", URL: "file:///nonexistent"},
		// An excluded fork must not collide with, and so block, a real repo.
		{ID: "R4", Name: "bar", URL: "file:///nonexistent"},
		{ID: "R5", Name: "Bar", URL: "file:///nonexistent", IsFork: true},
	})

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--root", t.TempDir(), "--protocol", "https", "--dry-run", "testorg"}, &stdout, &stderr)
	if code != exitRuntimeFail {
		t.Fatalf("Run() = %d, want %d; stderr=%s", code, exitRuntimeFail, stderr.String())
	}
	if !strings.Contains(stderr.String(), "collide") || !strings.Contains(stderr.String(), "not a valid repo name") {
		t.Fatalf("stderr should report the collision and the invalid name: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "bar: clone") {
		t.Fatalf("bar should be planned despite the excluded fork Bar: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "Foo:") || strings.Contains(stdout.String(), "foo:") {
		t.Fatalf("colliding repos must not be planned: %s", stdout.String())
	}
}

func TestRunWarnsWhenListingMayBeTruncated(t *testing.T) {
	stubGhRepoList(t, []ghRepo{
		{ID: "R1", Name: "one", URL: "file:///nonexistent"},
		{ID: "R2", Name: "two", URL: "file:///nonexistent"},
	})

	for _, tc := range []struct {
		maxRepos string
		warn     bool
	}{{"2", true}, {"3", false}} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), []string{"--root", t.TempDir(), "--protocol", "https", "--dry-run", "--max-repos", tc.maxRepos, "testorg"}, &stdout, &stderr)
		if code != exitSuccess {
			t.Fatalf("max-repos=%s: Run() = %d, stderr=%s", tc.maxRepos, code, stderr.String())
		}
		if got := strings.Contains(stderr.String(), "may be truncated"); got != tc.warn {
			t.Fatalf("max-repos=%s: truncation warning = %v, want %v; stderr=%s", tc.maxRepos, got, tc.warn, stderr.String())
		}
	}
}

func TestConfigFlagsAfterOrg(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())

	cfg, _, err := resolveConfig([]string{"myorg", "--dry-run", "--concurrency", "3"}, os.Stderr)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Owner != "myorg" || !cfg.DryRun || cfg.Concurrency != 3 {
		t.Fatalf("flags after the org were not applied: %+v", cfg)
	}
}

func TestHelpExitsZero(t *testing.T) {
	for _, args := range [][]string{
		{"-h"},
		{"--help"},
		{"worktree", "add", "-h"},
		{"worktree", "remove", "--help"},
		{"worktree", "list", "-h"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr); code != exitSuccess {
			t.Fatalf("Run(%v) = %d, want %d; stderr=%s", args, code, exitSuccess, stderr.String())
		}
		if !strings.Contains(strings.ToLower(stderr.String()), "usage") {
			t.Fatalf("Run(%v) printed no usage: %s", args, stderr.String())
		}
		if strings.Contains(stderr.String(), "help requested") {
			t.Fatalf("Run(%v) printed the flag package's error: %s", args, stderr.String())
		}
	}
}
