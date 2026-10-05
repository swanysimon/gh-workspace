package main

import (
	"io"

	"github.com/swanysimon/gh-org-clone/internal/store"
)

// state, repoState, repoStatus, stateVersion, statusCloned, statusArchived
// are aliases for internal/store's types/consts, which now hold the real
// implementation (state.go's old content, moved verbatim, taking plain
// root/owner strings instead of cfg). Every existing state{...}/
// repoState{...} literal across the codebase (main_test.go, state_test.go,
// archive.go) keeps compiling unchanged.
type state = store.State
type repoState = store.RepoState
type repoStatus = store.Status

const (
	stateVersion   = store.Version
	statusCloned   = store.StatusCloned
	statusArchived = store.StatusArchived
)

func orgDir(cfg config) string      { return store.OwnerDir(cfg.Root, cfg.Org) }
func reposDir(cfg config) string    { return store.ReposDir(cfg.Root, cfg.Org) }
func archivesDir(cfg config) string { return store.ArchivesDir(cfg.Root, cfg.Org) }
func statePath(cfg config) string   { return store.StatePath(cfg.Root, cfg.Org) }
func lockPath(cfg config) string    { return store.LockPath(cfg.Root, cfg.Org) }

// acquireLock takes the per-org lock that serializes every command which
// mutates an org's clones or state (a sync run, and worktree add).
func acquireLock(cfg config) (release func(), err error) {
	return store.AcquireLock(cfg.Root, cfg.Org)
}

// loadState never errors; see store.LoadState.
func loadState(path, org string, stderr io.Writer) state {
	return store.LoadState(path, org, stderr)
}

// saveState writes atomically; see store.SaveState.
func saveState(path string, s state) error {
	return store.SaveState(path, s)
}

func validRepoName(name string) bool {
	return store.ValidRepoName(name)
}
