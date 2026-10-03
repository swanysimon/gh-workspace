package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file tests git.go's shims directly (not internal/gitcli's own
// logic, which gitcli_test.go covers against a real git binary), confirming
// cfg's fields actually reach gitcli with the right values -- the same gap
// an independent review found and fixed for gh.go's shims (see
// gh_shim_test.go and AIDEV.md's ghcli entry).

// TestCloneRepoShimComputesDestAndURL is the one gitcli shim with real
// logic of its own (reposDir(cfg)+repo.Name for dest/tmp, cloneURL(repo,
// cfg) for the URL) rather than pure forwarding; every other shim in
// git.go just hands cfg.Timeout/cfg.Deps.exec straight through, with no
// risk of a "right value, wrong field" mix-up the way gh.go's listRepos
// had between cfg.Org and cfg.MaxRepos.
func TestCloneRepoShimComputesDestAndURL(t *testing.T) {
	cfg := defaultConfig()
	cfg.Root = t.TempDir()
	cfg.Org = "shimorg"
	cfg.Protocol = "ssh"
	mustMkReposDir(t, cfg)

	var gotArgs []string
	cfg.Deps.exec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotArgs = args
		// cloneRepo's rename-into-place is a real os.Rename after this
		// (fake) exec call "succeeds", so tmp must really exist as a
		// directory for that rename to succeed -- exactly what a real
		// "git clone" would have left behind.
		tmp := args[len(args)-1]
		return nil, os.MkdirAll(tmp, 0o700)
	}

	repo := ghRepo{Name: "repo1", SSHURL: "git@example.invalid:shimorg/repo1.git", URL: "https://example.invalid/shimorg/repo1"}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatalf("cloneRepo: %v", err)
	}

	argv := strings.Join(gotArgs, " ")
	if !strings.Contains(argv, repo.SSHURL) {
		t.Fatalf("cfg.Protocol=ssh should have selected SSHURL, got args %v", gotArgs)
	}
	tmp := gotArgs[len(gotArgs)-1]
	if !strings.Contains(tmp, "repo1") || !strings.Contains(tmp, reposDir(cfg)) {
		t.Fatalf("tmp path %q is not under reposDir(cfg) and named after the repo", tmp)
	}

	dest := filepath.Join(reposDir(cfg), "repo1")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("clone should have landed at %s: %v", dest, err)
	}
}

func TestCloneRepoShimSelectsHTTPSURL(t *testing.T) {
	cfg := defaultConfig()
	cfg.Root = t.TempDir()
	cfg.Protocol = "https"
	mustMkReposDir(t, cfg)

	var gotArgs []string
	cfg.Deps.exec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, os.MkdirAll(args[len(args)-1], 0o700)
	}

	repo := ghRepo{Name: "repo2", SSHURL: "git@example.invalid:org/repo2.git", URL: "https://example.invalid/org/repo2"}
	if err := cloneRepo(context.Background(), cfg, repo); err != nil {
		t.Fatalf("cloneRepo: %v", err)
	}
	if argv := strings.Join(gotArgs, " "); !strings.Contains(argv, repo.URL) {
		t.Fatalf("cfg.Protocol=https should have selected URL, got args %v", gotArgs)
	}
}

// TestGitShimsForwardDirAndTimeout checks every remaining gitcli shim
// (runGit, fetchRepo, isDirty, updateWorktree, headInfo, tags,
// linkedWorktrees, setRemoteURL) actually passes its dir argument through
// to cfg.Deps.exec, so a copy-paste bug swapping which directory a shim
// operates on would be caught here rather than only by the real-git
// integration tests elsewhere in this package.
func TestGitShimsForwardDirAndTimeout(t *testing.T) {
	const wantDir = "/shim/test/dir"
	cfg := defaultConfig()

	var gotDir string
	cfg.Deps.exec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotDir = dir
		// A plausible-looking success payload for whichever caller is
		// currently invoking exec; each shim below only cares that gotDir
		// was recorded, not about the return value's content.
		return []byte(""), nil
	}

	cases := []struct {
		name string
		call func()
	}{
		{"runGit", func() { runGit(context.Background(), cfg, wantDir, "status") }},
		{"fetchRepo", func() { fetchRepo(context.Background(), cfg, wantDir) }},
		{"isDirty", func() { isDirty(context.Background(), cfg, wantDir) }},
		{"updateWorktree", func() { updateWorktree(context.Background(), cfg, wantDir, "main") }},
		{"headInfo", func() { headInfo(context.Background(), cfg, wantDir, "") }},
		{"tags", func() { tags(context.Background(), cfg, wantDir) }},
		{"linkedWorktrees", func() { linkedWorktrees(context.Background(), cfg, wantDir) }},
		{"setRemoteURL", func() { setRemoteURL(context.Background(), cfg, wantDir, "https://example.invalid/x") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDir = ""
			tc.call()
			if gotDir != wantDir {
				t.Fatalf("%s: exec received dir %q, want %q", tc.name, gotDir, wantDir)
			}
		})
	}
}
