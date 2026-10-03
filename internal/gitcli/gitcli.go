// Package gitcli is the one place that runs git. Every function takes an
// execx.Exec and a timeout explicitly, rather than this repo's config
// struct, so it has no dependency on anything else in this module. URL and
// path resolution (which repo, which directory, which clone URL) stay the
// caller's job — this package only runs the git commands once a caller has
// already decided what to run them against.
package gitcli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/swanysimon/gh-org-clone/internal/execx"
)

// Tag is one entry from Tags, in the shape an archive manifest records.
type Tag struct {
	Name      string    `json:"name"`
	SHA       string    `json:"sha"`
	CreatedAt time.Time `json:"createdAt"`
}

// Run runs one git command bounded by timeout.
func Run(ctx context.Context, run execx.Exec, timeout time.Duration, dir string, args ...string) ([]byte, error) {
	return execx.WithTimeout(ctx, timeout, run, dir, "git", args...)
}

// Clone clones url into tmp and renames it to dest on success, so an
// interrupted clone can never be mistaken for a complete one. Deliberately
// no --depth, --filter, --single-branch, --bare or --mirror: this tool
// exists to give coding agents a readable offline working tree. The caller
// picks tmp (conventionally a sibling of dest) and is responsible for
// removing it if some other step fails first; Clone only removes it itself
// on its own failure.
func Clone(ctx context.Context, run execx.Exec, timeout time.Duration, url, dest, tmp string) error {
	_, err := execx.WithTimeout(ctx, timeout, run, "", "git", "clone", "--quiet", "--no-single-branch", "--origin", "origin", "--", url, tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("cloning %q: %w", url, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("renaming clone of %q into place: %w", url, err)
	}
	return nil
}

// Fetch mirrors upstream refs exactly: --prune and --prune-tags make
// deleted branches and tags disappear locally too.
func Fetch(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) error {
	_, err := execx.WithTimeout(ctx, timeout, run, dir, "git", "fetch", "--quiet", "--all", "--tags", "--prune", "--prune-tags")
	if err != nil {
		return fmt.Errorf("fetching %s: %w", dir, err)
	}
	return nil
}

func IsDirty(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) (bool, error) {
	out, err := execx.WithTimeout(ctx, timeout, run, dir, "git", "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("checking status of %s: %w", dir, err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// UpdateWorktree never runs a destructive git command. Any state other than
// "clean and on the default branch and fast-forwardable" is left untouched
// and reported as a warning instead of an error. The returned dirty flag
// lets a caller decide not to record a repo's pushedAt when dirty, so the
// warning repeats every run instead of silently serving a stale tree
// forever.
//
// AIDEV: a repo whose default branch was renamed upstream keeps its old
// checkout until a human runs git switch; upgrade path is a rename-aware
// branch switch.
func UpdateWorktree(ctx context.Context, run execx.Exec, timeout time.Duration, dir, defaultBranch string) (warning string, dirty bool, err error) {
	if defaultBranch == "" {
		return "", false, nil
	}

	dirty, err = IsDirty(ctx, run, timeout, dir)
	if err != nil {
		return "", false, err
	}
	if dirty {
		return "working tree has uncommitted changes, left untouched", true, nil
	}

	branchOut, err := execx.WithTimeout(ctx, timeout, run, dir, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "HEAD is detached, left untouched", false, nil
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch != defaultBranch {
		return fmt.Sprintf("on branch %q instead of default %q, left untouched", branch, defaultBranch), false, nil
	}

	if _, err := execx.WithTimeout(ctx, timeout, run, dir, "git", "merge", "--ff-only", "--quiet", "refs/remotes/origin/"+defaultBranch); err != nil {
		return fmt.Sprintf("fast-forward merge failed: %v", err), false, nil
	}
	return "", false, nil
}

// HeadInfo resolves the default branch's remote-tracking commit, falling
// back to HEAD. A repo with no commits returns zero values and no error.
func HeadInfo(ctx context.Context, run execx.Exec, timeout time.Duration, dir, defaultBranch string) (sha string, committedAt time.Time, subject string, err error) {
	ref := "HEAD"
	if defaultBranch != "" {
		if out, rerr := execx.WithTimeout(ctx, timeout, run, dir, "git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+defaultBranch+"^{commit}"); rerr == nil {
			ref = strings.TrimSpace(string(out))
		}
	}

	out, err := execx.WithTimeout(ctx, timeout, run, dir, "git", "log", "-1", "--format=%H%x00%cI%x00%s", ref)
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

// Tags returns an empty slice and no error when the repo has no tags.
func Tags(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) ([]Tag, error) {
	out, err := execx.WithTimeout(ctx, timeout, run, dir, "git", "for-each-ref",
		"--format=%(refname:short)%00%(objectname)%00%(creatordate:iso-strict)", "refs/tags")
	if err != nil {
		return nil, fmt.Errorf("listing tags in %s: %w", dir, err)
	}

	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return []Tag{}, nil
	}

	var result []Tag
	for _, line := range strings.Split(trimmed, "\n") {
		fields := strings.SplitN(line, "\x00", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected git for-each-ref output for %s: %q", dir, line)
		}
		createdAt, perr := time.Parse(time.RFC3339, fields[2])
		if perr != nil {
			return nil, fmt.Errorf("parsing tag creation time for %s: %w", dir, perr)
		}
		result = append(result, Tag{Name: fields[0], SHA: fields[1], CreatedAt: createdAt})
	}
	if result == nil {
		result = []Tag{}
	}
	return result, nil
}

// LinkedWorktrees reports every worktree attached to dir other than dir's
// own primary working tree. "git worktree list --porcelain" always lists
// the primary worktree first, so the first block is skipped positionally
// rather than by comparing paths (which would need symlink-aware
// resolution).
func LinkedWorktrees(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) ([]string, error) {
	out, err := execx.WithTimeout(ctx, timeout, run, dir, "git", "worktree", "list", "--porcelain")
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

func SetRemoteURL(ctx context.Context, run execx.Exec, timeout time.Duration, dir, url string) error {
	if _, err := execx.WithTimeout(ctx, timeout, run, dir, "git", "remote", "set-url", "origin", "--", url); err != nil {
		return fmt.Errorf("setting remote url for %s: %w", dir, err)
	}
	return nil
}
