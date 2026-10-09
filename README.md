# gh-workspace

A [`gh`](https://cli.github.com/) extension that mirrors GitHub repositories — whole orgs/users, or individual
repos — into one local workspace, plus worktrees checked out from those central clones. Built so the repos are
available offline as reference material (for coding agents or otherwise). On each run it clones what's missing,
fetches what already exists, and tarballs anything archived upstream — doing as little work as possible on repeat
runs.

## Install

```sh
gh extension install swanysimon/gh-workspace
```

This downloads a prebuilt binary for your platform — no Go toolchain required. Requires `gh` itself to
be authenticated (`gh auth login`) and `git` on `PATH`.

To upgrade later:

```sh
gh extension upgrade swanysimon/gh-workspace
```

<details>
<summary>Building from source instead</summary>

```sh
go build -o gh-workspace .
gh extension install .
```

`gh extension install .` (run from this checkout) links the extension to the binary you just built,
which is useful while developing against a local change.
</details>

## What's tracked

A repo is tracked if either of these is true:

- Its owner is listed under `owners` in the config: every repo `gh repo list <owner>` returns is tracked,
  subject to that owner's own filters (forks, etc.).
- It was explicitly added: listed under `repos` in the config, or brought in by `clone <owner>/<repo>` or
  `worktree add <owner>/<repo> ...`. Explicit adds are recorded in state, so the config file doesn't have
  to be edited by hand.

`owners`/`repos` are declarative — a repo stays tracked for as long as the config lists it. Removing it
from the config stops tracking it unless it was ever explicitly added (`clone`'d or checked out as a
worktree), in which case `untrack` is what clears that. Nothing is ever deleted as a side effect of
tracking changing.

## Usage

### Sync

```sh
gh workspace                      # sync everything the config tracks
gh workspace --tracked-only       # same, but skip owner listings (cheap refresh)
gh workspace my-org                # sync one owner's full listing
gh workspace sync my-org/my-repo   # refresh one repo, tracked or not
```

Run it against any org or user you can see with `gh` — public repos need no special access, private ones
need a token with read access to them.

`sync` with no arguments is the workspace-wide form: every configured owner gets its own `gh repo list`,
and every explicitly tracked repo outside those owners gets refreshed through one batched lookup spanning
every owner at once. `sync <owner>` and `sync <owner>/<repo>` reach the same per-repo logic for just one
owner or one repo, whether or not it's configured. `--tracked-only` skips every owner listing and only
refreshes repos already present locally or in state — the cheap check for "I only care about the handful
of repos I actually cloned out of a huge org."

Bare `gh workspace <owner>` is an undocumented alias for `sync <owner>`, as long as `<owner>` isn't one of
the reserved words `sync`, `clone`, `untrack`, or `worktree` — an org or user with one of those names needs
the explicit `sync <name>` form.

### Track individual repos

```sh
gh workspace clone someone/useful-lib cli/cli   # track, and clone if missing
gh workspace untrack someone/useful-lib          # stop tracking; keeps the clone
```

`clone` tracks each argument and clones it if missing, or otherwise applies the same skip/fetch/archive
logic a sync would. It continues past a failed argument to try the rest, exiting non-zero if any of them
failed.

`untrack` only clears the explicit-track bit in state — it never deletes local data. It fails if the
config's `owners`/`repos` still track the repo; edit the config to actually stop tracking it in that case.

### Worktrees

Every cloned repo is a normal, non-bare working tree, so `git worktree` works on it as-is. The
`worktree` subcommand is a thin wrapper that resolves `<owner>/<repo>` to that path (cloning first if
needed) instead of you having to remember `<root>/<owner>/repos/<repo>` yourself; git does the rest.

```sh
# Clones my-org/my-repo first if it isn't already local, then adds a worktree for
# `some-branch`, placed per the worktreeRoot/worktreePath config (default: the
# current directory, named after the repo).
gh workspace worktree add my-org/my-repo some-branch
gh workspace worktree add my-org/my-repo some-branch ~/code/my-repo-some-branch  # explicit path

gh workspace worktree list my-org/my-repo     # list one repo's worktrees
gh workspace worktree list my-org             # list every repo's worktrees
gh workspace worktree list                    # list every worktree in the workspace
gh workspace worktree remove my-org/my-repo ~/code/my-repo-some-branch
gh workspace worktree remove ~/code/my-repo-some-branch                         # path-only form
gh workspace worktree remove --force my-org/my-repo ~/code/my-repo-some-branch  # even if it's dirty
```

`worktree add` fetches the central clone first, so a branch pushed since the last sync is available.
An existing local or `origin/` branch is checked out as-is; a branch that exists nowhere is created
from `origin/<default branch>` (with no upstream set, so the first `git push -u` decides it). It takes
the same per-owner lock as a sync run, so it refuses while one is in progress. The path-only form of
`worktree remove` finds the owning repo by following the worktree's own `.git` file back to its central
clone.

`worktree add` refuses (after cloning, so the clone still lands) if the repo is archived upstream —
archived repos aren't expected to get new work. If the repo is already archived locally, it refuses
without re-cloning and points at the tarball instead. `worktree add`/`remove`/`list` accept `--root`,
`--protocol`, `--timeout` and `--config`, same as `sync`; `--concurrency`, `--max-repos`,
`--include-forks` and `--archive` don't apply to a single repo and aren't accepted (their environment
variables and config keys are ignored).

#### Worktree placement

`[path]` is optional. Without it, `worktreeRoot` (default: the current directory the command runs in)
and `worktreePath` (default: `{repo}`, just the repo's name) decide where the worktree lands — so
`worktree add my-org/my-repo feat/x` run from `~/code` creates `~/code/my-repo`. `worktreePath` is a
template with `{owner}`, `{repo}`, `{branch}` placeholders (`/` in a branch name is replaced with `-`
first, so `feat/x` can't create extra path levels a template didn't ask for); the result must stay under
`worktreeRoot`. A second worktree of the same repo that computes the same path fails with a message
suggesting an explicit path or a template that includes `{branch}`. An explicit `path` argument always
wins over the template, resolved against your current directory exactly as before, and isn't subject to
the under-`worktreeRoot` check — the user always gets to say where a worktree lives, whether that's by
where they ran the command, a configured template, or an explicit path.

## Configure

Precedence is **flags > environment > config file > defaults**.

| Flag | Env var | Config key | Default | Meaning |
| --- | --- | --- | --- | --- |
| `--root` | `GH_WORKSPACE_ROOT` | `root` | see [Where repositories end up](#where-repositories-end-up) | root directory for all cloned orgs |
| `--concurrency` | `GH_WORKSPACE_CONCURRENCY` | `concurrency` | `8` | repos synced in parallel |
| `--timeout` | `GH_WORKSPACE_TIMEOUT` | `timeout` | `30m` | per-subprocess timeout |
| `--max-repos` | `GH_WORKSPACE_MAX_REPOS` | `maxRepos` | `10000` | `gh repo list --limit`; gh itself defaults to 30 |
| `--protocol` | `GH_WORKSPACE_PROTOCOL` | `protocol` | `ssh` | `ssh` or `https` clone URLs |
| `--include-forks` | `GH_WORKSPACE_INCLUDE_FORKS` | `includeForks` | `false` | include forked repos |
| `--archive` | `GH_WORKSPACE_ARCHIVE` | `archive` | `true` | tarball archived repos and remove their clones |
| `--worktree-root` | `GH_WORKSPACE_WORKTREE_ROOT` | `worktreeRoot` | the current directory | root directory new worktrees are placed under |
| `--worktree-path` | `GH_WORKSPACE_WORKTREE_PATH` | `worktreePath` | `{repo}` | worktree path template (`{owner}`, `{repo}`, `{branch}`) under `worktreeRoot` |
| `--force` | — | — | `false` | ignore stored `pushedAt`, re-sync every repo |
| `--tracked-only` | — | — | `false` | skip owner listings; refresh only repos already present locally or in state |
| `--dry-run` | — | — | `false` | print planned actions, do nothing |
| `-v`, `--verbose` | — | — | `false` | verbose output (e.g. reports skipped forks) |
| `--yes` | — | — | `false` | don't prompt before removing worktrees to archive a repo they belong to |
| `--config` | `GH_WORKSPACE_CONFIG` | — | see below | path to the JSON config file |

Not every command accepts every flag above — run `gh workspace <command> --help` for a command's actual
set (e.g. `worktree remove`/`list` have no use for `--worktree-root`/`--worktree-path`, and only `sync`
accepts `--tracked-only`).

Flags follow `gh`'s own convention: every long flag is `--name`; `-v`/`--verbose` is the one flag with a
one-letter shorthand, again matching `gh`. Flags can go before or after positional arguments; `--` ends
flag parsing. Boolean flags take an explicit value to turn off a default, e.g. `--archive=false`.

The config file is JSON, e.g.:

```json
{
  "root": "~/.local/share/gh-workspace",
  "worktreeRoot": "~/code",
  "worktreePath": "{owner}/{repo}/{branch}",
  "protocol": "ssh",
  "owners": [
    { "name": "my-org" },
    { "name": "other-org", "includeForks": true, "archive": false }
  ],
  "repos": ["someone/useful-lib", "cli/cli"]
}
```

`owners` is a list of orgs/users to sync wholesale, each optionally overriding the global
`includeForks`/`archive`/`maxRepos`. `repos` is a flat `"<owner>/<repo>"` list of individually tracked
repos outside those owners — the same list `clone`/`worktree add` add to implicitly. `root` and
`worktreeRoot` accept a leading `~/`, expanded to your home directory (`~user/` and `$HOME` are not
expanded; use an absolute path for those). Its search path (first match wins): `--config` flag,
`$GH_WORKSPACE_CONFIG`, `$XDG_CONFIG_HOME/gh-workspace/config.json`, else
`~/.config/gh-workspace/config.json`. An unknown key or a value that fails to parse is a hard error — it
is never silently ignored.

## Where repositories end up

```
<root>/<owner>/state.json
<root>/<owner>/repos/<name>/          # normal working trees — point coding agents here
<root>/<owner>/archives/<name>.tar.gz
<root>/<owner>/archives/<name>.json
```

`<root>` defaults to `$XDG_DATA_HOME/gh-workspace`, falling back to `~/.local/share/gh-workspace`. This
is **not** where most Mac users look for things, and Spotlight and Time Machine will index and back up
everything under it — pass `--root` explicitly if that matters to you. Worktrees live wherever
`worktreeRoot`/`worktreePath` (or an explicit path) puts them, entirely separate from this layout.

## Things worth knowing before you run this on a big org

- **Disk footprint.** Every live repo gets a full-history working-tree clone (not `--bare`/`--mirror`,
  not shallow), and every archived repo gets a full tarball before its clone is deleted. A 500-repo org
  can easily run to tens of gigabytes, and there is no size cap.
- **A dirty working tree is never touched.** If a clone has uncommitted changes, this tool still fetches
  refs but leaves the checkout alone, warns, and will keep warning on every future run instead of ever
  fast-forwarding or archiving over your local edits.
- **Nothing is ever deleted because it's missing upstream or untracked.** A repo that disappears from an
  org listing (renamed, deleted, or just no longer visible to your token), or one that's removed from the
  config without ever being explicitly tracked, is reported, never removed locally — absence looks
  identical to lost access.
- **Archiving a repo with a live `git worktree` asks first.** Deleting a repo's clone out from under a
  linked worktree elsewhere would permanently break that worktree with no clean recovery, so a sync run
  stops and asks before removing any worktree to proceed with archiving — and, on an unattended run (no
  terminal on stdin), answers "no" and refuses to archive rather than hang or guess. Pass `--yes` to
  answer "yes" unattended once you're sure.
- **A workspace-wide sync locks per owner, not globally.** Every configured owner's tasks share one worker
  pool, but each owner's state is saved (and its lock released) independently as soon as that owner's work
  finishes. If another run already holds one owner's lock, that owner is reported and skipped; every other
  owner still finishes, and the run exits non-zero.

## Verifying a large org wasn't silently truncated

`gh repo list` is always called with an explicit `--limit` (`--max-repos`), and a run warns if the listing
comes back exactly that long, since that is the only sign of truncation gh gives. To double check by hand,
compare the dry-run plan (which prints one line per repo, forks included only with `--include-forks`)
against the org's own count:

```sh
gh workspace --dry-run --include-forks <org> | wc -l
gh api /orgs/<org> --jq '.public_repos + .total_private_repos'
```

The two can also differ because of repos your token can't see, or repos skipped for an invalid or
case-colliding name (reported on stderr, and they fail the run). If they disagree for another reason, the
fallback is `gh api --paginate '/orgs/<org>/repos?per_page=100&type=all'`.
