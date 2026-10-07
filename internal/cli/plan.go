package cli

import "github.com/swanysimon/gh-org-clone/internal/plan"

// action and the actionX constants are aliases for plan.Action/plan's
// constants, so every existing comparison and switch case elsewhere in the
// codebase (buildTasks, processTask, archive.go, every test file) keeps
// compiling and comparing correctly unchanged.
type action = plan.Action

const (
	actionSkip          = plan.Skip
	actionClone         = plan.Clone
	actionFetch         = plan.Fetch
	actionArchive       = plan.Archive
	actionAdoptArchived = plan.AdoptArchived
	actionUnarchive     = plan.Unarchive
	actionNotARepo      = plan.NotARepo
)

// decide is a thin shim over plan.Decide, which now holds the real decision
// logic (plan.go's old content, moved verbatim, taking plain RepoFacts/
// PrevState/Options instead of ghRepo/repoState/config). The known &&
// prev.Status == statusX mapping below is the one piece of real logic this
// shim has of its own.
func decide(repo ghRepo, prev repoState, known, dirExists, isGitDir, archiveExists bool, cfg config) (action, string) {
	return plan.Decide(
		plan.RepoFacts{Archived: repo.IsArchived, PushedAt: repo.PushedAt},
		plan.PrevState{
			Known:    known,
			Archived: known && prev.Status == statusArchived,
			Cloned:   known && prev.Status == statusCloned,
			PushedAt: prev.PushedAt,
		},
		dirExists, isGitDir, archiveExists,
		plan.Options{Force: cfg.Force, Archive: cfg.Archive},
	)
}
