// Package archive is the archive procedure: tarball a repo's working tree,
// write a manifest recording its head commit and tags, and only then
// delete the clone. It depends on gitcli (to inspect the repo being
// archived) and store (RepoState/Status, since archiving is one of the
// outcomes a sync run records), but takes plain arguments rather than this
// repo's config/ghRepo types, and takes "clone it for me" as an injected
// callback rather than knowing how to construct a clone URL itself — that
// stays the caller's job (gh.go/git.go's shims), so this package doesn't
// also need to depend on ghcli.
package archive

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/swanysimon/gh-workspace/internal/execx"
	"github.com/swanysimon/gh-workspace/internal/gitcli"
	"github.com/swanysimon/gh-workspace/internal/store"
)

const ManifestVersion = 1

type Manifest struct {
	Version         int          `json:"version"`
	Org             string       `json:"org"`
	Repo            string       `json:"repo"`
	NameWithOwner   string       `json:"nameWithOwner"`
	URL             string       `json:"url"`
	DefaultBranch   string       `json:"defaultBranch"`
	HeadSHA         string       `json:"headSha"`
	HeadCommittedAt time.Time    `json:"headCommittedAt"`
	HeadSubject     string       `json:"headSubject"`
	Tags            []gitcli.Tag `json:"tags"`
	PushedAt        time.Time    `json:"pushedAt"`
	ArchivedAt      time.Time    `json:"archivedAt"`
	CapturedAt      time.Time    `json:"capturedAt"`
	Tarball         string       `json:"tarball"`
	TarballBytes    int64        `json:"tarballBytes"`
	TarballSHA256   string       `json:"tarballSha256"`
}

// Repo is the subset of repo metadata Archive needs; a caller already
// holding a richer repo type (today, gh.go's ghRepo) picks these fields out
// of it.
type Repo struct {
	ID            string
	Owner         string
	Name          string
	NameWithOwner string
	URL           string
	PushedAt      time.Time
	ArchivedAt    time.Time
	DefaultBranch string
}

// WorktreeStatus is a linked worktree plus whether it currently has
// uncommitted changes, so a removal prompt can warn before discarding them.
type WorktreeStatus struct {
	Path  string
	Dirty bool
}

// ConfirmFunc is the seam Archive uses to ask before removing worktrees,
// instead of touching os.Stdin directly. See DefaultConfirm for the
// production implementation.
type ConfirmFunc func(repoName string, worktrees []WorktreeStatus) (bool, error)

// confirmMu serializes prompts across concurrent callers. Archiving runs
// many repos in parallel; without this, two repos hitting a live worktree
// at once would interleave their prompts into unreadable output.
var confirmMu sync.Mutex

// DefaultConfirm is ConfirmFunc's production implementation. It never
// blocks a non-interactive run: yes=true answers "yes" unconditionally
// (for scripted use where the caller has already decided), and anything
// else answers "no" unless stdin is a terminal, so cron/CI runs fail
// closed instead of hanging forever on a prompt nobody can see.
func DefaultConfirm(yes bool) ConfirmFunc {
	return func(repoName string, worktrees []WorktreeStatus) (bool, error) {
		if yes {
			return true, nil
		}
		if !isInteractive(os.Stdin) {
			return false, nil
		}

		confirmMu.Lock()
		defer confirmMu.Unlock()

		fmt.Fprintf(os.Stderr, "%q has %d live worktree(s):\n", repoName, len(worktrees))
		for _, w := range worktrees {
			suffix := ""
			if w.Dirty {
				suffix = " (uncommitted changes will be discarded)"
			}
			fmt.Fprintf(os.Stderr, "  %s%s\n", w.Path, suffix)
		}
		fmt.Fprint(os.Stderr, "remove them and continue archiving? [y/N] ")

		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return false, err
		}
		line = strings.ToLower(strings.TrimSpace(line))
		return line == "y" || line == "yes", nil
	}
}

// isInteractive is a stdlib-only tty heuristic (no golang.org/x/term
// dependency): a character device is the closest approximation of "there is
// a human who can see and answer this prompt" available without an ioctl,
// except that /dev/null is *also* a character device and is exactly what
// cron and CI redirect stdin from, so it gets an explicit carve-out.
func isInteractive(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if nullInfo, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, nullInfo) {
		return false
	}
	return true
}

// ConfirmAndRemoveWorktrees is a no-op when dir has no linked worktrees.
// When it does, it asks before removing any of them (see DefaultConfirm for
// exactly what "no answer available" resolves to) and, on "no", refuses to
// proceed rather than orphaning them.
func ConfirmAndRemoveWorktrees(ctx context.Context, run execx.Exec, timeout time.Duration, confirm ConfirmFunc, repoName, dir string) error {
	paths, err := gitcli.LinkedWorktrees(ctx, run, timeout, dir)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}

	worktrees := make([]WorktreeStatus, len(paths))
	for i, p := range paths {
		dirty, err := gitcli.IsDirty(ctx, run, timeout, p)
		if err != nil {
			return fmt.Errorf("checking worktree %q of %q: %w", p, repoName, err)
		}
		worktrees[i] = WorktreeStatus{Path: p, Dirty: dirty}
	}

	ok, err := confirm(repoName, worktrees)
	if err != nil {
		return fmt.Errorf("confirming archive of %q with live worktrees: %w", repoName, err)
	}
	if !ok {
		return fmt.Errorf("repo %q has %d live worktree(s), refusing to archive; remove them (or rerun with --yes, or interactively and answer yes) to proceed", repoName, len(worktrees))
	}

	for _, w := range worktrees {
		if _, err := execx.WithTimeout(ctx, timeout, run, dir, "git", "worktree", "remove", "--force", "--", w.Path); err != nil {
			return fmt.Errorf("removing worktree %q of %q: %w", w.Path, repoName, err)
		}
	}
	return nil
}

func ManifestPath(archivesDir, repoName string) string {
	return filepath.Join(archivesDir, repoName+".json")
}

func TarballPath(archivesDir, repoName string) string {
	return filepath.Join(archivesDir, repoName+".tar.gz")
}

// WriteTarball writes to dest.tmp-<pid> first and renames on success, so a
// crash mid-write can never leave a corrupt file at dest. The checksum is
// computed for free via io.MultiWriter alongside the gzip write.
func WriteTarball(dir, repoName, dest string) (bytesWritten int64, sha256hex string, err error) {
	tmp := dest + ".tmp-" + fmt.Sprint(os.Getpid())
	f, err := os.Create(tmp)
	if err != nil {
		return 0, "", fmt.Errorf("creating temp tarball: %w", err)
	}
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()

	hash := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(f, hash))
	tw := tar.NewWriter(gz)

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}

		info, lerr := os.Lstat(path)
		if lerr != nil {
			return lerr
		}
		mode := info.Mode()

		if mode&(os.ModeSocket|os.ModeDevice|os.ModeNamedPipe|os.ModeCharDevice) != 0 {
			fmt.Fprintf(os.Stderr, "warning: skipping non-regular file %s\n", path)
			return nil
		}

		var link string
		if mode&os.ModeSymlink != 0 {
			link, lerr = os.Readlink(path)
			if lerr != nil {
				return lerr
			}
		}

		hdr, herr := tar.FileInfoHeader(info, link)
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(filepath.Join(repoName, rel))
		if info.IsDir() {
			hdr.Name += "/"
		}
		if werr := tw.WriteHeader(hdr); werr != nil {
			return werr
		}
		if info.Mode().IsRegular() {
			file, oerr := os.Open(path)
			if oerr != nil {
				return oerr
			}
			_, cerr := io.Copy(tw, file)
			file.Close()
			if cerr != nil {
				return cerr
			}
		}
		return nil
	})
	if walkErr != nil {
		tw.Close()
		gz.Close()
		f.Close()
		return 0, "", fmt.Errorf("walking %s: %w", dir, walkErr)
	}

	if err = tw.Close(); err != nil {
		f.Close()
		return 0, "", fmt.Errorf("closing tar writer: %w", err)
	}
	if err = gz.Close(); err != nil {
		f.Close()
		return 0, "", fmt.Errorf("closing gzip writer: %w", err)
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return 0, "", fmt.Errorf("syncing tarball: %w", err)
	}
	if err = f.Close(); err != nil {
		return 0, "", fmt.Errorf("closing tarball: %w", err)
	}
	if err = os.Rename(tmp, dest); err != nil {
		return 0, "", fmt.Errorf("renaming tarball into place: %w", err)
	}

	info, serr := os.Stat(dest)
	if serr != nil {
		return 0, "", fmt.Errorf("stating tarball: %w", serr)
	}
	return info.Size(), hex.EncodeToString(hash.Sum(nil)), nil
}

func WriteManifest(path string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}
	tmp := path + ".tmp-" + fmt.Sprint(os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing temp manifest: %w", err)
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY, 0o600)
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("reopening temp manifest to sync: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("syncing temp manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("closing temp manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming manifest into place: %w", err)
	}
	return nil
}

// LocalArchiveExists reports whether both halves of a repo's local archive
// (manifest and tarball) are on disk. It does not verify the tarball's
// contents; Archive does that before trusting it.
func LocalArchiveExists(archivesDir, repoName string) bool {
	if _, err := os.Stat(ManifestPath(archivesDir, repoName)); err != nil {
		return false
	}
	_, err := os.Stat(TarballPath(archivesDir, repoName))
	return err == nil
}

func ReadManifest(path string) (Manifest, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, false
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, false
	}
	return m, true
}

// Archive tarballs a repo and deletes its clone only after the tarball and
// manifest are both durably on disk — see the ordering below. A dirty
// working tree refuses to archive. clone is called only if dir doesn't
// already exist and no valid manifest+tarball pair is found; it stays the
// caller's job to know how to actually clone the repo (URL, destination,
// temp-dir convention), which this package deliberately doesn't duplicate.
//
// AIDEV: no force override for the dirty-tree refusal; uncommitted work is
// never tarred-and-deleted.
func Archive(ctx context.Context, run execx.Exec, timeout time.Duration, confirm ConfirmFunc, repo Repo, dir, archivesDir string, clone func(ctx context.Context) error) (store.RepoState, []string, error) {
	var notes []string
	tb := TarballPath(archivesDir, repo.Name)
	mp := ManifestPath(archivesDir, repo.Name)

	_, dirErr := os.Stat(dir)
	dirExists := dirErr == nil

	// Step 1: filesystem is truth; state is only a cache.
	if !dirExists {
		if m, ok := ReadManifest(mp); ok {
			if info, err := os.Stat(tb); err == nil && info.Size() == m.TarballBytes {
				return store.RepoState{
					ID:          repo.ID,
					PushedAt:    repo.PushedAt,
					SyncedAt:    time.Now(),
					Status:      store.StatusArchived,
					ArchivePath: "archives/" + repo.Name + ".tar.gz",
				}, notes, nil
			}
		}
		// Step 2: no valid manifest — clone first; archived repos are
		// still readable on GitHub.
		if err := clone(ctx); err != nil {
			return store.RepoState{}, notes, fmt.Errorf("cloning archived repo %q before archiving: %w", repo.Name, err)
		}
	}

	dirty, err := gitcli.IsDirty(ctx, run, timeout, dir)
	if err != nil {
		return store.RepoState{}, notes, err
	}
	if dirty {
		return store.RepoState{}, notes, fmt.Errorf("repo %q has uncommitted changes, refusing to archive", repo.Name)
	}

	// A linked worktree elsewhere on disk survives only as long as this
	// repo's .git directory does; deleting it out from under a worktree
	// leaves that worktree permanently broken with no clean recovery. Ask
	// before doing that, rather than silently orphaning it.
	if err := ConfirmAndRemoveWorktrees(ctx, run, timeout, confirm, repo.Name, dir); err != nil {
		return store.RepoState{}, notes, err
	}

	if err := gitcli.Fetch(ctx, run, timeout, dir); err != nil {
		return store.RepoState{}, notes, err
	}

	sha, committedAt, subject, err := gitcli.HeadInfo(ctx, run, timeout, dir, repo.DefaultBranch)
	if err != nil {
		return store.RepoState{}, notes, err
	}
	tagList, err := gitcli.Tags(ctx, run, timeout, dir)
	if err != nil {
		return store.RepoState{}, notes, err
	}

	// Step 6: resume check — a matching manifest+tarball means the tar
	// work is already done.
	needsTar := true
	if m, ok := ReadManifest(mp); ok && sha != "" && m.HeadSHA == sha {
		if _, err := os.Stat(tb); err == nil {
			needsTar = false
		}
	}

	if needsTar {
		bytesWritten, sum, err := WriteTarball(dir, repo.Name, tb)
		if err != nil {
			return store.RepoState{}, notes, fmt.Errorf("writing tarball for %q: %w", repo.Name, err)
		}
		manifest := Manifest{
			Version:         ManifestVersion,
			Org:             repo.Owner,
			Repo:            repo.Name,
			NameWithOwner:   repo.NameWithOwner,
			URL:             repo.URL,
			DefaultBranch:   repo.DefaultBranch,
			HeadSHA:         sha,
			HeadCommittedAt: committedAt,
			HeadSubject:     subject,
			Tags:            tagList,
			PushedAt:        repo.PushedAt,
			ArchivedAt:      repo.ArchivedAt,
			CapturedAt:      time.Now(),
			Tarball:         repo.Name + ".tar.gz",
			TarballBytes:    bytesWritten,
			TarballSHA256:   sum,
		}
		if err := WriteManifest(mp, manifest); err != nil {
			return store.RepoState{}, notes, fmt.Errorf("writing manifest for %q: %w", repo.Name, err)
		}

		dirHandle, err := os.Open(archivesDir)
		if err != nil {
			return store.RepoState{}, notes, fmt.Errorf("opening archives dir to sync: %w", err)
		}
		syncErr := dirHandle.Sync()
		dirHandle.Close()
		if syncErr != nil {
			return store.RepoState{}, notes, fmt.Errorf("syncing archives dir: %w", syncErr)
		}
	}

	if err := os.RemoveAll(dir); err != nil {
		return store.RepoState{}, notes, fmt.Errorf("removing clone of archived repo %q: %w", repo.Name, err)
	}

	return store.RepoState{
		ID:          repo.ID,
		PushedAt:    repo.PushedAt,
		SyncedAt:    time.Now(),
		Status:      store.StatusArchived,
		ArchivePath: "archives/" + repo.Name + ".tar.gz",
	}, notes, nil
}
