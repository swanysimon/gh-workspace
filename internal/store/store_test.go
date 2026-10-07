package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOwnerPaths(t *testing.T) {
	root := "/root"
	owner := "myorg"

	if got, want := OwnerDir(root, owner), "/root/myorg"; got != want {
		t.Fatalf("OwnerDir = %q, want %q", got, want)
	}
	if got, want := ReposDir(root, owner), "/root/myorg/repos"; got != want {
		t.Fatalf("ReposDir = %q, want %q", got, want)
	}
	if got, want := ArchivesDir(root, owner), "/root/myorg/archives"; got != want {
		t.Fatalf("ArchivesDir = %q, want %q", got, want)
	}
	if got, want := StatePath(root, owner), "/root/myorg/state.json"; got != want {
		t.Fatalf("StatePath = %q, want %q", got, want)
	}
	if got, want := LockPath(root, owner), "/root/myorg/lock"; got != want {
		t.Fatalf("LockPath = %q, want %q", got, want)
	}

	// Every path must actually be a child of OwnerDir, since AcquireLock
	// (and callers' MkdirAll of ReposDir/ArchivesDir) depend on that.
	for _, p := range []string{ReposDir(root, owner), ArchivesDir(root, owner), StatePath(root, owner), LockPath(root, owner)} {
		if !strings.HasPrefix(p, OwnerDir(root, owner)+string(filepath.Separator)) {
			t.Fatalf("%q is not under OwnerDir %q", p, OwnerDir(root, owner))
		}
	}
}

func TestAcquireLock(t *testing.T) {
	root := t.TempDir()
	owner := "myorg"
	if err := os.MkdirAll(OwnerDir(root, owner), 0o700); err != nil {
		t.Fatal(err)
	}

	release, err := AcquireLock(root, owner)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if _, err := os.Stat(LockPath(root, owner)); err != nil {
		t.Fatalf("lock file should exist after AcquireLock: %v", err)
	}

	if _, err := AcquireLock(root, owner); err == nil {
		t.Fatal("a second AcquireLock while the first is held should fail")
	}

	release()
	if _, err := os.Stat(LockPath(root, owner)); !os.IsNotExist(err) {
		t.Fatalf("lock file should be gone after release, stat err = %v", err)
	}

	// Now that it's released, a fresh acquire must succeed.
	release2, err := AcquireLock(root, owner)
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	release2()
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := State{
		Version:   Version,
		Org:       "myorg",
		UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Repos: map[string]RepoState{
			"one": {ID: "R_one", PushedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), SyncedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Status: StatusCloned},
			"two": {ID: "R_two", PushedAt: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), SyncedAt: time.Date(2025, 6, 2, 0, 0, 0, 0, time.UTC), Status: StatusArchived, ArchivePath: "archives/two.tar.gz"},
		},
	}

	if err := SaveState(path, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded := LoadState(path, "myorg", os.Stderr)
	if loaded.Version != s.Version || loaded.Org != s.Org || !loaded.UpdatedAt.Equal(s.UpdatedAt) {
		t.Fatalf("top-level fields mismatch: got %+v", loaded)
	}
	if len(loaded.Repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(loaded.Repos))
	}
	for name, want := range s.Repos {
		got, ok := loaded.Repos[name]
		if !ok {
			t.Fatalf("missing repo %q", name)
		}
		if got.ID != want.ID || !got.PushedAt.Equal(want.PushedAt) || !got.SyncedAt.Equal(want.SyncedAt) || got.Status != want.Status || got.ArchivePath != want.ArchivePath {
			t.Fatalf("repo %q mismatch: got %+v, want %+v", name, got, want)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "2026-01-02T03:04:05Z") {
		t.Fatalf("expected RFC3339 timestamps in state file, got: %s", raw)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Fatalf("unexpected leftover file: %s", e.Name())
		}
	}
}

func TestLoadStateMigratesV1ToV2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	v1 := `{
		"version": 1,
		"org": "myorg",
		"updatedAt": "2026-01-01T00:00:00Z",
		"repos": {
			"one": {"id": "R_one", "pushedAt": "2026-01-01T00:00:00Z", "syncedAt": "2026-01-02T00:00:00Z", "status": "cloned"},
			"two": {"id": "R_two", "pushedAt": "2025-06-01T00:00:00Z", "syncedAt": "2025-06-02T00:00:00Z", "status": "archived", "archivePath": "archives/two.tar.gz"}
		}
	}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderrBuf bytes.Buffer
	s := LoadState(path, "myorg", &stderrBuf)

	if s.Version != Version {
		t.Fatalf("Version = %d, want %d (current) after migration", s.Version, Version)
	}
	if len(s.Repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(s.Repos))
	}
	for name, rs := range s.Repos {
		if !rs.Tracked {
			t.Errorf("repo %q: Tracked = false, want true after migrating a v1 file (see LoadState's doc comment for why)", name)
		}
	}
	// Field values from the v1 file must survive the migration untouched,
	// not just the new Tracked bit.
	if s.Repos["two"].Status != StatusArchived || s.Repos["two"].ArchivePath != "archives/two.tar.gz" {
		t.Fatalf("migration altered an existing field: %+v", s.Repos["two"])
	}
	if stderrBuf.Len() == 0 {
		t.Fatalf("expected a migration notice on stderr")
	}

	// The migrated state must actually be worth persisting: saving and
	// reloading it should come back as v2 with Tracked still true, not
	// silently re-migrate (there is no v2-to-v2 "migration" path, so this
	// also confirms the current-version branch is reached, not re-falling
	// into case 1).
	if err := SaveState(path, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	var stderrBuf2 bytes.Buffer
	reloaded := LoadState(path, "myorg", &stderrBuf2)
	if stderrBuf2.Len() != 0 {
		t.Fatalf("reloading an already-migrated v2 file should not warn/notice, got: %s", stderrBuf2.String())
	}
	if !reloaded.Repos["one"].Tracked {
		t.Fatalf("Tracked did not survive a save/reload round trip")
	}
}

func TestLoadStateCorruptBacksUpFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	garbage := []byte("not json at all")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}

	var stderrBuf bytes.Buffer
	s := LoadState(path, "myorg", &stderrBuf)
	if len(s.Repos) != 0 {
		t.Fatalf("expected an empty state, got %+v", s)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var backups []string
	for _, e := range entries {
		if e.Name() != "state.json" {
			backups = append(backups, e.Name())
		}
	}
	if len(backups) != 1 {
		t.Fatalf("expected exactly one backup file, got %v", backups)
	}
	if !strings.HasPrefix(backups[0], "state.json.corrupt-") {
		t.Fatalf("backup file %q does not match the expected naming", backups[0])
	}

	backupContent, err := os.ReadFile(filepath.Join(dir, backups[0]))
	if err != nil {
		t.Fatal(err)
	}
	if string(backupContent) != string(garbage) {
		t.Fatalf("backup content = %q, want the original corrupt content %q", backupContent, garbage)
	}
	if !strings.Contains(stderrBuf.String(), backups[0]) {
		t.Fatalf("warning should name the backup path, got: %s", stderrBuf.String())
	}
}

func TestLoadStateCorrupt(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		content []byte // nil means "do not create the file"
	}{
		{"missing file", nil},
		{"garbage", []byte("not json at all")},
		{"wrong version", []byte(`{"version": 99, "org": "myorg", "repos": {}}`)},
		{"nil repos", []byte(`{"version": 1, "org": "myorg", "repos": null}`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			if tc.content != nil {
				if err := os.WriteFile(path, tc.content, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			var stderrBuf bytes.Buffer
			s := LoadState(path, "myorg", &stderrBuf)

			if s.Repos == nil {
				t.Fatalf("Repos map must never be nil")
			}
			if tc.name == "garbage" || tc.name == "wrong version" {
				if stderrBuf.Len() == 0 {
					t.Fatalf("expected a warning on stderr for %s", tc.name)
				}
			}
		})
	}
}

func TestIsTracked(t *testing.T) {
	cases := []struct {
		ownerConfigured bool
		repoConfigured  bool
		explicit        bool
		want            bool
	}{
		{false, false, false, false},
		{true, false, false, true},
		{false, true, false, true},
		{false, false, true, true},
		{true, true, false, true},
		{true, false, true, true},
		{false, true, true, true},
		{true, true, true, true},
	}
	for _, tc := range cases {
		got := IsTracked(tc.ownerConfigured, tc.repoConfigured, tc.explicit)
		if got != tc.want {
			t.Errorf("IsTracked(owner=%v, repo=%v, explicit=%v) = %v, want %v",
				tc.ownerConfigured, tc.repoConfigured, tc.explicit, got, tc.want)
		}
	}
}

func TestValidRepoName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"repo", true},
		{".github", true},
		{"foo.bar", true},
		{"a-b", true},
		{"x_y", true},
		{"a1", true},
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{"-x", false},
		{"--upload-pack=x", false},
		{"é", false},
		{"a b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidRepoName(tc.name); got != tc.ok {
				t.Fatalf("ValidRepoName(%q) = %v, want %v", tc.name, got, tc.ok)
			}
		})
	}
}
