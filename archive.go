package main

import (
	"archive/tar"
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
	"time"
)

const manifestVersion = 1

type archiveManifest struct {
	Version         int          `json:"version"`
	Org             string       `json:"org"`
	Repo            string       `json:"repo"`
	NameWithOwner   string       `json:"nameWithOwner"`
	URL             string       `json:"url"`
	DefaultBranch   string       `json:"defaultBranch"`
	HeadSHA         string       `json:"headSha"`
	HeadCommittedAt time.Time    `json:"headCommittedAt"`
	HeadSubject     string       `json:"headSubject"`
	Tags            []archiveTag `json:"tags"`
	PushedAt        time.Time    `json:"pushedAt"`
	ArchivedAt      time.Time    `json:"archivedAt"`
	CapturedAt      time.Time    `json:"capturedAt"`
	Tarball         string       `json:"tarball"`
	TarballBytes    int64        `json:"tarballBytes"`
	TarballSHA256   string       `json:"tarballSha256"`
}

// worktreeStatus is a linked worktree plus whether it currently has
// uncommitted changes, so a removal prompt can warn before discarding them.
type worktreeStatus struct {
	Path  string
	Dirty bool
}

// confirmAndRemoveWorktrees is a no-op when dir has no linked worktrees. When
// it does, it asks before removing any of them (see
// defaultConfirmArchiveWithWorktrees for exactly what "no answer available"
// resolves to) and, on "no", refuses to archive rather than orphaning them.
func confirmAndRemoveWorktrees(ctx context.Context, cfg config, repo ghRepo, dir string) error {
	paths, err := linkedWorktrees(ctx, cfg, dir)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}

	worktrees := make([]worktreeStatus, len(paths))
	for i, p := range paths {
		dirty, err := isDirty(ctx, cfg, p)
		if err != nil {
			return fmt.Errorf("checking worktree %q of %q: %w", p, repo.Name, err)
		}
		worktrees[i] = worktreeStatus{Path: p, Dirty: dirty}
	}

	ok, err := cfg.Deps.confirm(cfg, repo.Name, worktrees)
	if err != nil {
		return fmt.Errorf("confirming archive of %q with live worktrees: %w", repo.Name, err)
	}
	if !ok {
		return fmt.Errorf("repo %q has %d live worktree(s), refusing to archive; remove them (or rerun with --yes, or interactively and answer yes) to proceed", repo.Name, len(worktrees))
	}

	for _, w := range worktrees {
		if _, err := cfg.Deps.exec(ctx, dir, "git", "worktree", "remove", "--force", "--", w.Path); err != nil {
			return fmt.Errorf("removing worktree %q of %q: %w", w.Path, repo.Name, err)
		}
	}
	return nil
}

func manifestPath(cfg config, repoName string) string {
	return filepath.Join(archivesDir(cfg), repoName+".json")
}

func tarballPath(cfg config, repoName string) string {
	return filepath.Join(archivesDir(cfg), repoName+".tar.gz")
}

// writeTarball writes to dest.tmp-<pid> first and renames on success, so a
// crash mid-write can never leave a corrupt file at dest. The checksum is
// computed for free via io.MultiWriter alongside the gzip write.
func writeTarball(dir, repoName, dest string) (bytesWritten int64, sha256hex string, err error) {
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

func writeManifest(path string, m archiveManifest) error {
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

// localArchiveExists reports whether both halves of a repo's local archive
// (manifest and tarball) are on disk. It does not verify the tarball's
// contents; archiveRepo does that before trusting it.
func localArchiveExists(cfg config, repoName string) bool {
	if _, err := os.Stat(manifestPath(cfg, repoName)); err != nil {
		return false
	}
	_, err := os.Stat(tarballPath(cfg, repoName))
	return err == nil
}

func readManifest(path string) (archiveManifest, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return archiveManifest{}, false
	}
	var m archiveManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return archiveManifest{}, false
	}
	return m, true
}

// archiveRepo tarballs a repo and deletes its clone only after the tarball
// and manifest are both durably on disk — see the ordering below. A dirty
// working tree refuses to archive.
//
// AIDEV: no -force override for the dirty-tree refusal; uncommitted work is
// never tarred-and-deleted.
func archiveRepo(ctx context.Context, cfg config, repo ghRepo) (repoState, []string, error) {
	var notes []string
	dir := filepath.Join(reposDir(cfg), repo.Name)
	tb := tarballPath(cfg, repo.Name)
	mp := manifestPath(cfg, repo.Name)

	_, dirErr := os.Stat(dir)
	dirExists := dirErr == nil

	// Step 1: filesystem is truth; state is only a cache.
	if !dirExists {
		if m, ok := readManifest(mp); ok {
			if info, err := os.Stat(tb); err == nil && info.Size() == m.TarballBytes {
				return repoState{
					ID:          repo.ID,
					PushedAt:    repo.PushedAt,
					SyncedAt:    time.Now(),
					Status:      statusArchived,
					ArchivePath: "archives/" + repo.Name + ".tar.gz",
				}, notes, nil
			}
		}
		// Step 2: no valid manifest — clone first; archived repos are
		// still readable on GitHub.
		if err := cloneRepo(ctx, cfg, repo); err != nil {
			return repoState{}, notes, fmt.Errorf("cloning archived repo %q before archiving: %w", repo.Name, err)
		}
	}

	dirty, err := isDirty(ctx, cfg, dir)
	if err != nil {
		return repoState{}, notes, err
	}
	if dirty {
		return repoState{}, notes, fmt.Errorf("repo %q has uncommitted changes, refusing to archive", repo.Name)
	}

	// A linked worktree elsewhere on disk survives only as long as this
	// repo's .git directory does; deleting it out from under a worktree
	// leaves that worktree permanently broken with no clean recovery. Ask
	// before doing that, rather than silently orphaning it.
	if err := confirmAndRemoveWorktrees(ctx, cfg, repo, dir); err != nil {
		return repoState{}, notes, err
	}

	if err := fetchRepo(ctx, cfg, dir); err != nil {
		return repoState{}, notes, err
	}

	defaultBranch := ""
	if repo.DefaultBranch != nil {
		defaultBranch = repo.DefaultBranch.Name
	}
	sha, committedAt, subject, err := headInfo(ctx, cfg, dir, defaultBranch)
	if err != nil {
		return repoState{}, notes, err
	}
	tagList, err := tags(ctx, cfg, dir)
	if err != nil {
		return repoState{}, notes, err
	}

	// Step 6: resume check — a matching manifest+tarball means the tar
	// work is already done.
	needsTar := true
	if m, ok := readManifest(mp); ok && sha != "" && m.HeadSHA == sha {
		if _, err := os.Stat(tb); err == nil {
			needsTar = false
		}
	}

	if needsTar {
		bytesWritten, sum, err := writeTarball(dir, repo.Name, tb)
		if err != nil {
			return repoState{}, notes, fmt.Errorf("writing tarball for %q: %w", repo.Name, err)
		}
		manifest := archiveManifest{
			Version:         manifestVersion,
			Org:             cfg.Org,
			Repo:            repo.Name,
			NameWithOwner:   repo.NameWithOwner,
			URL:             repo.URL,
			DefaultBranch:   defaultBranch,
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
		if err := writeManifest(mp, manifest); err != nil {
			return repoState{}, notes, fmt.Errorf("writing manifest for %q: %w", repo.Name, err)
		}

		dirHandle, err := os.Open(archivesDir(cfg))
		if err != nil {
			return repoState{}, notes, fmt.Errorf("opening archives dir to sync: %w", err)
		}
		syncErr := dirHandle.Sync()
		dirHandle.Close()
		if syncErr != nil {
			return repoState{}, notes, fmt.Errorf("syncing archives dir: %w", syncErr)
		}
	}

	if err := os.RemoveAll(dir); err != nil {
		return repoState{}, notes, fmt.Errorf("removing clone of archived repo %q: %w", repo.Name, err)
	}

	return repoState{
		ID:          repo.ID,
		PushedAt:    repo.PushedAt,
		SyncedAt:    time.Now(),
		Status:      statusArchived,
		ArchivePath: "archives/" + repo.Name + ".tar.gz",
	}, notes, nil
}
