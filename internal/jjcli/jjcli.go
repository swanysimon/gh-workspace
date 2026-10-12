// Package jjcli is the one place that runs jj, exactly as internal/gitcli
// is the one place that runs git. Every function takes an execx.Exec and a
// timeout explicitly, rather than this repo's config struct, so it has no
// dependency on anything else in this module. URL and path resolution
// (which repo, which directory, which clone URL) stay the caller's job —
// this package only runs the jj commands once a caller has already decided
// what to run them against.
//
// Clones are always colocated (--colocate), so a jj-managed clone is also
// a valid git repo: every read-only git helper in gitcli (HeadInfo, Tags)
// and every ".git exists" check elsewhere keeps working against it
// unchanged, and only mutations need to come through here. --colocate is
// jj's own default as of 0.45 but is passed explicitly anyway: it is
// accepted by older versions and it overrides a user's own
// "git.colocate = false".
package jjcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swanysimon/gh-workspace/internal/execx"
)

// fieldSep separates fields within one line of a -T template's output. jj
// templates accept "\0", and neither a bookmark name nor a path can
// contain a NUL byte, so unlike "|" or a tab it cannot appear inside a
// field value.
const fieldSep = "\x00"

// Workspace is one entry from Workspaces: a jj workspace attached to a
// repo. Dirtiness is deliberately not part of this — jj only snapshots the
// working copy of the workspace a command runs in, so another workspace's
// recorded commit can be stale. Callers that need it run IsDirty against
// the workspace's own Path, the same way the git path runs git status in
// each linked worktree.
type Workspace struct {
	Name string
	Path string
}

// Run runs one jj command bounded by timeout.
func Run(ctx context.Context, run execx.Exec, timeout time.Duration, dir string, args ...string) ([]byte, error) {
	return execx.WithTimeout(ctx, timeout, run, dir, "jj", args...)
}

// Clone clones url into tmp and renames it to dest on success, so an
// interrupted clone can never be mistaken for a complete one — the same
// contract (and the same caller responsibilities) as gitcli.Clone. A jj
// clone's own .jj/repo is a directory rather than a path reference, so
// renaming the finished clone into place is safe.
func Clone(ctx context.Context, run execx.Exec, timeout time.Duration, url, dest, tmp string) error {
	if _, err := Run(ctx, run, timeout, "", "git", "clone", "--colocate", "--quiet", "--", url, tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("cloning %q with jj: %w", url, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("renaming jj clone of %q into place: %w", url, err)
	}
	return nil
}

// Adopt converts an existing git clone in dir into a colocated jj repo in
// place. The caller must have already established that dir is not dirty:
// "jj git init" snapshots a dirty working tree into @ and stages it in
// git's index, which would be a mutation of the user's uncommitted work.
//
// A freshly adopted repo's remote bookmarks are untracked, so a later
// Fetch would not advance them; tracking the default branch's remote
// bookmark is what makes an adopted clone behave like a cloned one. That
// step failing (no such bookmark, already tracked) is a warning, not a
// failure: the repo is colocated either way.
func Adopt(ctx context.Context, run execx.Exec, timeout time.Duration, dir, defaultBranch string) (warning string, err error) {
	if _, err := Run(ctx, run, timeout, dir, "git", "init", "--colocate"); err != nil {
		return "", fmt.Errorf("initializing jj in %s: %w", dir, err)
	}
	if defaultBranch == "" {
		return "", nil
	}
	if _, err := Run(ctx, run, timeout, dir, "bookmark", "track", defaultBranch+"@origin"); err != nil {
		return fmt.Sprintf("adopted into jj, but could not track %s@origin: %v", defaultBranch, err), nil
	}
	return "", nil
}

// Fetch mirrors upstream the way gitcli.Fetch's --prune/--prune-tags do:
// jj abandons commits the remote no longer reaches and propagates deleted
// remote bookmarks and tags, with no flag needed.
func Fetch(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) error {
	if _, err := Run(ctx, run, timeout, dir, "git", "fetch", "--remote", "origin"); err != nil {
		return fmt.Errorf("fetching %s with jj: %w", dir, err)
	}
	return nil
}

// IsDirty reports whether the working copy at dir holds work. jj has no
// uncommitted state — the working copy is always snapshotted into @ — so
// "dirty" means @ has content of its own: a non-empty diff against its
// parent, or a description someone wrote. Both mean a human is working
// here and nothing should be moved under them.
//
// Unlike every other read in this package this deliberately does not pass
// --ignore-working-copy: taking the snapshot is the measurement.
func IsDirty(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) (bool, error) {
	out, err := Run(ctx, run, timeout, dir, "log", "--no-graph", "-r", "@",
		"-T", `empty ++ "\0" ++ if(description, "d", "")`)
	if err != nil {
		return false, fmt.Errorf("checking status of %s: %w", dir, err)
	}
	fields := strings.SplitN(strings.TrimRight(string(out), "\n"), fieldSep, 2)
	if len(fields) != 2 {
		return false, fmt.Errorf("unexpected jj log output for %s: %q", dir, out)
	}
	return fields[0] != "true" || fields[1] != "", nil
}

// UpdateWorkingCopy is gitcli.UpdateWorktree's jj counterpart, with the
// same signature and the same rule: never run a destructive command. Any
// state other than "clean, and one empty change on top of the default
// branch" is left untouched and reported as a warning instead of an error.
//
// A colocated clone's git HEAD is detached, so gitcli.UpdateWorktree would
// warn "HEAD is detached, left untouched" on every run forever — this is
// the one place colocation does not let the git implementation serve both
// kinds of repo.
func UpdateWorkingCopy(ctx context.Context, run execx.Exec, timeout time.Duration, dir, defaultBranch string) (warning string, dirty bool, err error) {
	if defaultBranch == "" {
		return "", false, nil
	}

	dirty, err = IsDirty(ctx, run, timeout, dir)
	if err != nil {
		return "", false, err
	}
	if dirty {
		return "working copy has changes, left untouched", true, nil
	}

	targetIDs, err := Resolve(ctx, run, timeout, dir, RemoteBookmark(defaultBranch))
	if err != nil {
		return "", false, err
	}
	if len(targetIDs) != 1 {
		return fmt.Sprintf("no remote bookmark %q, left untouched", defaultBranch+"@origin"), false, nil
	}

	parentIDs, err := Resolve(ctx, run, timeout, dir, "@-")
	if err != nil {
		return "", false, err
	}
	if len(parentIDs) == 1 && parentIDs[0] == targetIDs[0] {
		return "", false, nil
	}
	if len(parentIDs) > 1 {
		// A merge as the working copy's parent is not a plain reference
		// checkout; moving off it is a judgement call for a human.
		return "working copy sits on a merge, left untouched", false, nil
	}

	if _, err := Run(ctx, run, timeout, dir, "new", targetIDs[0]); err != nil {
		return fmt.Sprintf("could not move the working copy to %s@origin: %v", defaultBranch, err), false, nil
	}
	return "", false, nil
}

// LocalBookmark and RemoteBookmark build the revset that matches exactly
// one bookmark by name. They exist instead of the shorthand "<name>" and
// "<name>@origin" because a name reaches here from a branch argument or
// from GitHub's API, and revsets are an expression language: pasted
// straight in, a name containing an operator would be evaluated rather
// than matched. jj's "exact:" pattern plus escaped string literals mean
// any name at all is matched literally, with nothing to validate or
// reject.
func LocalBookmark(name string) string {
	return `bookmarks(exact:` + quote(name) + `)`
}

func RemoteBookmark(name string) string {
	return `remote_bookmarks(exact:` + quote(name) + `, exact:"origin")`
}

var revsetEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func quote(s string) string {
	return `"` + revsetEscaper.Replace(s) + `"`
}

// Resolve returns every commit a revset resolves to, newest first, or an
// empty slice when it resolves to nothing. A revset naming something that
// does not exist (an absent bookmark) exits non-zero, which is reported as
// "nothing," not as an error — the same pragma gitcli uses for
// "rev-parse --verify --quiet" and for an empty repo in HeadInfo.
func Resolve(ctx context.Context, run execx.Exec, timeout time.Duration, dir, revset string) ([]string, error) {
	out, err := Run(ctx, run, timeout, dir, "log", "--no-graph", "--ignore-working-copy",
		"-r", revset, "-T", `commit_id ++ "\n"`)
	if err != nil {
		return nil, nil
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

// Workspaces reports every workspace attached to the repo at dir other
// than dir's own. gitcli.LinkedWorktrees can skip the primary worktree
// positionally (git documents it first); jj documents no such order, and
// "default" is only a default name that "jj workspace rename" can change,
// so dir's own entry is found by comparing resolved paths instead.
//
// A workspace with no recorded root (created before jj 0.38) is an error
// rather than a skipped line: callers use this to decide whether deleting
// a repo's clone would orphan a working copy, and silently omitting one
// would make that decision wrong in the destructive direction.
func Workspaces(ctx context.Context, run execx.Exec, timeout time.Duration, dir string) ([]Workspace, error) {
	out, err := Run(ctx, run, timeout, dir, "workspace", "list", "--ignore-working-copy",
		"-T", `name ++ "\0" ++ if(root, root, "") ++ "\n"`)
	if err != nil {
		return nil, fmt.Errorf("listing jj workspaces for %s: %w", dir, err)
	}

	self := resolvePath(dir)
	var workspaces []Workspace
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, fieldSep, 2)
		if len(fields) != 2 {
			return nil, fmt.Errorf("unexpected jj workspace list output for %s: %q", dir, line)
		}
		name, root := fields[0], fields[1]
		if root == "" {
			return nil, fmt.Errorf("jj workspace %q of %s has no recorded root path (created by jj before 0.38); remove it by hand", name, dir)
		}
		if resolvePath(root) == self {
			continue
		}
		workspaces = append(workspaces, Workspace{Name: name, Path: root})
	}
	return workspaces, nil
}

// resolvePath makes two paths comparable despite symlinked ancestors (on
// macOS, /tmp -> /private/tmp): jj records a resolved root, while dir is
// whatever string the caller built out of config values. An unresolvable
// path (it may not exist) is cleaned and compared as-is rather than
// failing a lookup that should not care.
func resolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// WorkspaceAdd adds a workspace at path, named name, with its working-copy
// commit starting on top of revset. An empty revset leaves the choice to
// jj (the new workspace shares the current one's parents).
//
// No bookmark is created: in jj a bookmark is named when work is pushed,
// not when it starts, so the branch a caller asked for selects the
// starting revision and nothing more.
func WorkspaceAdd(ctx context.Context, run execx.Exec, timeout time.Duration, dir, name, path, revset string) error {
	args := []string{"workspace", "add", "--name", name}
	if revset != "" {
		args = append(args, "-r", revset)
	}
	args = append(args, "--", path)
	if _, err := Run(ctx, run, timeout, dir, args...); err != nil {
		return fmt.Errorf("adding jj workspace %q at %s: %w", name, path, err)
	}
	return nil
}

// WorkspaceForget stops the repo at dir tracking the named workspace. It
// leaves the workspace's directory on disk: removing it is the caller's
// job, deliberately, so the one destructive step stays where the caller
// can see it.
func WorkspaceForget(ctx context.Context, run execx.Exec, timeout time.Duration, dir, name string) error {
	if _, err := Run(ctx, run, timeout, dir, "workspace", "forget", name); err != nil {
		return fmt.Errorf("forgetting jj workspace %q of %s: %w", name, dir, err)
	}
	return nil
}

func SetRemoteURL(ctx context.Context, run execx.Exec, timeout time.Duration, dir, url string) error {
	if _, err := Run(ctx, run, timeout, dir, "git", "remote", "set-url", "origin", url); err != nil {
		return fmt.Errorf("setting remote url for %s: %w", dir, err)
	}
	return nil
}
