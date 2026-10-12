# gh-workspace

A [`gh`](https://cli.github.com/) extension that keeps GitHub repos mirrored into one local
workspace — whole orgs, individual repos, or both at once — plus worktrees checked out from
those central clones. The point is to have the repos on disk already, ready for a coding agent
or just for you, instead of waiting on a clone every time you need one. Run it again later and it
only does the work that changed: clone what's new, fetch what's already there, tarball anything
archived upstream.

## Install

```sh
gh extension install swanysimon/gh-workspace
```

That pulls a prebuilt binary, so there's no Go toolchain to install. You do need `gh` itself
authenticated (`gh auth login`) and `git` on `PATH`; add `jj` too if you're using
[jujutsu](#using-jujutsu) for any repo.

Upgrading later is the same shape:

```sh
gh extension upgrade swanysimon/gh-workspace
```

<details>
<summary>Building from source instead</summary>

```sh
go build -o gh-workspace .
gh extension install .
```

Run `gh extension install .` from this checkout to point the extension at the binary you just
built — handy while you're developing against a local change.
</details>

## How tracking works

A repo counts as tracked if either of these is true:

- Its owner is listed under `owners` in the config, so everything `gh repo list <owner>` returns
  is tracked (subject to that owner's own filters, like forks).
- It was explicitly added — listed under `repos` in the config, or pulled in by `clone
  <owner>/<repo>` or `worktree add <owner>/<repo> ...`. An explicit add is recorded in state, so
  you never have to hand-edit the config just to remember a one-off repo.

`owners` and `repos` are declarative: a repo stays tracked for as long as the config lists it.
Drop it from the config and tracking stops, unless it was ever explicitly added — in that case
`untrack` is what actually clears it. Nothing gets deleted just because tracking changed; that's
a separate decision, covered below.

## Commands

```
gh workspace [flags] <owner>
gh workspace sync [flags] [<owner>[/<repo>]]
gh workspace clone [flags] <owner>/<repo>...
gh workspace untrack [flags] <owner>/<repo>
gh workspace worktree add [flags] <owner>/<repo> <branch> [path]
gh workspace worktree remove [--force] [flags] <owner>/<repo> <path> | <path>
gh workspace worktree list [flags] [<owner>/<repo>|<owner>]
```

Every command accepts `--root`, `--timeout`, `--protocol`, and `--config` — see
[Configure](#configure) for what each does. The rest of a command's flags are its own; they're
called out below, and `gh workspace <command> --help` always shows the authoritative list.

### `sync`

```sh
gh workspace                      # sync everything the config tracks
gh workspace --tracked-only       # same, but skip owner listings (cheap refresh)
gh workspace my-org                # sync one owner's full listing
gh workspace sync my-org/my-repo   # refresh one repo, tracked or not
```

This is the one you'll run most, and it's the only command that takes the full flag set:
`--root`, `--concurrency`, `--timeout`, `--max-repos`, `--protocol`, `--vcs`, `--include-forks`,
`--archive`, `--force`, `--tracked-only`, `--archive-missing`, `--dry-run`, `-v`/`--verbose`,
`--yes`, `--config`.

With no arguments it's workspace-wide: every configured owner gets its own `gh repo list`, and
every explicitly tracked repo outside those owners gets refreshed through one batched lookup that
spans all of them at once. Give it `<owner>` or `<owner>/<repo>` and it narrows to that one
listing or repo, configured or not. `--tracked-only` skips every owner listing entirely and only
refreshes what's already on disk or in state — the cheap option when you just want to check on
the handful of repos you actually cloned out of a huge org.

Bare `gh workspace <owner>` works as an undocumented shorthand for `sync <owner>`, as long as
`<owner>` isn't one of the words `sync`, `clone`, `untrack`, or `worktree` — an org with one of
those names needs the explicit `sync <name>` form to avoid the ambiguity.

### `clone`

```sh
gh workspace clone someone/useful-lib cli/cli
```

Tracks each argument and clones it if it's missing; if it's already there, it runs the same
skip/fetch/archive logic a sync would. A bad argument doesn't stop the rest — it keeps going and
exits non-zero if anything failed. Takes `--root`, `--timeout`, `--protocol`, `--vcs`, `--archive`,
`--force`, `--config`. No `--concurrency`/`--max-repos`/`--include-forks`/`--dry-run` — those only
mean something for a whole owner's listing.

### `untrack`

```sh
gh workspace untrack someone/useful-lib
```

Clears the explicit-track bit and nothing else — the clone stays exactly where it is. It refuses
if the config's `owners`/`repos` still track the repo, since clearing the bit would just get
overwritten by the next sync; it tells you which config entry to edit instead. Takes `--root`,
`--timeout`, `--protocol`, `--config` only.

### `worktree`

Every cloned repo is a normal working tree, so `git worktree` already works on it. `worktree` is
a thin wrapper that resolves `<owner>/<repo>` to that path for you — cloning first if it isn't
local yet — instead of making you remember `<root>/<owner>/repos/<repo>` by hand.

```sh
# Clones my-org/my-repo first if needed, then adds a worktree for `some-branch` at a path
# computed from worktreeRoot/worktreePath (default: the current directory, named after the repo).
gh workspace worktree add my-org/my-repo some-branch
gh workspace worktree add my-org/my-repo some-branch ~/code/my-repo-some-branch  # explicit path

gh workspace worktree list my-org/my-repo     # one repo's worktrees
gh workspace worktree list my-org             # every repo under one owner
gh workspace worktree list                    # every worktree in the workspace
gh workspace worktree remove my-org/my-repo ~/code/my-repo-some-branch
gh workspace worktree remove ~/code/my-repo-some-branch                         # path-only form
gh workspace worktree remove --force my-org/my-repo ~/code/my-repo-some-branch  # even if dirty
```

**`add`** fetches the central clone first, so a branch pushed since the last sync is there to
check out. An existing local or `origin/` branch is checked out as-is; one that exists nowhere yet
is created from `origin/<default branch>`, with no upstream set — your first `git push -u`
decides that. It takes the owner's sync lock for the duration, so it refuses while a sync is in
progress. It also refuses, after cloning (so the clone still lands), if the repo is archived
upstream — archived repos aren't expected to get new work; if it's already archived locally, it
skips the re-clone and just points you at the tarball. Flags: `--root`, `--timeout`, `--protocol`,
`--vcs`, `--worktree-root`, `--worktree-path`, `--config`.

**`remove`** takes `--root`, `--timeout`, `--protocol`, `--config`, plus its own `--force` — worth
flagging because it's *not* the same `--force` sync and clone have. Here it means "remove even
though the worktree has uncommitted changes"; there it means "ignore stored `pushedAt` and re-sync
anyway." Same flag name, different command, different meaning. The path-only form of `remove`
finds the owning repo by following the worktree's own `.git` file back to its central clone (or,
for a jj workspace, its `.jj/repo` file).

**`list`** takes `--root`, `--timeout`, `--protocol`, `--config` — no write flags at all, since it
only reads.

None of the three take `--concurrency`, `--max-repos`, `--include-forks`, or `--archive`; they
don't apply to a single repo, and their env vars and config keys are ignored here too.

#### Worktree placement

`[path]` is optional. Leave it off and `worktreeRoot` (default: wherever you ran the command) plus
`worktreePath` (default: `{repo}`, just the repo's name) decide where the worktree lands — so
`worktree add my-org/my-repo feat/x` run from `~/code` creates `~/code/my-repo`. `worktreePath` is
a template with `{owner}`, `{repo}`, `{branch}` placeholders (a `/` in the branch name becomes
`-` first, so `feat/x` can't sneak in extra path levels the template didn't ask for), and the
result has to stay under `worktreeRoot`. If a second worktree of the same repo computes the same
path, it fails with a nudge toward an explicit path or a template that includes `{branch}`. An
explicit `path` argument always wins over the template — resolved against your current directory,
exactly as before any of this existed, and not subject to the under-`worktreeRoot` check. You
always get to say where a worktree lives, whether that's by where you ran the command, a
configured template, or just typing the path.

## Configure

Precedence is **flags > environment > config file > defaults**.

| Flag | Env var | Config key | Default | Commands | Meaning |
| --- | --- | --- | --- | --- | --- |
| `--root` | `GH_WORKSPACE_ROOT` | `root` | see [Where repositories end up](#where-repositories-end-up) | all | root directory for all cloned orgs |
| `--concurrency` | `GH_WORKSPACE_CONCURRENCY` | `concurrency` | `8` | sync | repos synced in parallel |
| `--timeout` | `GH_WORKSPACE_TIMEOUT` | `timeout` | `30m` | all | per-subprocess timeout |
| `--max-repos` | `GH_WORKSPACE_MAX_REPOS` | `maxRepos` | `10000` | sync | `gh repo list --limit`; gh itself defaults to 30 |
| `--protocol` | `GH_WORKSPACE_PROTOCOL` | `protocol` | `ssh` | all | `ssh` or `https` clone URLs |
| `--vcs` | `GH_WORKSPACE_VCS` | `vcs` | `git` | sync, clone, worktree add | `git` or `jj` — see [Using jujutsu](#using-jujutsu) |
| `--include-forks` | `GH_WORKSPACE_INCLUDE_FORKS` | `includeForks` | `false` | sync | include forked repos |
| `--include-archived` | `GH_WORKSPACE_INCLUDE_ARCHIVED` | `includeArchived` | `false` | sync | include archived repos |
| `--archive` | `GH_WORKSPACE_ARCHIVE` | `archive` | `true` | sync, clone | tarball archived repos and remove their clones |
| `--worktree-root` | `GH_WORKSPACE_WORKTREE_ROOT` | `worktreeRoot` | the current directory | worktree add | root directory new worktrees are placed under |
| `--worktree-path` | `GH_WORKSPACE_WORKTREE_PATH` | `worktreePath` | `{repo}` | worktree add | worktree path template (`{owner}`, `{repo}`, `{branch}`) under `worktreeRoot` |
| `--force` | — | — | `false` | sync, clone | ignore stored `pushedAt`, re-sync every repo |
| `--force` | — | — | `false` | worktree remove | different meaning here: remove even if the worktree is dirty |
| `--tracked-only` | — | — | `false` | sync | skip owner listings; refresh only repos already present locally or in state |
| `--archive-missing` | — | — | `false` | sync | tarball the clones of repos that no longer resolve upstream (never deletes) |
| `--dry-run` | — | — | `false` | sync | print planned actions, do nothing |
| `-v`, `--verbose` | — | — | `false` | sync | verbose output (e.g. reports skipped forks) |
| `--yes` | — | — | `false` | sync | don't prompt before removing worktrees to archive a repo they belong to |
| `--config` | `GH_WORKSPACE_CONFIG` | — | see below | all | path to the JSON config file |

Run `gh workspace <command> --help` if you want a command's exact flag set printed back at you
instead of cross-referencing the table above.

Flags follow `gh`'s own convention: every long flag is `--name`, and `-v`/`--verbose` is the one
with a one-letter shorthand (again matching `gh`). Flags can go before or after positional
arguments, and `--` ends flag parsing. A boolean flag needs an explicit value to turn off a
default that's already `true` — e.g. `--archive=false`.

`--include-archived` and `--archive` answer different questions, and are easy to conflate:
`--include-archived` (default off) decides whether an upstream-archived repo is looked at *at
all* — off, it is skipped entirely, exactly like an excluded fork: no clone, no tarball, nothing
on disk. `--archive` only matters for a repo that *is* included and turns out to be archived
upstream: tarball it and remove the clone (the default), or just keep it as an ordinary live
clone (`--archive=false`).

The config file is JSON, e.g.:

```json
{
  "root": "~/.local/share/gh-workspace",
  "worktreeRoot": "~/code",
  "worktreePath": "{owner}/{repo}/{branch}",
  "protocol": "ssh",
  "owners": [
    { "name": "my-org" },
    { "name": "other-org", "includeForks": true, "archive": false },
    { "name": "jj-org", "vcs": "jj" }
  ],
  "repos": ["someone/useful-lib", { "name": "cli/cli", "vcs": "jj" }]
}
```

`owners` lists orgs/users to sync wholesale, each optionally overriding the global
`includeForks`/`includeArchived`/`archive`/`maxRepos`/`vcs`. `repos` lists individually tracked repos outside those
owners — the same list `clone`/`worktree add` add to implicitly. Each entry is either a plain
`"<owner>/<repo>"` string or an object with that `name` plus its own `vcs`. `root` and
`worktreeRoot` accept a leading `~/`, expanded to your home directory (`~user/` and `$HOME` are
not expanded; use an absolute path for those instead). The file's search path, first match wins:
the `--config` flag, then `$GH_WORKSPACE_CONFIG`, then `$XDG_CONFIG_HOME/gh-workspace/config.json`,
and finally `~/.config/gh-workspace/config.json`. An unknown key or a value that fails to parse is
a hard error, never silently ignored.

## Using jujutsu

Set `vcs` to `jj` — globally, for one owner, or for one repo — and that repo is managed with
[jujutsu](https://jj-vcs.dev) instead of git. `git` stays the default and nothing about a
git-backed repo changes.

```sh
gh workspace --vcs jj clone my-org/my-repo       # jj git clone --colocate
gh workspace worktree add my-org/my-repo feat/x  # jj workspace add
```

A few things are worth knowing going in. Clones are colocated — a jj clone holds both `.jj` and
`.git`, so `git` keeps working inside it, and so does anything else that only speaks git,
including coding agents. Which VCS a clone actually uses is detected, not configured: a clone with
a `.jj` directory is managed with jj no matter what the config says, so flipping `vcs` never
invalidates what's already on disk, and one workspace can hold both kinds side by side. The
setting only decides what a *new* clone becomes, and whether an existing one gets converted.

That conversion happens on sync, once you ask for it: set `vcs` to `jj` for a repo that's already
cloned with git, and the next sync runs `jj git init --colocate` on it and starts tracking the
default branch's remote bookmark. A clone with uncommitted changes is never converted — it's
reported and left alone, every run, until the tree is clean (`--dry-run` tells you which repos
would convert). Going back the other way is just `rm -rf .jj`.

`worktree add` on a jj repo creates no bookmark, since in jj you name a bookmark when you push,
not when you start — the branch argument only picks where the new workspace starts (that branch's
bookmark if it exists, otherwise the default branch), and the command prints the `jj bookmark
create` line for you to run once you're ready to push. A jj workspace itself isn't a git repo —
`jj workspace add` has no colocated form, so it has a `.jj` and no `.git`; use `jj` inside it, not
git-only tooling. (The central clone is still colocated, so that's where to point anything that
needs git.) And because `jj workspace forget` deliberately leaves the directory behind, `worktree
remove` deletes it itself — only ever a path jj actually reports as one of that repo's own
workspaces, and only after the forget succeeds. A workspace with uncommitted changes needs
`--force`.

`jj` needs to be on `PATH`, with a name and email configured, for any of this to work.

## Where repositories end up

```
<root>/<owner>/state.json
<root>/<owner>/repos/<name>/          # normal working trees — point coding agents here
<root>/<owner>/archives/<name>.tar.gz
<root>/<owner>/archives/<name>.json
```

`<root>` defaults to `$XDG_DATA_HOME/gh-workspace`, falling back to `~/.local/share/gh-workspace`
— not where most Mac users look for things, and Spotlight and Time Machine will both index and
back up everything under it. Pass `--root` explicitly if that matters to you. Worktrees live
wherever `worktreeRoot`/`worktreePath` (or an explicit path) puts them, entirely separate from
this layout.

## Things worth knowing before you run this on a big org

Disk footprint adds up fast: every live repo gets a full-history working-tree clone (not
`--bare`/`--mirror`, not shallow), and every archived repo gets a full tarball before its clone is
deleted. A 500-repo org can easily run to tens of gigabytes, and there's no size cap.

A dirty working tree is never touched. If a clone has uncommitted changes, this still fetches refs
but leaves the checkout alone, warns about it, and keeps warning on every future run rather than
ever fast-forwarding or archiving over your local edits.

A renamed repo keeps its worktrees. When a repo is renamed upstream, its clone's directory gets
moved (not re-cloned), and every worktree attached to it is re-pointed at the new location
afterward — `git worktree repair`, or the jj equivalent. Nothing is deleted and no history is
re-downloaded.

Nothing is ever deleted just because it went missing or untracked. A repo that disappears from an
org listing — renamed, deleted, or simply no longer visible to your token — or one that's dropped
from the config without ever being explicitly tracked, gets reported, never removed locally.
Absence looks identical to lost access: GitHub answers `NOT_FOUND` the same way for a deleted repo
and for one your token can no longer see, deliberately, so it never leaks whether a private repo
exists. So this tool records *how long* a repo has been unresolvable (`missingSince` in
`state.json`) and reports that, instead of guessing why. `--archive-missing` is the opt-in action:
it tarballs those clones through the same path an archived-upstream repo takes — manifest,
worktree prompt, all of it — so the history survives as an archive and only the working tree gets
reclaimed. It still never deletes, which is what makes it recoverable if the real cause turns out
to be an expired token: untar it, or just re-sync once access is back. A repo the listing *did*
return but that got filtered out (a fork under `includeForks: false`) is never treated as missing.

Archiving a repo with a live `git worktree` asks first. Deleting a repo's clone out from under a
linked worktree elsewhere would permanently break that worktree with no clean recovery, so a sync
run stops and asks before removing any worktree to proceed with archiving — and on an unattended
run (no terminal on stdin), it answers "no" and refuses rather than hang or guess. Pass `--yes` to
answer "yes" unattended, once you're sure.

A workspace-wide sync locks per owner, not globally. Every configured owner's tasks share one
worker pool, but each owner's state is saved — and its lock released — independently, as soon as
that owner's work finishes. If another run already holds one owner's lock, that owner gets
reported and skipped while every other owner still finishes; the run as a whole still exits
non-zero.

## Verifying a large org wasn't silently truncated

`gh repo list` is always called with an explicit `--limit` (`--max-repos`), and a run warns if the
listing comes back exactly that long, since that's the only sign of truncation gh gives you. To
double-check by hand, compare the dry-run plan (one line per repo, forks included only with
`--include-forks`) against the org's own count:

```sh
gh workspace --dry-run --include-forks <org> | wc -l
gh api /orgs/<org> --jq '.public_repos + .total_private_repos'
```

The two numbers can also differ for reasons that aren't truncation — repos your token can't see,
or repos skipped for an invalid or case-colliding name (reported on stderr, and they fail the
run). If they still disagree for no obvious reason, fall back to `gh api --paginate
'/orgs/<org>/repos?per_page=100&type=all'`.
