package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This file tests archive.go's shims directly (not internal/archive's own
// logic, which internal/archive/archive_test.go covers against real git
// repos), confirming cfg's fields actually reach archive.Archive with the
// right values. archiveRepo's manifest-field mapping (cfg.Org -> Owner,
// repo.* -> the rest) is the one piece of real logic these shims have of
// their own; the manifest written by archive_test.go's own tests was never
// actually inspected for THESE specific fields before (only Tarball*/head*
// fields, which internal/archive's own tests already re-verify), so this
// is new coverage, not just moved coverage.
func TestArchiveRepoShimMapsFieldsIntoManifest(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Org = "shimorg"
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)
	if err := os.MkdirAll(archivesDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}

	pushedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	archivedAt := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	repo := ghRepo{
		Name:          "repo1",
		NameWithOwner: "shimorg/repo1",
		URL:           "file://" + origin,
		IsArchived:    true,
		PushedAt:      pushedAt,
		ArchivedAt:    archivedAt,
		DefaultBranch: &ghRefName{Name: "main"},
	}

	if _, _, err := archiveRepo(context.Background(), cfg, repo); err != nil {
		t.Fatalf("archiveRepo: %v", err)
	}

	m, ok := readManifest(manifestPath(cfg, "repo1"))
	if !ok {
		t.Fatalf("could not read manifest")
	}
	if m.Org != "shimorg" {
		t.Errorf("manifest.Org = %q, want %q (cfg.Org did not reach the manifest)", m.Org, "shimorg")
	}
	if m.NameWithOwner != repo.NameWithOwner {
		t.Errorf("manifest.NameWithOwner = %q, want %q", m.NameWithOwner, repo.NameWithOwner)
	}
	if m.URL != repo.URL {
		t.Errorf("manifest.URL = %q, want %q", m.URL, repo.URL)
	}
	if m.DefaultBranch != "main" {
		t.Errorf("manifest.DefaultBranch = %q, want %q", m.DefaultBranch, "main")
	}
	if !m.PushedAt.Equal(pushedAt) {
		t.Errorf("manifest.PushedAt = %v, want %v", m.PushedAt, pushedAt)
	}
	if !m.ArchivedAt.Equal(archivedAt) {
		t.Errorf("manifest.ArchivedAt = %v, want %v", m.ArchivedAt, archivedAt)
	}
}

// TestArchiveRepoShimComputesDir confirms the shim's dir computation
// (reposDir(cfg) + repo.Name) is what actually gets archived/removed, by
// checking that directory specifically disappears rather than trusting
// that *some* directory did.
func TestArchiveRepoShimComputesDir(t *testing.T) {
	origin := initTestRepo(t)
	cfg := testConfig(t, t.TempDir())
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)
	if err := os.MkdirAll(archivesDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}

	repo := ghRepo{
		Name: "repo1", NameWithOwner: "testorg/repo1", URL: "file://" + origin,
		IsArchived: true, DefaultBranch: &ghRefName{Name: "main"},
	}
	wantDir := filepath.Join(reposDir(cfg), "repo1")

	if _, _, err := archiveRepo(context.Background(), cfg, repo); err != nil {
		t.Fatalf("archiveRepo: %v", err)
	}
	if _, err := os.Stat(wantDir); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be gone after archiving, stat err = %v", wantDir, err)
	}
}
