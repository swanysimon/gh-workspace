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

const Version = 2

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
//
// Tracked records whether this repo was *explicitly* added -- via `clone`,
// `worktree add`, or being named in the config's `repos` list -- as
// opposed to being swept in only because its owner is configured for a
// full sync. It is one of the two inputs to IsTracked (the other being
// "is this repo's owner or the repo itself still configured right now"),
// and it is sticky: once a repo has been explicitly added, it stays
// tracked even if it's later dropped from the config, until something
// calls Untrack on it. A v1 state file predates this field; LoadState's
// migration sets it true for every repo already in a v1 file (see
// LoadState's doc comment for why true, not false, is the safe default).
type RepoState struct {
	ID          string    `json:"id"`
	PushedAt    time.Time `json:"pushedAt"`
	SyncedAt    time.Time `json:"syncedAt"`
	Status      Status    `json:"status"`
	ArchivePath string    `json:"archivePath,omitempty"`
	Tracked     bool      `json:"tracked"`
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
		return nil, fmt.Errorf("another gh-workspace run appears to be in progress for org %q (lock file %s exists; delete it if a previous run died): %w", owner, lp, err)
	}
	fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	f.Close()
	return func() { os.Remove(lp) }, nil
}

// LoadState never errors: a missing file yields an empty state, a file
// that fails to parse is backed up next to itself (so nothing is silently
// lost) and yields an empty state, and a file with an unrecognized version
// (newer than this binary knows, or garbage) warns and yields an empty
// state. Losing the cache costs one re-verify pass and destroys nothing,
// which is strictly better than failing the run.
//
// A v1 file (Version == 1, from before RepoState.Tracked existed) is
// migrated in place: every repo already in it becomes Tracked = true, not
// false. v1 had no owners/repos config and no explicit/implicit
// distinction at all -- every entry in a v1 file came from either a full
// owner sync or a `worktree add` that predates this field -- so there is
// no way to correctly reconstruct which v1 entries would have been
// "explicit" under the new rule. Erring toward Tracked = true means a
// repo that already had real local data before upgrading keeps being
// synced after upgrading, even if its owner isn't configured; erring
// toward false risks `sync --tracked-only` silently stopping work on a
// repo someone is actively using, which is a worse failure than some
// redundant syncing.
func LoadState(path, owner string, stderr io.Writer) State {
	empty := State{Version: Version, Org: owner, Repos: map[string]RepoState{}}

	data, err := os.ReadFile(path)
	if err != nil {
		return empty
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		backupPath := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if werr := os.WriteFile(backupPath, data, 0o600); werr != nil {
			fmt.Fprintf(stderr, "warning: state file %s is corrupt, starting fresh: %v (also failed to back up the corrupt file: %v)\n", path, err, werr)
		} else {
			fmt.Fprintf(stderr, "warning: state file %s is corrupt, starting fresh: %v (corrupt file backed up to %s)\n", path, err, backupPath)
		}
		return empty
	}

	switch s.Version {
	case Version:
		// current version, nothing to do
	case 1:
		for name, rs := range s.Repos {
			rs.Tracked = true
			s.Repos[name] = rs
		}
		s.Version = Version
		fmt.Fprintf(stderr, "notice: migrated state file %s from version 1 to %d; every repo already in it is now explicitly tracked (see AIDEV.md)\n", path, Version)
	default:
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

// IsTracked is the single rule every "never delete, never silently stop
// syncing" invariant in this tool rests on: a repo is tracked if its
// owner is configured for a full sync (ownerConfigured), if the repo
// itself is explicitly named in the config right now (repoConfigured), or
// if it was explicitly added at some point in the past and nothing has
// untracked it since (explicit, from RepoState.Tracked) -- even if
// neither config-based reason applies any more today. Dropping a repo
// from the config does not stop it from being tracked if it was ever
// explicitly added; only Untrack does that.
func IsTracked(ownerConfigured, repoConfigured, explicit bool) bool {
	return ownerConfigured || repoConfigured || explicit
}
