package main

import "github.com/swanysimon/gh-org-clone/internal/archive"

// worktreeStatus is an alias for archive.WorktreeStatus, so
// deps.go's ConfirmFunc type and every existing worktreeStatus{...} literal
// across the codebase keeps compiling unchanged.
type worktreeStatus = archive.WorktreeStatus

// manifestPath, tarballPath and localArchiveExists are thin shims over
// internal/archive, still real: worktree.go's "already archived locally"
// check uses tarballPath/localArchiveExists, and manifestPath is used by
// tests asserting neither half of an archive exists. readManifest/
// writeManifest/writeTarball/archiveManifest/manifestVersion were removed
// as dead code once engine.processTask started calling archive.Archive
// directly instead of through a root-level archiveRepo shim (see AIDEV.md).

func manifestPath(cfg config, repoName string) string {
	return archive.ManifestPath(archivesDir(cfg), repoName)
}

func tarballPath(cfg config, repoName string) string {
	return archive.TarballPath(archivesDir(cfg), repoName)
}

func localArchiveExists(cfg config, repoName string) bool {
	return archive.LocalArchiveExists(archivesDir(cfg), repoName)
}
