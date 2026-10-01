package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/swanysimon/gh-org-clone/internal/execx"
)

// execCommand delegates to internal/execx.Run, which is now the one real
// implementation of this behavior (GIT_TERMINAL_PROMPT=0 etc. included).
// This indirection exists only until gitcli/ghcli are themselves extracted
// into internal/ (see AIDEV.md Phase 2) and start calling execx directly;
// until then, every git.go/gh.go caller still goes through this name.
func execCommand(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return execx.Run(ctx, dir, name, args...)
}

// runGit runs one git command bounded by cfg.Timeout.
func runGit(ctx context.Context, cfg config, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	return cfg.Deps.exec(ctx, dir, "git", args...)
}

type archiveTag struct {
	Name      string    `json:"name"`
	SHA       string    `json:"sha"`
	CreatedAt time.Time `json:"createdAt"`
}

// cloneRepo clones into a temp sibling directory and renames on success, so
// an interrupted clone can never be mistaken for a complete one. Deliberately
// no --depth, --filter, --single-branch, --bare or --mirror: this tool exists
// to give coding agents a readable offline working tree.
func cloneRepo(ctx context.Context, cfg config, repo ghRepo) error {
	url := cloneURL(repo, cfg)
	if url == "" {
		return fmt.Errorf("repo %q has no clone URL for protocol %q", repo.Name, cfg.Protocol)
	}

	dest := filepath.Join(reposDir(cfg), repo.Name)
	tmp := filepath.Join(reposDir(cfg), ".tmp-"+repo.Name+"-"+strconv.Itoa(os.Getpid()))

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	_, err := cfg.Deps.exec(ctx, "", "git", "clone", "--quiet", "--no-single-branch", "--origin", "origin", "--", url, tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("cloning %q: %w", repo.Name, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("renaming clone of %q into place: %w", repo.Name, err)
	}
	return nil
}

// fetchRepo mirrors upstream refs exactly: --prune and --prune-tags make
// deleted branches and tags disappear locally too.
func fetchRepo(ctx context.Context, cfg config, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	_, err := cfg.Deps.exec(ctx, dir, "git", "fetch", "--quiet", "--all", "--tags", "--prune", "--prune-tags")
	if err != nil {
		return fmt.Errorf("fetching %s: %w", dir, err)
	}
	return nil
}

func isDirty(ctx context.Context, cfg config, dir string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	out, err := cfg.Deps.exec(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("checking status of %s: %w", dir, err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// updateWorktree never runs a destructive git command. Any state other than
// "clean and on the default branch and fast-forwardable" is left untouched
// and reported as a warning instead of an error. The returned dirty flag
// lets a caller decide not to record pushedAt for a dirty repo, so the
// warning repeats every run instead of silently serving a stale tree
// forever.
//
// AIDEV: a repo whose default branch was renamed upstream keeps its old
// checkout until a human runs git switch; upgrade path is a rename-aware
// branch switch.
func updateWorktree(ctx context.Context, cfg config, dir, defaultBranch string) (warning string, dirty bool, err error) {
	if defaultBranch == "" {
		return "", false, nil
	}

	dirty, err = isDirty(ctx, cfg, dir)
	if err != nil {
		return "", false, err
	}
	if dirty {
		return "working tree has uncommitted changes, left untouched", true, nil
	}

	tctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	branchOut, err := cfg.Deps.exec(tctx, dir, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "HEAD is detached, left untouched", false, nil
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch != defaultBranch {
		return fmt.Sprintf("on branch %q instead of default %q, left untouched", branch, defaultBranch), false, nil
	}

	mctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if _, err := cfg.Deps.exec(mctx, dir, "git", "merge", "--ff-only", "--quiet", "refs/remotes/origin/"+defaultBranch); err != nil {
		return fmt.Sprintf("fast-forward merge failed: %v", err), false, nil
	}
	return "", false, nil
}

// headInfo resolves the default branch's remote-tracking commit, falling
// back to HEAD. A repo with no commits returns zero values and no error.
func headInfo(ctx context.Context, cfg config, dir, defaultBranch string) (sha string, committedAt time.Time, subject string, err error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	ref := "HEAD"
	if defaultBranch != "" {
		if out, rerr := cfg.Deps.exec(ctx, dir, "git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+defaultBranch+"^{commit}"); rerr == nil {
			ref = strings.TrimSpace(string(out))
		}
	}

	out, err := cfg.Deps.exec(ctx, dir, "git", "log", "-1", "--format=%H%x00%cI%x00%s", ref)
	if err != nil {
		// No commits reachable from ref (empty repo): not an error.
		return "", time.Time{}, "", nil
	}

	fields := strings.SplitN(strings.TrimRight(string(out), "\n"), "\x00", 3)
	if len(fields) != 3 {
		return "", time.Time{}, "", fmt.Errorf("unexpected git log output for %s: %q", dir, out)
	}
	committedAt, perr := time.Parse(time.RFC3339, fields[1])
	if perr != nil {
		return "", time.Time{}, "", fmt.Errorf("parsing commit time for %s: %w", dir, perr)
	}
	return fields[0], committedAt, fields[2], nil
}

// tags returns an empty slice and no error when the repo has no tags.
func tags(ctx context.Context, cfg config, dir string) ([]archiveTag, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	out, err := cfg.Deps.exec(ctx, dir, "git", "for-each-ref",
		"--format=%(refname:short)%00%(objectname)%00%(creatordate:iso-strict)", "refs/tags")
	if err != nil {
		return nil, fmt.Errorf("listing tags in %s: %w", dir, err)
	}

	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return []archiveTag{}, nil
	}

	var result []archiveTag
	for _, line := range strings.Split(trimmed, "\n") {
		fields := strings.SplitN(line, "\x00", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected git for-each-ref output for %s: %q", dir, line)
		}
		createdAt, perr := time.Parse(time.RFC3339, fields[2])
		if perr != nil {
			return nil, fmt.Errorf("parsing tag creation time for %s: %w", dir, perr)
		}
		result = append(result, archiveTag{Name: fields[0], SHA: fields[1], CreatedAt: createdAt})
	}
	if result == nil {
		result = []archiveTag{}
	}
	return result, nil
}

// linkedWorktrees reports every worktree attached to dir other than dir's
// own primary working tree. "git worktree list --porcelain" always lists the
// primary worktree first, so the first block is skipped positionally rather
// than by comparing paths (which would need symlink-aware resolution).
func linkedWorktrees(ctx context.Context, cfg config, dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	out, err := cfg.Deps.exec(ctx, dir, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("listing worktrees for %s: %w", dir, err)
	}

	var paths []string
	skippedPrimary := false
	for _, line := range strings.Split(string(out), "\n") {
		p, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		if !skippedPrimary {
			skippedPrimary = true
			continue
		}
		paths = append(paths, p)
	}
	return paths, nil
}

func setRemoteURL(ctx context.Context, cfg config, dir, url string) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	if _, err := cfg.Deps.exec(ctx, dir, "git", "remote", "set-url", "origin", "--", url); err != nil {
		return fmt.Errorf("setting remote url for %s: %w", dir, err)
	}
	return nil
}
