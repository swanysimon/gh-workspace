package main

import (
	"context"
	"path/filepath"

	"github.com/swanysimon/gh-org-clone/internal/archive"
)

// archiveManifest and worktreeStatus are aliases for archive.Manifest/
// archive.WorktreeStatus, so every existing archiveManifest{...}/
// worktreeStatus{...} literal and every field access across the codebase
// keeps compiling unchanged.
type archiveManifest = archive.Manifest
type worktreeStatus = archive.WorktreeStatus

const manifestVersion = archive.ManifestVersion

// Every function below is a thin shim over internal/archive, which now
// holds the real implementation (archive.go's old content, moved
// verbatim, taking plain arguments instead of cfg/ghRepo).

func manifestPath(cfg config, repoName string) string {
	return archive.ManifestPath(archivesDir(cfg), repoName)
}

func tarballPath(cfg config, repoName string) string {
	return archive.TarballPath(archivesDir(cfg), repoName)
}

func localArchiveExists(cfg config, repoName string) bool {
	return archive.LocalArchiveExists(archivesDir(cfg), repoName)
}

func readManifest(path string) (archiveManifest, bool) {
	return archive.ReadManifest(path)
}

func writeManifest(path string, m archiveManifest) error {
	return archive.WriteManifest(path, m)
}

func writeTarball(dir, repoName, dest string) (int64, string, error) {
	return archive.WriteTarball(dir, repoName, dest)
}

// confirmAndRemoveWorktrees is a no-op when dir has no linked worktrees;
// see archive.ConfirmAndRemoveWorktrees for the full behavior.
func confirmAndRemoveWorktrees(ctx context.Context, cfg config, repo ghRepo, dir string) error {
	confirm := func(repoName string, worktrees []worktreeStatus) (bool, error) {
		return cfg.Deps.confirm(cfg, repoName, worktrees)
	}
	return archive.ConfirmAndRemoveWorktrees(ctx, cfg.Deps.exec, cfg.Timeout, confirm, repo.Name, dir)
}

// archiveRepo tarballs a repo and deletes its clone only after the tarball
// and manifest are both durably on disk; see archive.Archive for the full
// behavior. Building the archive.Repo/confirm/clone values below is the
// one piece of real logic this shim has of its own.
func archiveRepo(ctx context.Context, cfg config, repo ghRepo) (repoState, []string, error) {
	dir := filepath.Join(reposDir(cfg), repo.Name)

	defaultBranch := ""
	if repo.DefaultBranch != nil {
		defaultBranch = repo.DefaultBranch.Name
	}

	archiveRepoInfo := archive.Repo{
		ID:            repo.ID,
		Owner:         cfg.Org,
		Name:          repo.Name,
		NameWithOwner: repo.NameWithOwner,
		URL:           repo.URL,
		PushedAt:      repo.PushedAt,
		ArchivedAt:    repo.ArchivedAt,
		DefaultBranch: defaultBranch,
	}
	confirm := func(repoName string, worktrees []worktreeStatus) (bool, error) {
		return cfg.Deps.confirm(cfg, repoName, worktrees)
	}
	clone := func(ctx context.Context) error {
		return cloneRepo(ctx, cfg, repo)
	}

	return archive.Archive(ctx, cfg.Deps.exec, cfg.Timeout, confirm, archiveRepoInfo, dir, archivesDir(cfg), clone)
}
