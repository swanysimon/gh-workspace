// Package plan is the pure decision core: given facts about a repo (what
// gh says, what was previously recorded, what's on disk), it decides what
// to do next. No filesystem, no subprocess, no clock, no dependency on any
// other package in this module — every fact arrives as a plain argument,
// so the whole decision matrix stays table-testable without a fixture.
package plan

import "time"

type Action string

const (
	Skip          Action = "skip"
	Clone         Action = "clone"
	Fetch         Action = "fetch"
	Archive       Action = "archive"
	AdoptArchived Action = "adopt-archived" // archive already on disk, just record it
	Unarchive     Action = "unarchive"      // was archived locally, now live upstream
	NotARepo      Action = "not-a-repo"     // dir exists, no .git — report, touch nothing
	// MissingUpstream is for an explicitly tracked repo a batched
	// repository(owner:, name:) lookup reports as gone, renamed without a
	// redirect, or no longer visible -- the explicit-repo counterpart to a
	// repo simply absent from an owner's listing: report it, touch
	// nothing. Decide never returns this itself (it has no RepoFacts for a
	// repo gh can't resolve at all); callers set it directly when a batch
	// lookup's result is nil -- see engine.BuildExplicitTasks.
	MissingUpstream Action = "missing-upstream"
)

// RepoFacts is what gh reports about a repo "right now."
type RepoFacts struct {
	Archived bool
	PushedAt time.Time
}

// PrevState is what was recorded about a repo the last time it was
// resolved, if ever. Archived and Cloned are mutually exclusive in
// practice (a repo is recorded as one or the other, never both), kept as
// two independent bools rather than a shared enum so this package has no
// dependency on whatever status type a caller's store uses — the caller
// (today, gh.go's decide shim) does that mapping.
type PrevState struct {
	Known    bool
	Archived bool
	Cloned   bool
	PushedAt time.Time
}

// Options is the subset of a caller's settings Decide actually needs.
type Options struct {
	Force   bool
	Archive bool
}

// Decide is pure: no filesystem, no subprocess, no clock. All filesystem
// facts arrive as parameters so the whole decision matrix is table-testable.
//
// archiveExists means both the manifest and the tarball are on disk.
func Decide(repo RepoFacts, prev PrevState, dirExists, isGitDir, archiveExists bool, opts Options) (Action, string) {
	if dirExists && !isGitDir {
		return NotARepo, "directory exists but is not a git repository"
	}

	if repo.Archived && opts.Archive {
		if archiveExists && !dirExists {
			// Once recorded, an archive is not re-verified on every run;
			// --force re-checks the tarball against its manifest.
			if prev.Known && !opts.Force && prev.Archived {
				return Skip, "already archived locally"
			}
			return AdoptArchived, "archive already on disk"
		}
		return Archive, "repo is archived upstream"
	}

	if !repo.Archived && prev.Known && prev.Archived {
		return Unarchive, "repo was archived locally but is live upstream again"
	}

	// AIDEV: pushedAt does not move for every conceivable upstream ref
	// change, so -force is the escape hatch; upgrade path is a periodic
	// full ls-remote verification pass.
	if prev.Known && !opts.Force && prev.PushedAt.Equal(repo.PushedAt) && dirExists && isGitDir && prev.Cloned {
		return Skip, "pushedAt unchanged since last sync"
	}

	if !dirExists {
		return Clone, "no local clone exists"
	}

	return Fetch, "local clone exists and may be stale"
}
