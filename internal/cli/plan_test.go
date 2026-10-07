package cli

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	baseCfg := defaultConfig()
	forceCfg := baseCfg
	forceCfg.Force = true
	noArchiveCfg := baseCfg
	noArchiveCfg.Archive = false

	cases := []struct {
		name          string
		repo          ghRepo
		prev          repoState
		known         bool
		dirExists     bool
		isGitDir      bool
		archiveExists bool
		cfg           config
		want          action
	}{
		{
			name:      "fresh unknown repo",
			repo:      ghRepo{PushedAt: t1},
			known:     false,
			dirExists: false,
			isGitDir:  false,
			cfg:       baseCfg,
			want:      actionClone,
		},
		{
			name:      "unchanged known repo",
			repo:      ghRepo{PushedAt: t1},
			prev:      repoState{PushedAt: t1, Status: statusCloned},
			known:     true,
			dirExists: true,
			isGitDir:  true,
			cfg:       baseCfg,
			want:      actionSkip,
		},
		{
			name:      "unchanged known repo with force",
			repo:      ghRepo{PushedAt: t1},
			prev:      repoState{PushedAt: t1, Status: statusCloned},
			known:     true,
			dirExists: true,
			isGitDir:  true,
			cfg:       forceCfg,
			want:      actionFetch,
		},
		{
			name:      "changed pushedAt",
			repo:      ghRepo{PushedAt: t2},
			prev:      repoState{PushedAt: t1, Status: statusCloned},
			known:     true,
			dirExists: true,
			isGitDir:  true,
			cfg:       baseCfg,
			want:      actionFetch,
		},
		{
			name:      "known repo whose directory was deleted",
			repo:      ghRepo{PushedAt: t1},
			prev:      repoState{PushedAt: t1, Status: statusCloned},
			known:     true,
			dirExists: false,
			isGitDir:  false,
			cfg:       baseCfg,
			want:      actionClone,
		},
		{
			name:      "directory present without .git",
			repo:      ghRepo{PushedAt: t1},
			known:     false,
			dirExists: true,
			isGitDir:  false,
			cfg:       baseCfg,
			want:      actionNotARepo,
		},
		{
			name:      "newly archived upstream",
			repo:      ghRepo{PushedAt: t1, IsArchived: true},
			prev:      repoState{PushedAt: t1, Status: statusCloned},
			known:     true,
			dirExists: true,
			isGitDir:  true,
			cfg:       baseCfg,
			want:      actionArchive,
		},
		{
			name:          "archived with manifest and no clone",
			repo:          ghRepo{PushedAt: t1, IsArchived: true},
			known:         false,
			dirExists:     false,
			isGitDir:      false,
			archiveExists: true,
			cfg:           baseCfg,
			want:          actionAdoptArchived,
		},
		{
			name:          "already archived and recorded",
			repo:          ghRepo{PushedAt: t1, IsArchived: true},
			prev:          repoState{PushedAt: t1, Status: statusArchived},
			known:         true,
			archiveExists: true,
			cfg:           baseCfg,
			want:          actionSkip,
		},
		{
			name:          "already archived and recorded, with force",
			repo:          ghRepo{PushedAt: t1, IsArchived: true},
			prev:          repoState{PushedAt: t1, Status: statusArchived},
			known:         true,
			archiveExists: true,
			cfg:           forceCfg,
			want:          actionAdoptArchived,
		},
		{
			name:  "recorded as archived but archive missing from disk",
			repo:  ghRepo{PushedAt: t1, IsArchived: true},
			prev:  repoState{PushedAt: t1, Status: statusArchived},
			known: true,
			cfg:   baseCfg,
			want:  actionArchive,
		},
		{
			name:      "archived upstream but cfg.Archive false",
			repo:      ghRepo{PushedAt: t1, IsArchived: true},
			known:     false,
			dirExists: false,
			isGitDir:  false,
			cfg:       noArchiveCfg,
			want:      actionClone,
		},
		{
			name:      "previously archived, now live upstream",
			repo:      ghRepo{PushedAt: t2, IsArchived: false},
			prev:      repoState{PushedAt: t1, Status: statusArchived},
			known:     true,
			dirExists: false,
			isGitDir:  false,
			cfg:       baseCfg,
			want:      actionUnarchive,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := decide(tc.repo, tc.prev, tc.known, tc.dirExists, tc.isGitDir, tc.archiveExists, tc.cfg)
			if got != tc.want {
				t.Fatalf("decide() = %q (%s), want %q", got, reason, tc.want)
			}
		})
	}
}
