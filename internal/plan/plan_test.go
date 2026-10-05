package plan

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	baseOpts := Options{Archive: true}
	forceOpts := Options{Archive: true, Force: true}
	noArchiveOpts := Options{Archive: false}

	cases := []struct {
		name          string
		repo          RepoFacts
		prev          PrevState
		dirExists     bool
		isGitDir      bool
		archiveExists bool
		opts          Options
		want          Action
	}{
		{
			name:      "fresh unknown repo",
			repo:      RepoFacts{PushedAt: t1},
			dirExists: false,
			isGitDir:  false,
			opts:      baseOpts,
			want:      Clone,
		},
		{
			name:      "unchanged known repo",
			repo:      RepoFacts{PushedAt: t1},
			prev:      PrevState{Known: true, Cloned: true, PushedAt: t1},
			dirExists: true,
			isGitDir:  true,
			opts:      baseOpts,
			want:      Skip,
		},
		{
			name:      "unchanged known repo with force",
			repo:      RepoFacts{PushedAt: t1},
			prev:      PrevState{Known: true, Cloned: true, PushedAt: t1},
			dirExists: true,
			isGitDir:  true,
			opts:      forceOpts,
			want:      Fetch,
		},
		{
			name:      "changed pushedAt",
			repo:      RepoFacts{PushedAt: t2},
			prev:      PrevState{Known: true, Cloned: true, PushedAt: t1},
			dirExists: true,
			isGitDir:  true,
			opts:      baseOpts,
			want:      Fetch,
		},
		{
			name:      "known repo whose directory was deleted",
			repo:      RepoFacts{PushedAt: t1},
			prev:      PrevState{Known: true, Cloned: true, PushedAt: t1},
			dirExists: false,
			isGitDir:  false,
			opts:      baseOpts,
			want:      Clone,
		},
		{
			name:      "directory present without .git",
			repo:      RepoFacts{PushedAt: t1},
			dirExists: true,
			isGitDir:  false,
			opts:      baseOpts,
			want:      NotARepo,
		},
		{
			name:      "newly archived upstream",
			repo:      RepoFacts{PushedAt: t1, Archived: true},
			prev:      PrevState{Known: true, Cloned: true, PushedAt: t1},
			dirExists: true,
			isGitDir:  true,
			opts:      baseOpts,
			want:      Archive,
		},
		{
			name:          "archived with manifest and no clone",
			repo:          RepoFacts{PushedAt: t1, Archived: true},
			dirExists:     false,
			isGitDir:      false,
			archiveExists: true,
			opts:          baseOpts,
			want:          AdoptArchived,
		},
		{
			name:          "already archived and recorded",
			repo:          RepoFacts{PushedAt: t1, Archived: true},
			prev:          PrevState{Known: true, Archived: true, PushedAt: t1},
			archiveExists: true,
			opts:          baseOpts,
			want:          Skip,
		},
		{
			name:          "already archived and recorded, with force",
			repo:          RepoFacts{PushedAt: t1, Archived: true},
			prev:          PrevState{Known: true, Archived: true, PushedAt: t1},
			archiveExists: true,
			opts:          forceOpts,
			want:          AdoptArchived,
		},
		{
			name: "recorded as archived but archive missing from disk",
			repo: RepoFacts{PushedAt: t1, Archived: true},
			prev: PrevState{Known: true, Archived: true, PushedAt: t1},
			opts: baseOpts,
			want: Archive,
		},
		{
			name:      "archived upstream but opts.Archive false",
			repo:      RepoFacts{PushedAt: t1, Archived: true},
			dirExists: false,
			isGitDir:  false,
			opts:      noArchiveOpts,
			want:      Clone,
		},
		{
			name:      "previously archived, now live upstream",
			repo:      RepoFacts{PushedAt: t2, Archived: false},
			prev:      PrevState{Known: true, Archived: true, PushedAt: t1},
			dirExists: false,
			isGitDir:  false,
			opts:      baseOpts,
			want:      Unarchive,
		},
		// Not present in the original root-level plan_test.go: a case
		// where prev.Known is true but neither Archived nor Cloned is set
		// (the shim never produces this -- a known repo's status is always
		// one or the other -- but Decide itself has no way to enforce
		// that, so it's worth pinning what happens if a future caller
		// violates the assumption: fall through to the normal
		// clone/fetch decision based on dirExists, not crash or
		// misbehave).
		{
			name:      "known but neither archived nor cloned (shouldn't happen, but shouldn't crash either)",
			repo:      RepoFacts{PushedAt: t1},
			prev:      PrevState{Known: true, PushedAt: t1},
			dirExists: true,
			isGitDir:  true,
			opts:      baseOpts,
			want:      Fetch,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := Decide(tc.repo, tc.prev, tc.dirExists, tc.isGitDir, tc.archiveExists, tc.opts)
			if got != tc.want {
				t.Fatalf("Decide() = %q (%s), want %q", got, reason, tc.want)
			}
		})
	}
}
