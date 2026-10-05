// Package store is this tool's on-disk layout: where an owner's clones,
// archives, state file and lock live, and the state file's own load/save
// and repo-name validation. Every function takes root/owner as plain
// strings rather than this repo's config struct, so it has no dependency
// on anything else in the module.
package store

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const Version = 1

type Status string

const (
	StatusCloned   Status = "cloned"
	StatusArchived Status = "archived"
)

type State struct {
	Version   int                  `json:"version"`
	Org       string               `json:"org"`
	UpdatedAt time.Time            `json:"updatedAt"`
	Repos     map[string]RepoState `json:"repos"`
}

// RepoState.PushedAt is written only after a fully successful sync of that
// repo, so a failed repo is automatically eligible again next run with no
// separate retry bookkeeping.
type RepoState struct {
	ID          string    `json:"id"`
	PushedAt    time.Time `json:"pushedAt"`
	SyncedAt    time.Time `json:"syncedAt"`
	Status      Status    `json:"status"`
	ArchivePath string    `json:"archivePath,omitempty"`
}

func OwnerDir(root, owner string) string    { return filepath.Join(root, owner) }
func ReposDir(root, owner string) string    { return filepath.Join(OwnerDir(root, owner), "repos") }
func ArchivesDir(root, owner string) string { return filepath.Join(OwnerDir(root, owner), "archives") }
func StatePath(root, owner string) string   { return filepath.Join(OwnerDir(root, owner), "state.json") }
func LockPath(root, owner string) string    { return filepath.Join(OwnerDir(root, owner), "lock") }

// AcquireLock takes the per-owner lock that serializes every command which
// mutates an owner's clones or state (a sync run, and worktree add). It
// fails rather than waits: a held lock usually means a long sync run, and a
// stale one from a crashed run needs a human to delete it.
func AcquireLock(root, owner string) (release func(), err error) {
	lp := LockPath(root, owner)
	f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("another gh-org-clone run appears to be in progress for org %q (lock file %s exists; delete it if a previous run died): %w", owner, lp, err)
	}
	fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	f.Close()
	return func() { os.Remove(lp) }, nil
}

// LoadState never errors: a missing file yields an empty state, and a file
// that fails to parse or carries the wrong version warns and yields an
// empty state. Losing the cache costs one re-verify pass and destroys
// nothing, which is strictly better than failing the run.
func LoadState(path, owner string, stderr io.Writer) State {
	empty := State{Version: Version, Org: owner, Repos: map[string]RepoState{}}

	data, err := os.ReadFile(path)
	if err != nil {
		return empty
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		fmt.Fprintf(stderr, "warning: state file %s is corrupt, starting fresh: %v\n", path, err)
		return empty
	}
	if s.Version != Version {
		fmt.Fprintf(stderr, "warning: state file %s has version %d, expected %d, starting fresh\n", path, s.Version, Version)
		return empty
	}
	if s.Repos == nil {
		s.Repos = map[string]RepoState{}
	}
	return s
}

// SaveState writes atomically: temp file in the same directory, fsync,
// close, rename, then fsync the directory so the rename itself is durable.
func SaveState(path string, s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "state-*.json")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp state file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("renaming state file into place: %w", err)
	}

	dirHandle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening state dir to sync: %w", err)
	}
	defer dirHandle.Close()
	if err := dirHandle.Sync(); err != nil {
		return fmt.Errorf("syncing state dir: %w", err)
	}
	return nil
}

// repoNamePattern is a trust boundary: the name comes from the GitHub API
// and becomes a path segment and a git argument. The first character may
// be alphanumeric or "." (GitHub's own ".github" repo convention) but
// never "-", which blocks the name from being read by git as a flag.
var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9.][A-Za-z0-9._-]*$`)

func ValidRepoName(name string) bool {
	if name == "." || name == ".." {
		return false
	}
	return repoNamePattern.MatchString(name)
}
