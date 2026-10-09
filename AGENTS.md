# gh-workspace

This project helps users manage their GitHub repositories as one local workspace: central clones plus
worktrees, for any mix of whole orgs/users and individual repos. It's often convenient to have
repositories available ahead of time, either for reference with coding agents or just to not have to
re-clone information.

See `AIDEV.md` for the full design history and the still-open questions; this file is the short,
load-bearing summary of how the tool actually behaves.

## The workspace model

A repo is **tracked** if either of these is true:

- Its owner is listed under `owners` in the config: every repo `gh repo list <owner>` returns is tracked,
  subject to that owner's own filters (forks, etc.).
- It was explicitly added: listed under `repos` in the config, or brought in by `clone <owner>/<repo>` or
  `worktree add <owner>/<repo> ...`.

`owners`/`repos` in the config are **declarative**: a repo is tracked for as long as the config lists it.
An explicit `clone`/`worktree add` records an **imperative** add in per-owner state, which survives the
repo being dropped from the config. `untrack` clears only that state flag — it refuses if the config
still tracks the repo, naming the config entry to edit instead of silently doing nothing.

Central clones always live at `<root>/<owner>/repos/<repo>`. Worktrees are placed by the configured
`worktreeRoot`/`worktreePath` template (default: the current directory, named after the repo) or an
explicit path — never implicitly created by a plain `clone`.

## Commands

The CLI is installed as a `gh` extension (`gh extension install swanysimon/gh-workspace`):

* `gh workspace [flags] <owner>` / `gh workspace sync [flags] [<owner>[/<repo>]]` - sync every tracked
  repo (no args), one owner's full listing, or one repo
* `gh workspace sync --tracked-only` - skip owner listings; refresh only repos already present locally or
  in state
* `gh workspace clone <owner>/<repo>...` - track each argument, cloning if missing; otherwise the same
  skip/fetch/archive logic as sync
* `gh workspace untrack <owner>/<repo>` - clear an explicit track; never deletes local data
* `gh workspace worktree add <owner>/<repo> <branch> [path]` - clone first if needed (and track), then add
  a worktree. If the repo is archived upstream, fail with a message (after the clone)
* `gh workspace worktree remove <owner>/<repo> <path>` / `worktree remove <path>` - spin down a worktree;
  the path-only form finds the owning repo via the worktree's own `.git` file
* `gh workspace worktree list [<owner>[/<repo>]]` - list worktrees for one repo, one owner, or (no
  argument) the whole workspace

A no-op sync costs one API call per configured owner, plus one batched lookup covering every explicitly
tracked repo outside those owners — never one `gh repo view` per such repo. On each run, for every repo
needing a decision: if it does not exist locally, clone it. If it does, fetch all updates. If it is
archived upstream, make it an archive on the machine (tarball), with an indication of the last work done
to the repository — a JSON manifest next to the tarball (`archives/<name>.json`) recording commit hash,
tags and timestamps together: the default branch's head commit (SHA, commit time, subject), every tag
(name, SHA, creation time), GitHub's `pushedAt`/`archivedAt` timestamps, and the tarball's size and
SHA-256.

## Invariants

- **As little work as possible, tracked per repo, not as an early exit.** Each owner is listed with a
  single `gh repo list` call (or a batched GraphQL lookup for explicitly tracked repos outside any
  listing), and each repository's `pushedAt` is compared against `state.json`: an unchanged repository (or
  one already archived locally) is skipped with no git or network work at all. Stopping at the first
  unchanged repository would be wrong — archiving a repository doesn't move its `pushedAt`, so an early
  exit would miss newly archived repositories (and renames, which are matched by repository ID, not name).
  `--force` ignores the stored state and re-syncs everything.
- **Nothing is ever deleted because it's missing or untracked.** A repo absent from a listing, or dropped
  from the config without ever being explicitly tracked, is reported, never removed locally.
- **A dirty working tree is never touched.** Fetches still happen; the checkout itself is left alone.
- **The user always specifies where a worktree lives** — where the command runs, a configured
  `worktreeRoot`/`worktreePath` template, or an explicit path are the only three ways a worktree's
  location is decided. The central clones stay in one location; worktrees can go anywhere.
- **Archiving a repo with a live worktree prompts first.** If an archived-upstream repo has a live `git
  worktree`, the CLI asks before removing it to proceed with archiving (answers "no" and refuses on an
  unattended run with no terminal on stdin, unless `--yes`). If the user agrees, it removes every worktree
  and then archives as normal.
- **Per-owner locks, never a workspace-wide one.** A workspace-wide sync's tasks share one worker pool,
  but each owner's lock/state is independent: another run holding one owner's lock gets that owner
  skipped and reported, while every other owner still finishes and the run exits non-zero.

## Process

This is a jujutsu repository. Every phase of work — and every independently reviewable step within a
large phase — gets its own dedicated jujutsu commit (`jj new -m "<description>"`) *before* any edits, to
partition concepts from each other and keep everything easier to track and review.
