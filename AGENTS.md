# gh-workspace

This project helps users manage their GitHub repositories as one local workspace: central clones plus
worktrees, for any mix of whole orgs/users and individual repos. It's often convenient to have
repositories available ahead of time, either for reference with coding agents or just to not have to
re-clone information.

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

A repo is managed with **git** (the default) or **jujutsu**, set by `vcs` globally, per owner
(`owners[].vcs`), or per repo (a `repos[]` entry). jj clones are colocated, so they are valid git repos
too and every read-only git helper still works against them; `internal/vcs` dispatches everything that
*mutates* a clone or worktree, detecting the kind from a clone's own `.jj` directory rather than from
configuration. A jj worktree is a `jj workspace`, which is not a git repo.

## Commands

The CLI is installed as a `gh` extension (`gh extension install swanysimon/gh-workspace`); full flag
documentation lives in README.md, not here:

- `gh workspace [flags] <owner>` / `gh workspace sync [flags] [<owner>[/<repo>]]` - sync every tracked
  repo (no args), one owner's full listing, or one repo
- `gh workspace sync --tracked-only` - skip owner listings; refresh only repos already present locally or
  in state
- `gh workspace clone <owner>/<repo>...` - track each argument, cloning if missing; otherwise the same
  skip/fetch/archive logic as sync
- `gh workspace untrack <owner>/<repo>` - clear an explicit track; never deletes local data
- `gh workspace worktree add <owner>/<repo> <branch> [path]` - clone first if needed (and track), then add
  a worktree. If the repo is archived upstream, fail with a message (after the clone). On a jj repo this
  is `jj workspace add` and **no bookmark is created**: `<branch>` only selects the starting revision,
  and the command prints the `jj bookmark create` line to run
- `gh workspace worktree remove <owner>/<repo> <path>` / `worktree remove <path>` - spin down a worktree;
  the path-only form finds the owning repo via the worktree's own `.git` (or, for a jj workspace,
  `.jj/repo`) file. Its own `--force` means "even if dirty" — unrelated to sync/clone's `--force`
- `gh workspace worktree list [<owner>[/<repo>]]` - list worktrees for one repo, one owner, or (no
  argument) the whole workspace

A no-op sync costs one API call per configured owner, plus one batched lookup covering every explicitly
tracked repo outside those owners — never one `gh repo view` per such repo. On each run, for every repo
needing a decision: if it does not exist locally, clone it. If it does, fetch all updates. If it is
archived upstream, make it an archive on the machine (tarball), with an indication of the last work done
to the repository — a JSON manifest next to the tarball (`archives/<name>.json`) recording commit hash,
tags and timestamps together: the default branch's head commit (SHA, commit time, subject), every tag
(name, SHA, creation time), GitHub's `pushedAt`/`archivedAt` timestamps, and the tarball's size and
SHA-256.

## Project organization

`main.go` only calls `internal/cli.Run`. Everything else is layered so a package only ever depends on
what's below it, never sideways into a peer and never back up into `cli`:

```
cli        command dispatch, flag/env/config resolution, help text — the only package that knows
           command names or flag syntax
settings   the resolvable config (flags/env/file/defaults) and the config file's own shape
engine     orchestration: given repos + previous state, builds tasks (via plan), runs them on a
           worker pool, reports results
plan       pure decision logic — no filesystem, no subprocess, no clock, table-testable with plain
           structs
archive    tarball + manifest + delete, for the one archiving step engine calls into
progress   live per-repo progress on a terminal, plain lines anywhere else
vcs        the git/jj dispatch seam for anything that *mutates* a clone or worktree
ghcli      the only package that calls the gh CLI
gitcli     the only package that runs git
jjcli      the only package that runs jj
store      on-disk layout: state.json, locks, repo-name validation
execx      the only package that actually starts a subprocess
```

Every package below `cli` takes plain arguments (an `execx.Exec`, a timeout, a root/owner string) rather
than this repo's `config`/`settings.Settings` struct, specifically so it has no dependency on anything
above it. Keep new code on that side of the line: if you're tempted to pass `config` into `internal/plan`
or `internal/store`, that's a sign the logic belongs in `engine` or `cli` instead.

Within that shape, prefer the existing package for a thing over adding a new one. A new file in an
existing package beats a new package; a new package is for a genuinely new concern (a new VCS, a new
output format), not a place to put "the next few functions."

## Invariants

- **As little work as possible, tracked per repo, not as an early exit.** Each owner is listed with a
  single `gh repo list` call (or a batched GraphQL lookup for explicitly tracked repos outside any
  listing), and each repository's `pushedAt` is compared against `state.json`: an unchanged repository (or
  one already archived locally) is skipped with no git or network work at all. Stopping at the first
  unchanged repository would be wrong — archiving a repository doesn't move its `pushedAt`, so an early
  exit would miss newly archived repositories (and renames, which are matched by repository ID, not name).
  `--force` ignores the stored state and re-syncs everything.
- **Nothing is ever deleted because it's missing or untracked.** A repo absent from a listing, or dropped
  from the config without ever being explicitly tracked, is reported, never removed locally. GitHub
  answers `NOT_FOUND` identically for a deleted repo and one a token can't see, so the tool records
  *how long* it has been unresolvable (`missingSince`) instead of guessing why, and `--archive-missing`
  is the opt-in action: tarball the clone through the normal archive path, never delete it. "Missing"
  means the listing never returned it — a repo it returned and a filter then excluded (a fork under
  `includeForks: false`) is not missing.
- **A dirty working tree is never touched.** Fetches still happen; the checkout itself is left alone.
- **The user always specifies where a worktree lives** — where the command runs, a configured
  `worktreeRoot`/`worktreePath` template, or an explicit path are the only three ways a worktree's
  location is decided. The central clones stay in one location; worktrees can go anywhere.
- **Archiving a repo with a live worktree prompts first.** If an archived-upstream repo has a live `git worktree`, the CLI asks before removing it to proceed with archiving (answers "no" and refuses on an
  unattended run with no terminal on stdin, unless `--yes`). If the user agrees, it removes every worktree
  and then archives as normal.
- **Which VCS manages a clone is detected, not configured.** A clone holding `.jj` is managed with jj
  whatever `vcs` says, so changing the setting never invalidates what is on disk and a workspace can
  hold both kinds. `vcs` decides only what a new clone is, and whether to convert an existing git clone.
- **A dirty clone is never adopted into jj.** `jj git init --colocate` snapshots the working tree into
  `@` and stages it in git's index, so converting a clone with uncommitted changes would be touching
  them. It is reported and skipped, repeating every run, like every other dirty-tree case.
- **A jj workspace is only ever removed by name, through its own repo.** `jj workspace forget` leaves
  the directory behind, so this tool does the deletion itself — and only for a path jj reported as one
  of that clone's own workspaces.
- **Per-owner locks, never a workspace-wide one.** A workspace-wide sync's tasks share one worker pool,
  but each owner's lock/state is independent: another run holding one owner's lock gets that owner
  skipped and reported, while every other owner still finishes and the run exits non-zero.
- **Nothing writes to stderr while a progress tracker is live.** The live block redraws by moving
  the cursor up over its own rows, so any other output in between gets overwritten or leaves
  stale rows behind. Print before `progress.New` or after `Stop`; anything that has to talk to the
  terminal mid-run (the archive confirmation prompt) goes through `Tracker.Pause`.
- **Never guess at config intent.** An unknown config key, a malformed `owners`/`repos` entry, or a bad
  flag value is a hard error, not a best-effort fallback. This is deliberate and should hold for any new
  setting too.

## Verification

Before calling any change done, run all three — on this codebase they take under a minute combined, so
there's no reason to skip them or to defer them to CI:

```sh
gofmt -l .              # must print nothing
go vet ./...
go test -count=1 ./...
```

CI (`.github/workflows/ci.yml`) runs the same three plus `-race`, on Linux and macOS, with `jj`
installed. `-race` is slower; run it yourself before opening a PR if you touched anything concurrent
(the worker pool in `engine`, the per-owner locking), otherwise it's fine to let CI catch it. A test that
drives real `jj` skips itself when `jj` isn't on `PATH` — don't rely on that test alone for jj-specific
behavior if you don't have `jj` installed locally.

A change isn't finished until `gofmt` is silent and `go test` is green. Not "finished modulo a follow-up
to fix formatting" — actually done.

## Testing the CLI surface

`internal/cli/pin_test.go` pins the CLI's entire visible surface — every command's exact `--help` text
(usage line, flag list, descriptions, defaults), exit codes, and usage-error output — against the real
entry points. This exists so that changing a flag's name, default, help text, or which command accepts
it is never an accident: the test fails, by design, the moment the output changes.

That means:

- Adding, removing, renaming, or re-scoping a flag (which commands accept it — see `settingsTable` in
  `internal/settings/settings.go`) must update the matching pin test in the same change. A pin test
  failure is either "yes, I meant to change that, here's the updated expectation" or a caught regression
  — never something to silence by weakening the assertion.
- A new command or subcommand needs its own pin coverage for its `--help` output and exit codes before
  it counts as tested, not just a happy-path test of its behavior.
- This is deliberately a drive-the-real-binary-and-diff-the-output kind of test, not output sampled from
  internals — it's the only way it actually catches what a user or script would see change.

The same principle applies one level down: `internal/plan` is pure decision logic, so give it
table-driven tests over plain structs rather than fixtures. Prefer driving real `git`/`jj` subprocesses in
tests (as the existing suite does) over mocking them; `execx.Exec` is there to stub out `gh` specifically
(network calls, no local binary needed to install as a precondition), not to replace git/jj behavior that
the real binary would just as easily exercise.

## Comments

Default to none. Code should read from names and structure, not from prose next to it. Write a comment
only when it records something the code can't say for itself: a non-obvious *why* — a constraint from an
external tool's behavior, an invariant that would be easy to break by "simplifying," a deliberate
difference from what looks like the obvious approach. Never write a comment that just restates what the
line already says.

If you're rewriting or touching a function that's carrying an old comment explaining *what* it does
rather than *why*, feel free to delete it rather than preserve it out of caution.

## Making changes

Keep diffs small and scoped to one concern. If a change is naturally two ideas (e.g. a refactor plus a
new feature, or a fix plus a drive-by cleanup), split it into two commits — or two reviewable steps —
rather than bundling them because they happen to touch the same file.

Commit messages are short and plain: `type(scope): what changed`, imperative, one line, matching the
existing history (`feat(sync): record and act on repos missing upstream`, `fix(rename): repair linked
working copies after a clone moves`). The scope is usually the package or command most affected. Save the
"why" for the commit body only when the one-line summary genuinely can't carry it — most changes here
don't need one.

This is a jujutsu repository. Every phase of work — and every independently reviewable step within a
large phase — gets its own dedicated jujutsu commit (`jj new -m "<description>"`) *before* any edits, to
partition concepts from each other and keep everything easier to track and review.
