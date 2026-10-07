# AIDEV: from org-clone to a workspace CLI

Goal: one CLI, one configuration, that manages central clones **and**
worktrees for any mix of whole orgs/users and individual repos. Two threads:

- **Modularity**: internal packages with real seams, and one declarative
  settings table instead of per-command copies of the precedence logic.
- **Workspaces**: a workspace is **tracked clones plus configured worktree
  placement**. It knows which owners and repos it tracks, where their
  central clones go, and where their worktrees go. Cloning a repo tracks it
  but does not create a worktree. Worktrees always come from an explicit
  `worktree add`, placed by the configured template or an explicit path. A
  combined "clone and give me a checkout" command is out of scope.

This is a working plan. Update it when reality diverges, and record
decisions in place instead of making them silently in code.

Process (from AGENTS.md): **every phase starts with its own jj change**
(`jj new -m "<phase description>"`) *before* any edits, so each concept is
isolated. Checklist items below that say "jj" mean exactly that.

---

## The workspace model (target design)

### What a workspace tracks

A repo is **tracked** if either of these is true:

1. Its owner is listed under `owners` in the config. Every repo
   `gh repo list <owner>` returns is tracked, subject to that owner's
   filters (forks, etc.).
2. It was explicitly added: listed under `repos` in the config, or brought
   in by `clone <owner>/<repo>` or `worktree add <owner>/<repo> ...`.
   Explicit adds are recorded in state, so the config file doesn't have
   to be edited by hand.

#### Source of truth for tracking

- The config's `owners` and `repos` are **declarative**: a repo is tracked
  for as long as the config lists it. The explicit flag in state records
  **imperative** adds (`clone`, `worktree add`).
- A repo is tracked if the config lists it (directly or via its owner)
  **or** state marks it explicit.
- Removing a repo from the config's `repos` stops tracking it, unless state
  also marks it explicit (which it does if it was ever `clone`d). `sync`
  notes any repo that is on disk but no longer tracked. Nothing is deleted.
- `untrack` clears only the state flag. If the config tracks the repo
  (listed in `repos`, or under a listed owner), `untrack` fails with an
  error naming the config entry to edit, rather than silently doing nothing.

#### Discovering tracked repos

There is no workspace-level index. State stays per owner
(`<root>/<owner>/state.json`), so the per-owner lock remains the only
coordination. Commands that need the whole workspace (`sync`,
`sync --tracked-only`, `worktree list`) read the config and enumerate
`<root>/*/state.json`. That's one readdir of a directory with one entry per
owner. Consider a root-level index only if the scan proves slow.

Example config (JSON, strict unknown-key rejection as today):

```json
{
  "root": "~/src/.workspace",
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

Per-owner entries can override global defaults (`includeForks`, `archive`,
`maxRepos`). Global keys stay what they are today.

`root` and `worktreeRoot` accept a leading `~/`, expanded to the home
directory. This is new work (Phase 3): today `validateConfig` rejects
anything that isn't an absolute path, and nothing expands `~`. After
expansion, relative paths are still rejected, whether they come from flags,
env vars or the file. `$VAR` expansion is not supported.

### Commands

| Command | Behavior |
| --- | --- |
| `sync` | Sync every tracked repo: full listing for each configured owner, plus explicitly tracked repos outside those owners. |
| `sync <owner>` | Full listing sync of one owner (today's `gh org-clone <org>`), whether or not it's configured. |
| `sync <owner>/<repo>` | Sync one repo (clone, fetch, or archive), the same per-repo logic as a full sync. |
| `sync --tracked-only` | Skip the owner listings; refresh only repos already present locally or in state. This is the cheap refresh for "I cloned three repos from a huge org." |
| `clone <owner>/<repo>...` | Track, and clone if missing. If already present, same per-repo logic as `sync <owner>/<repo>` (skip when `pushedAt` is unchanged, otherwise fetch + fast-forward). Records the explicit track in state. |
| `untrack <owner>/<repo>` | Clear an explicit track in state; never deletes local data. Fails if the config tracks the repo (see source of truth above). |
| `worktree add <owner>/<repo> <branch> [path]` | Clone if needed (and track), then add the worktree. `path` is optional; without it, the placement rules below apply (default: under the current directory). |
| `worktree remove <owner>/<repo> <path>` / `worktree remove <path>` | The path-only form finds the owning repo via the worktree's `.git` file. |
| `worktree list [<owner>[/<repo>]]` | No arg lists every worktree in the workspace. |
| `status` (optional, later) | Tracked repos, their state (cloned/archived/dirty), and their worktrees. |

Bare `gh workspace <owner>` stays as an undocumented alias for
`sync <owner>` (muscle memory), but only when `<owner>` isn't a reserved
command word: `sync`, `clone`, `untrack`, `worktree`, `status`, `help`,
`completion`. An org with one of those names must use `sync <owner>`, which
is the documented form anyway. The same ambiguity already exists today for
an org named `worktree`.

### Worktree placement

- `worktreeRoot` + `worktreePath` template, with placeholders `{owner}`,
  `{repo}`, and `{branch}`. `{branch}` has `/` replaced (e.g. `feat/x` →
  `feat-x`) so a branch name can't create extra path levels. The substituted
  result must stay under `worktreeRoot`: reject `..` after cleaning.
- **Defaults (decided in Phase 0):** `worktreeRoot` defaults to the
  **current directory** when the command runs, and `worktreePath` defaults
  to `{repo}`: the repository name, without the owner. So
  `worktree add my-org/my-repo feat/x` run from `~/code` creates
  `~/code/my-repo`. A user-specified `worktreePath` (config, env or flag) or
  an explicit `path` argument replaces the default. A second worktree of the
  same repo in the same directory hits an existing path and fails with a
  message suggesting an explicit `path` or a template that includes
  `{branch}`. A `worktreeRoot` set in config, env or a flag must be absolute
  (after `~/` expansion); only the unset default resolves to the current
  directory.
- An explicit `path` argument always wins. It's resolved against the
  current directory as today and isn't subject to the under-`worktreeRoot`
  check. This keeps the AGENTS.md rule "the user always specifies where a
  worktree lives": where you run the command, a configured template, or an
  explicit path are all the user choosing. If the target path already
  exists, fail (git does this already); never pick an alternative name.
- Central clones stay at `<root>/<owner>/repos/<repo>`. The layout is
  unchanged, so existing data stays valid.

### Multi-owner sync

- **One worker pool for the whole run.** `sync` with no args plans every
  owner first, then feeds all owners' tasks into a single pool of
  `concurrency` workers. A workspace with many small owners isn't processed
  one owner at a time. The single collector loop still does all state
  mutation. It saves an owner's `state.json` as soon as that owner's last
  task finishes, so an interrupt loses progress only for owners still in
  flight.
- **Locks are per owner and fail fast** (unchanged). An owner's lock is
  taken before that owner is planned (listing or batch lookup, then
  `decide`) and held until its state is saved. Lock order doesn't matter
  because nothing ever waits on a lock.
- **Partial failure.** If another run holds an owner's lock, `sync` reports
  it, skips that owner entirely, finishes every other owner, and exits `1`.
  The same applies when an owner's listing fails. Results for the other
  owners are still saved.
- `sync <owner>` and `sync <owner>/<repo>` use the same machinery with a
  single owner.

### Invariants that must survive

- **A no-op `sync` stays cheap.** Today it costs one API call per owner.
  Explicitly tracked repos outside configured owners must **not** cost one
  `gh repo view` each. Batch them into `gh api graphql` queries using
  aliased `repository(owner:, name:)` fields (about 100 per query), with the
  same fields as `ghJSONFields`. Then `pushedAt` comparison skips unchanged
  repos exactly as today. Batch edge cases (verify each against the real
  API before relying on it):
  - A repo that's gone or no longer visible comes back as `null`, with an
    entry in `errors`. `gh api graphql` may exit non-zero while still
    printing partial `data`. Parse the partial data and treat a `null` repo
    as "missing upstream": report it, never delete (the same rule as absence
    from a listing). Only a response with no usable `data` fails the owner.
  - Renames: check whether `repository(owner:, name:)` follows redirects.
    If it does, the returned `id` matches state but the names differ, and
    today's ID-based rename fix-up handles it. If it doesn't, a renamed
    explicit repo looks missing, so report it with a hint to `clone` the
    new name.
  - Transfers: if the returned owner differs from the owner directory the
    repo lives in, report it. Don't move data between owner directories
    automatically.
- Per-repo skipping, not early exit (archiving doesn't move `pushedAt`;
  renames are matched by ID).
- Never delete because something is missing or untracked. Dirty trees are
  never touched. Archiving with live worktrees prompts first (`--yes`
  unattended).
- Per-owner lock serializes every mutation of an owner's clones or state.

### State changes

- Add `tracked: "owner" | "explicit"` (or `explicit: bool`) to `repoState`.
- An explicitly tracked repo is synced even if it's a fork and the owner's
  `includeForks` is false. An explicit request beats a filter.
- **Migrate, don't discard.** `loadState` currently throws away any file
  with the wrong version. That's fine for a pure cache, but not once state
  holds explicit tracking the user can't regenerate. Bump to version 2,
  migrate v1 → v2 in `loadState` (every v1 entry becomes
  `tracked: "owner"`), and keep discarding only for *unknown* versions,
  loudly. Also consider backing up a corrupt file (`state.json.corrupt-<ts>`)
  instead of silently starting over.

---

## Phase 0 — Decisions (resolved)

- [x] **Name: `gh-workspace`** (`github.com/swanysimon/gh-workspace`). The
      extension is invoked as `gh workspace`; the env prefix is
      `GH_WORKSPACE_*`; the data directory is
      `$XDG_DATA_HOME/gh-workspace` (falling back to
      `~/.local/share/gh-workspace`); the config file is
      `$XDG_CONFIG_HOME/gh-workspace/config.json` (falling back to
      `~/.config/gh-workspace/config.json`).
- [x] **Compatibility: clean break.** The only user is the maintainer. No
      fallback to `GH_ORG_CLONE_*`, the old config path or the old data
      directory, and no MIGRATING.md. My own machine gets migrated by hand
      (Phase 4 lists the steps). The one exception is the v1 → v2 state
      migration, which stays: it's a few lines, the on-disk layout is
      unchanged so a moved old root just works, and it sets up the
      "migrate, don't discard" rule for later state versions.
- [x] **Flag library: cobra** (with `pflag`), the first dependency in
      `go.mod`. The migration is its own Phase 1 step, a deliberate behavior
      change with pinned tests, not a pure refactor.
- [x] **API calls: keep shelling out to `gh`** (no `go-gh`), behind the
      `Exec` seam. The GraphQL batch is one `gh api graphql` call, and
      tests keep using fake exec.
- [x] **Default worktree location: the current directory.**
      `worktreeRoot` defaults to the directory the command runs in, and
      `worktreePath` defaults to `{repo}` (the repository name without the
      owner) unless the user specifies otherwise (see "Worktree
      placement"). Config, env or a flag can override both.
- [x] **Forks: excluded unless explicitly overridden.** A fork is synced
      only if its owner entry sets `includeForks: true` (or
      `--include-forks` / `GH_WORKSPACE_INCLUDE_FORKS` is set), or if the
      fork itself is explicitly tracked (config `repos`, `clone`, or
      `worktree add`).
- [x] **Library reuse: `internal/` for now.** No second consumer exists.
      Moving a package out of `internal/` later is mechanical, while
      un-publishing an API is not.
- [ ] Start Phase 1 with `jj new`.

## Phase 1 — Seams and settings, still in `package main`

Refactor *before* splitting packages, while every test still compiles
against unexported names. Behavior stays identical, except for the cobra
step, whose user-visible differences are deliberate and recorded.

- [x] jj: `jj new -m "refactor: injectable runner and prompt seams"`.
- [x] Replace the global `runner` variable with an explicit dependency.
      Define an `Exec` interface (`Run(ctx, dir, name, args...)`) carried in
      a small `deps` struct (exec, confirm prompt, clock, stderr) that every
      entry point takes. `gh_test.go`, `main_test.go` and `archive_test.go`
      currently swap `runner` globally, and `archive_test.go` swaps
      `confirmArchiveWithWorktrees`. Convert them to pass fakes. This is the
      prerequisite for splitting packages: a package-level variable can't be
      shared across `ghcli` and `gitcli`.
  - **Done, with two deliberate deviations from the wording above** (see
    `deps.go`): `Exec`/`ConfirmFunc` are function *types*, not a
    `Run(...)`-method interface — equivalent for this purpose, more
    idiomatic. `deps` has no `clock` (nothing in the codebase calls a clock
    seam-worthy source; adding an unused one would be dead code) and no
    `stderr` (it was never a global — every function already takes it as
    an explicit `io.Writer` parameter, so it wasn't a problem this step
    needed to solve). `cfg.Deps.exec`/`cfg.Deps.confirm` cover every
    production call site; two package-level vars (`execDefault`,
    `confirmDefault`) remain solely to seed `defaultConfig()`, for
    black-box tests that build their own `cfg` internally and so have
    nothing to inject into beforehand. Reviewed and passed by an
    independent agent; `go build`/`vet`/`gofmt`/`test -race` all clean.
- [x] Pin the behavior that must *not* change. Written against the
      pre-cobra code, in their own commit (`test: pin CLI exit codes, help
      text, and flag parsing before the cobra port`), reviewed by an
      independent agent through two rounds (first found a real env-leak bug
      and a too-weak assertion, both fixed and re-verified by deliberately
      reintroducing each regression in a scratch copy and confirming the
      test catches it):
  - usage errors exit `2`, runtime failures exit `1`, an interrupt exits
    `130`. Cobra returns errors and `main` maps them to exit codes; turn
    off cobra's own usage and error printing.
  - `--` ends flag parsing, and a positional arg starting with `-` works
    after it
  - bool flags accept `--archive=false`
  - `-v`/`--verbose` is the only shorthand flag
  - `-h`/`--help` exits `0`
  - exact `--help` content (top-level and each `worktree` subcommand), not
    just "contains the word usage" — the previous `TestHelpExitsZero` and
    `TestWorktreeHelpUsesGhFlagStyle` were confirmed (by deliberately
    breaking the help text) not to catch a reordered or reworded flag line
  - all of the above go through the real entry points (`run`,
    `runWorktree`, `cmdWorktreeRemove`), not `parseInterspersed`, which is
    being deleted
- [x] jj: `jj new -m "refactor(cli): move command parsing to cobra"`.
- [x] Ported using **`pflag` directly, not a `cobra.Command` tree.**
      Scope deviation, made deliberately and recorded here rather than
      silently: a `cobra.Command` per entry point would have meant
      reproducing cobra's own help/usage/error-printing pipeline (via
      `SilenceErrors`/`SilenceUsage`/custom `HelpFunc`) just to keep every
      pinned test's exact-text assertions passing, for a two-level dispatch
      (`run` → `runWorktree` → `cmdWorktreeAdd`/`Remove`/`List`) that stays
      hand-rolled either way — cobra's own tree-walking `Find`/`Execute`
      dispatch isn't used, and couldn't be, without also collapsing
      `runWorktree`/`cmdWorktreeAdd`/etc. into one `Execute()` call, which
      would break their existing standalone-callable signatures that
      `worktree_test.go` relies on throughout. `pflag` alone delivers every
      behavior the plan actually wanted (interspersed flags, `--`,
      `--archive=false`, real shorthands via `BoolVarP`) with far less risk.
      **A real `cobra.Command` tree is deferred to Phase 3**, where `sync`,
      `clone`, `untrack` and `status` genuinely need tree-based dispatch and
      cobra's own help formatting stops being a liability instead of a
      pinned-text risk. `go.mod` currently depends on `pflag` only
      (`go mod tidy` dropped the unused `cobra`/`mousetrap` transitive
      deps `go get cobra` had pulled in); cobra goes back in when Phase 3
      actually builds the tree.
- [x] Deleted `parseInterspersed` (pflag accepts interspersed flags and `--`
      natively — verified empirically against a throwaway program before
      relying on it, not assumed). Deleted the now-meaningless
      `TestParseInterspersed` unit test along with it; the same ground is
      covered end-to-end by `TestPinDashDashEndsFlagParsing` and
      `TestConfigFlagsAfterOrg`, which exercise `run()` itself rather than
      an internal helper.
- [x] Ran every pinned test from the previous commit against the ported
      CLI with **zero test-assertion changes required** (only the doomed
      `newFlagSet()`/`resolveConfig(fs, ...)` call-site plumbing changed,
      not what any test actually checks) — all passed first try, including
      the exact-text `--help` pins. Two real deliberate differences turned
      up during manual exploration (not from a pinned-test failure) and
      got their own new pinned tests rather than just a note:
  - **Single-dash long flags no longer work** (`-root`, `-config`, etc.);
    only `-v`/`-h` remain valid single-dash shorthands. The old behavior
    was an accident of stdlib `flag`'s leniency, contrary to this tool's
    own documented `--long` convention. Every test call site using
    single-dash long flags was updated to `--long`.
    `TestPinSingleDashLongFlagNoLongerAccepted` pins the new behavior.
  - **A raw flag-parse error (unknown flag, bad value) is now printed
    exactly once**, not twice. stdlib `flag`'s own internal `failf` printed
    the error to `stderr` itself *and* `run()` printed it again after
    `resolveConfig` returned; pflag's output is silenced
    (`SetOutput(io.Discard)`) so only `run()`'s print survives.
    `TestPinUsageErrorPrintedOnce` pins this. The full usage block is still
    shown exactly once on the same errors (unchanged), and a semantic
    `validateConfig` failure (e.g. a bad `--protocol` value) still shows
    *no* usage block (unchanged — this was deliberately *not* extended to
    match, to keep the change scoped to the parse-error path the plan
    actually called out).
  - Cobra's own `help`/`completion` subcommands do **not** appear (since no
    `cobra.Command` tree was built), so that anticipated difference from
    the original plan text did not materialize. Struck through above; it's
    a Phase 3 question once a real tree exists.
- [x] `go build`/`vet`/`gofmt`/`test -race -count=1` all clean. Reviewed by
      an independent agent, which additionally: ran the pinned tests
      itself, empirically verified interspersed flags/`--`/`--archive=false`
      by executing the built binary directly (not just reading the code),
      and confirmed both newly-recorded deliberate differences are backed
      by real regression tests by re-introducing each one in a scratch copy
      and watching the corresponding pinned test fail. Verdict: PASS.
- [x] jj: `jj new -m "refactor: single declarative settings table"`.
- [x] Replaced `resolveConfig` and `resolveWorktreeConfig`'s duplicated
      precedence logic with one table (new `settings.go`). Each entry
      (`setting`) has: flag name/shorthand/kind, env var, config key
      (documentation-only, cross-checked against `fileConfig`'s real json
      tags by a test rather than left to drift — see below), a `fileValue`
      parser, an `parseEnv` parser, and `commands` (the single source of
      truth for which commands accept it — `cmdSync` or `cmdWorktree`).
      `bindSettings(fs, cmd)` registers exactly that command's subset as
      `pflag` flags; the table itself has no pflag import dependency beyond
      that one binder, so it doesn't depend on the flag library (its
      `apply`/`fileValue`/`parseEnv` closures work with plain Go values).
      `resolveSettings(cfg, cmd, fs, bound, fc)` is the one function
      implementing flags > env > file > defaults for every command. A
      command still silently ignores an env var or config key it doesn't
      declare, exactly as before — now because `settingsFor(cmd)` simply
      never yields that setting, not because of two independently
      hand-maintained lists.
  - `--config`/`--help` stay special-cased outside the table (as before):
    `--config` names which file to load, so it can't be a value *within*
    that file, and `--help` was never in the flags table either.
  - The four sync-only pure-flag settings (`--force`, `--dry-run`,
    `--verbose`, `--yes`) are table entries too, with `envVar`/`configKey`
    left `""`. `resolveSettings` applies them through the exact same
    "only if `fs.Changed`" step as every layered setting — no special
    case needed, since each one's unchanged value already equals
    `defaultConfig()`'s.
- [x] Tests: every existing precedence test
      (`TestConfigPrecedence`/`TestConfigDefaults`/etc.) passed with zero
      assertion changes — only call-site plumbing differed. Added, in
      `settings_test.go`:
  - `TestSettingsFor`: the table-driven test the plan asked for, asserting
    each command accepts exactly its declared settings (no more, no
    fewer).
  - `TestBindSettingsRegistersExactlyDeclaredFlags`: the same claim, but
    through `bindSettings` and a real `pflag.FlagSet`, so a bug in its
    kind-`switch` would show up even if `settingsFor`'s data were right.
  - `TestSettingsTableConfigKeysMatchFileConfig`: a reflection-based check
    that every setting's `configKey` names a real `fileConfig` json tag
    and vice versa, so `configKey` (which nothing else reads — it's
    documentation-only per its doc comment) can't silently drift from the
    struct `loadFileConfig` actually decodes, and a new file key can't be
    added without a matching table entry.
- [x] `go build ./...`, `go vet ./...`, `gofmt -l .`, and
      `go test -race -count=1 ./...` all clean. Manually smoke-tested
      `--help`/`worktree add --help` output (unchanged) and a
      `--config`+env-var combination end to end. Reviewed by an
      independent agent, which additionally verified undeclared env/config
      ignoring empirically (set `GH_ORG_CLONE_ARCHIVE` and confirmed a
      worktree command ignores it) and confirmed both new table-driven
      tests catch real injected regressions in a scratch copy. Verdict:
      PASS.

## Phase 2 — Extract packages (behavior identical)

- [ ] jj: `jj new -m "refactor: extract internal packages"`. **Deviation,
      recorded as it happens rather than only at the end**: given how large
      this phase is, it's being done as a sequence of per-package jj
      changes (dependency order: `execx` → `ghcli`/`gitcli` →
      `store`/`plan` → `archive` → `settings` → `engine` → `cli` → shrink
      `main.go`), each reviewed before the next starts, rather than one
      single commit for the whole phase — matching how Phase 1's steps
      were handled, and keeping each reviewable on its own.
- [ ] **Keep `package main` at the repo root.** `gh-extension-precompile`
      builds the root package by default. Moving `main` to `cmd/` would need
      a `build_script_override`. Verify against the action's docs before
      changing anything here. The root `main.go` becomes a few lines calling
      `internal/cli`.
- [ ] Layout, under `internal/` per the Phase 0 library-reuse decision
      (dependencies point downward only):
  - [x] `internal/execx`: `Exec` type (a plain func type, not an
        interface — same deliberate choice as Phase 1's `deps.go`, which
        this subsumes), `Run` (the real implementation, moved verbatim from
        `git.go`'s `execCommand`), and a `WithTimeout` helper that replaces
        the two-line `context.WithTimeout`/`defer cancel()` boilerplate
        every gitcli call site used to repeat for itself. Root `git.go`'s
        `execCommand` now just forwards to `execx.Run`—kept as a thin
        shim (rather than updated at every call site immediately) so this
        step stays small and reviewable on its own; every call site moves
        onto `execx` directly when `gitcli`/`ghcli` themselves move.
        `deps.go`'s `Exec` is now `type Exec = execx.Exec` (a type alias,
        not a new type), so every existing fake-exec function in every test
        file keeps compiling with zero changes. New `execx_test.go`
        (`internal/execx` had no tests of its own before, since
        `execCommand`'s own behavior — error wrapping, env vars, timeout
        enforcement — was previously only exercised incidentally by other
        tests using it to set up fixtures, never asserted on directly).
        Reviewed by an independent agent (PASS); its one finding (the
        stderr-trimming assertion didn't actually distinguish trimmed from
        untrimmed, since the test's own command-args text happened to
        contain the same word) was fixed and re-verified by reintroducing
        the regression in a scratch copy and confirming the test now
        catches it. `go build`/`vet`/`gofmt`/`test -race` clean on both the
        root
        package and `internal/execx`, zero existing test changes needed.
  - [x] `internal/ghcli`: `Repo`/`RefName` types, `ListRepos(ctx, exec,
        owner, limit)`, `ViewRepo(ctx, exec, nameWithOwner)`, `CloneURL(repo,
        protocol)` — moved verbatim from `gh.go`, taking an `execx.Exec` and
        plain arguments instead of a config struct. `ViewRepos` (GraphQL
        batch) is deferred to Phase 3, when explicit multi-repo tracking
        actually needs it. Root `gh.go` now holds only type aliases
        (`ghRepo = ghcli.Repo`, `ghRefName = ghcli.RefName`) and three
        one-line forwarding shims, so every existing `ghRepo{...}` literal
        across the codebase (there are dozens, in `main.go`/`worktree.go`/
        `archive.go`/every test file) keeps compiling unchanged.
        `gh_test.go` was deleted outright rather than kept as a redundant
        shim test: its three tests (`TestParseRepoList`, `TestListReposArgs`,
        `TestListReposError`) moved to `internal/ghcli/ghcli_test.go`
        verbatim (same assertions), where the real logic now lives, plus
        three new ones (`TestListReposParseError`, `TestViewRepoArgs`,
        `TestViewRepoError`, `TestCloneURL`) that `gh_test.go` never had.
        **Correction after review:** the claim that existing
        `main_test.go`/`worktree_test.go` tests cover `cfg.Org`/
        `cfg.MaxRepos` actually reaching the `gh` invocation was false —
        the independent reviewer demonstrated this by hardcoding both to
        wrong values in a scratch copy and watching the full suite still
        pass (those tests' exec fakes return a fixed payload regardless of
        args, so they only prove `cfg.Deps.exec` is reached, not that it's
        reached with the right arguments). Fixed with a new
        `gh_shim_test.go` at the root, specifically testing `gh.go`'s shims
        (`listRepos`/`getRepo`/`cloneURL`) rather than `internal/ghcli`'s
        own logic, asserting the real argv. Re-verified by reintroducing
        the exact regression (hardcoding org/limit) in a scratch copy and
        confirming the new test now fails.
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package,
        `internal/execx`, and `internal/ghcli`, with zero existing-test
        changes needed. Re-reviewed after the fix: independently confirmed
        all three shim tests catch their regressions, and the corrected
        wording makes no new false claims. Verdict: PASS.
  - [x] `internal/gitcli`: `Run`/`Clone`/`Fetch`/`IsDirty`/
        `UpdateWorktree`/`HeadInfo`/`Tags`/`LinkedWorktrees`/`SetRemoteURL`,
        and a `Tag` type (was `archiveTag`) — moved verbatim from `git.go`,
        taking an `execx.Exec` and a `time.Duration` timeout instead of
        `cfg`. `Clone` takes `dest`/`tmp` explicitly rather than computing
        them: URL and path resolution stay the caller's job (`gh.go`'s
        `cloneRepo` shim still does that part, using `reposDir(cfg)` and
        `repo.Name`). Root `git.go` now holds `type archiveTag =
        gitcli.Tag` plus nine one-line forwarding shims.
        `git_test.go` kept only the fixtures other test files still share
        (`initTestRepo`/`testConfig`/`mustMkReposDir`) and lost its six
        behavioral tests, which moved to `internal/gitcli/gitcli_test.go`
        against the real git binary (via `execx.Run`, not `cfg`/`ghRepo`),
        plus two new ones (`TestClone`,
        `TestCloneFailureCleansUpTmpAndDoesNotRename`) that
        `cloneRepo`'s indirect exercise of git plumbing never covered on
        its own.
        **Applying the `ghcli` step's lesson before review, not after:**
        added `git_shim_test.go` up front, since `cloneRepo` is the one
        gitcli shim with real logic of its own (URL selection via
        `cfg.Protocol`, dest/tmp path computation via `reposDir(cfg)` and
        `repo.Name`) rather than pure forwarding — the same shape of risk
        `gh.go`'s `listRepos` had. `TestCloneRepoShimComputesDestAndURL`/
        `TestCloneRepoShimSelectsHTTPSURL` use a fake `exec` that
        `os.MkdirAll`s the captured `tmp` path before returning success (so
        the shim's real, unfaked `os.Rename` into `dest` still succeeds),
        then assert the captured argv and the final `dest`.
        `TestGitShimsForwardDirAndTimeout` checks the other eight shims
        each forward their `dir` argument to `cfg.Deps.exec` unchanged —
        the dimension most likely to suffer a copy-paste mistake, since
        every one of them takes `dir` as a plain parameter rather than
        computing it. Verified all three new tests actually catch their
        regressions by reintroducing each one (wrong URL field, wrong
        hardcoded dir) in scratch copies before trusting them.
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package and
        all three `internal/` packages so far. Reviewed by an independent
        agent (PASS): confirmed behavioral identity on all nine functions,
        confirmed the shared test fixtures were kept (not accidentally
        deleted), and independently reproduced both new regressions the
        shim tests are meant to catch. One cosmetic, non-blocking finding:
        `Clone`'s two error messages now interpolate `url` instead of the
        old `repo.Name` (`gitcli` has no notion of a repo name, only a
        clone URL) — a deliberate, reasonable consequence of the
        extraction, and nothing depends on the old wording, but it means
        "behavior identical" isn't 100% literal for that one error string.
  - [x] `internal/store`: `OwnerDir`/`ReposDir`/`ArchivesDir`/`StatePath`/
        `LockPath` (taking plain `root, owner string` instead of `cfg` —
        the `Owner` naming from this bullet's own wording is used in the
        new package now, even though the dedicated org→owner identifier
        rename elsewhere is still a later checklist item: no cost to
        naming new code correctly from the start), `State`/`RepoState`/
        `Status`, `LoadState`/`SaveState`/`AcquireLock`/`ValidRepoName` —
        moved verbatim from `state.go`. `State.Org`'s field name and json
        tag are deliberately left alone in this step (out of scope; the
        rename sweep is its own later step, and this package's job here is
        extraction, not renaming on-disk data). Root `state.go` is now
        aliases plus ten thin shims.
        `state_test.go` was deleted; its three tests moved to
        `internal/store/store_test.go` verbatim, plus three new ones
        (`TestOwnerPaths`, `TestAcquireLock`, and a second lock case) for
        two things that had **no prior direct test at all**: the path
        functions, and `AcquireLock` itself (previously only exercised
        indirectly, by tests that pre-create a lock file and check that
        `run()`/`cmdWorktreeAdd` refuse to proceed).
        **Added the shim-wiring test up front again** (`state_shim_test.go`,
        `TestPathShimsForwardRootAndOrg`): `orgDir`/`reposDir`/etc. are the
        one place in `state.go` with real logic of their own (joining
        `cfg.Root` and `cfg.Org`); `acquireLock`/`loadState`/`saveState`/
        `validRepoName` are pure forwards, same reasoning as `gitcli`'s
        step. Verified both this test and `internal/store`'s new
        `TestAcquireLock` actually catch their regressions (a hardcoded
        wrong org, and a lock that silently drops its exclusivity flag) in
        scratch copies before trusting them.
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package and
        all five `internal/` packages so far, zero existing test changes.
        Reviewed by an independent agent (PASS), which independently
        reproduced both new regression catches and confirmed `State.Org`'s
        json tag was left untouched.
  - [x] `internal/archive`: `Manifest`/`Repo`/`WorktreeStatus`/
        `ConfirmFunc`, `DefaultConfirm`/`isInteractive` (moved from
        `confirm.go`, not just `archive.go` — the plan's own bullet already
        said "the worktree confirmation prompt" belongs here),
        `ManifestPath`/`TarballPath`/`LocalArchiveExists`/`ReadManifest`/
        `WriteManifest`/`WriteTarball`, `ConfirmAndRemoveWorktrees`, and
        `Archive` (was `archiveRepo`) — moved verbatim, taking plain
        arguments and an injected `clone func(ctx) error` callback instead
        of `cfg`/`ghRepo`, so this package doesn't need to know how to
        construct a clone URL or temp-dir path itself (that stays `gh.go`/
        `git.go`'s job). This is the first package with real fan-in:
        depends on `execx`, `gitcli`, and `store` (for `RepoState`/`Status`,
        since archiving is one of the outcomes a sync run records) —
        matches the natural shape of what the function actually
        orchestrates. `Repo.Owner`/`Manifest.Org`'s field name and json tag
        are left alone, same scope boundary as `store`'s step.
        Root `archive.go` and `confirm.go` are now aliases plus eight thin
        shims; `archiveRepo`'s field-mapping into `archive.Repo` (`cfg.Org`
        → `Owner`, `repo.*` → the rest) and its `dir`/`clone`/`confirm`
        closures are the one piece of real logic left at the root.
        `archive_test.go` was deleted; all eight of its real-git-based
        tests moved to `internal/archive/archive_test.go` (using
        `execx.Run`/`gitcli.Clone` instead of `cfg`/`cloneRepo`) with
        equivalent assertions, plus `confirm_test.go`'s two tests moved to
        `internal/archive/confirm_test.go`. Root `confirm_test.go` was
        **kept** (not deleted) since it still tests the shim's one real
        piece of logic (`cfg.Yes` → `DefaultConfirm`'s parameter).
        **Added the shim-wiring tests up front again**
        (`archive_shim_test.go`): `TestArchiveRepoShimMapsFieldsIntoManifest`
        checks `cfg.Org`/`repo.*` actually reach the written manifest —
        new coverage, not just moved, since the original `archive_test.go`
        never asserted on `manifest.Org`/`NameWithOwner`/`URL`/
        `DefaultBranch`/`PushedAt`/`ArchivedAt` at all. Verified both new
        shim tests actually catch their regressions (a hardcoded wrong
        owner, and a hardcoded wrong clone directory) in scratch copies.
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package and
        all six `internal/` packages so far, with the one necessary,
        narrowly-scoped test change (`confirm_test.go` no longer calls
        `isInteractive` directly, since it moved). Reviewed by an
        independent agent (PASS, the most thorough review in this series
        given the size of this step): confirmed byte-for-byte logic
        identity including the "Step N" comments, zero `ghcli` dependency
        in `internal/archive`, correct closure capture semantics for
        `clone`, and independently reproduced both new shim-test
        regression catches.
  - [x] `internal/plan`: `Decide`/`Action` — moved verbatim, but taking
        plain `RepoFacts`/`PrevState`/`Options` structs instead of
        `ghRepo`/`repoState`/`config`, so this package has zero dependency
        on anything else in the module (matching its "pure" billing more
        literally than before: previously `decide` still imported the
        config/repo types even though it never touched the filesystem).
        `PrevState` has independent `Archived`/`Cloned` bools rather than a
        shared status enum, so `plan` doesn't need to know what a
        caller's store calls its status values — root `plan.go`'s shim
        does that one piece of real mapping logic
        (`known && prev.Status == statusArchived`, etc.) itself. Root
        `plan.go` is otherwise a type alias (`action = plan.Action`) plus
        aliased constants and the one-line-bodied `decide` shim.
        **No new shim test needed**, unlike `ghcli`/`gitcli`: the existing
        root `plan_test.go` (kept verbatim, unlike those two) already
        drives the shim with real `ghRepo`/`repoState`/`config` values
        across 12 cases, which turns out to already exercise the
        `Archived`/`Cloned` mapping thoroughly — confirmed by swapping the
        two in a scratch copy before trusting this claim, which failed 7
        of those 12. Added `internal/plan/plan_test.go` with the same 12
        cases translated to the new types, plus one new case (`Known` true
        with neither `Archived` nor `Cloned` set — shouldn't happen given
        how the shim builds `PrevState`, but `Decide` has no way to enforce
        that, so it's worth pinning that it falls through sanely rather
        than misbehaving).
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package and
        all four `internal/` packages so far, zero existing test changes.
        Reviewed by an independent agent (PASS), which independently
        reproduced the 7-of-12 failure count and confirmed the new pinned
        edge case documents a real fallthrough rather than an arbitrary
        choice.
  - [x] `internal/engine`: `Env` (bundles `Exec`/`Confirm`/`Owner`/
        `Settings`, plus `ReposDir`/`ArchivesDir`/`StatePath` methods),
        `Task`/`Result`, `BuildTasks`/`RunTasks` (was `buildTasks`/
        `runTasks`, with `fixupRename`/`renameApplies`/`processTask`/
        `progressReporter` moving as unexported internals), and the
        single-repo path (`CloneInto`, was the real half of `cloneRepo`;
        `EnsureCloned`, was `ensureClonedForWorktree`) shared by a full
        sync's clone action, `worktree add`, and (in Phase 3) a standalone
        `clone` command. This is the biggest fan-in package in the series:
        depends on `ghcli`, `gitcli`, `store`, `plan`, `archive` and
        `settings` directly, since it's the orchestration layer that calls
        all of them.
        `Task`'s fields are capitalized (`Repo`/`Action`/`Reason`/`Prev`),
        unlike the old lowercase `task{repo,action,reason,prev}` — the one
        real, if small, call-site change in this step, since a type alias
        can't rename a struct's own field names. Two call sites in `run()`
        (the dry-run print loop, and an "actionable" count used only for
        the "syncing N repos" message) needed `t.repo`/`t.action`/
        `t.reason` → `t.Repo`/`t.Action`/`t.Reason`. `result`'s fields were
        already capitalized before this step, so no `result`/`Result`
        call site needed to change.
        `buildEnv(cfg) engine.Env`, a new small root-level helper, adapts
        `cfg` into the plain `Env` every `engine` function takes — the one
        piece of real logic `main.go`'s shims have of their own now.
        **Found and fixed leftover dead code from the Phase 2 steps before
        this one**, surfaced by this step removing the last production
        callers: `archive.go`'s `confirmAndRemoveWorktrees` shim had
        *already* been dead since the `archive` extraction step (nothing
        ever called it — `archive.Archive` always called
        `archive.ConfirmAndRemoveWorktrees` internally, not through the
        root shim; this should have been caught by that step's review and
        wasn't, since Go gives no unused-function warning the way it does
        for unused imports/variables). This step's own changes made eleven
        more functions production-dead the same way, once `engine`'s
        `processTask`/`fixupRename` started calling `gitcli`/`ghcli`/
        `archive` directly instead of through `main.go`'s wrappers:
        `archiveRepo` (duplicated the exact `archive.Repo`-building logic
        `engine`'s archive branch now has, which is worse than merely dead
        — two copies that could drift), `cloneURL`, and `isDirty`/
        `updateWorktree`/`headInfo`/`tags`/`linkedWorktrees`/
        `setRemoteURL`/`readManifest`/`writeManifest`/`writeTarball`. All
        removed, along with the type aliases/consts that only existed to
        support them (`archiveTag`, `archiveManifest`, `manifestVersion`).
        Found by grepping each shim's call sites after this step's edits,
        not by assumption — `runGit`/`fetchRepo`/`getRepo`/`listRepos`/
        `validRepoName`/`tarballPath`/`localArchiveExists` were each
        individually confirmed to still have a real production caller
        (mostly in `worktree.go`) before being left alone.
        **Correction after review:** this list, and the review that
        checked it, both missed a twelfth: `cloneRepo` itself. Once
        `worktree.go`/`archive.go`/`main.go`'s `processTask` all call
        `engine.CloneInto`/`engine.EnsureCloned` directly, `cloneRepo`'s
        only remaining callers anywhere in the repo are tests
        (`worktree_test.go`, `main_test.go`, `git_shim_test.go`) using it
        as a fixture-setup convenience. Rather than rewrite roughly a
        dozen test call sites to build an `engine.Env` and call
        `engine.CloneInto` directly for a one-line forward with no
        remaining logic of its own to protect, `cloneRepo` was kept and
        its doc comment corrected to say plainly that it is now test-only
        — recorded here rather than left as a silent inaccuracy a second
        time.
        Test coverage for the removed shims didn't disappear: it had
        already moved to the relevant `internal/` package's own test suite
        in earlier steps (`gitcli_test.go`, `ghcli_test.go`), or — for the
        `archiveRepo` field-mapping tests specifically — moved just now
        into `internal/engine/engine_test.go`
        (`TestProcessTaskArchiveMapsFieldsIntoManifest`/
        `TestProcessTaskArchiveComputesDir`, exercising the archive branch
        through `RunTasks` since `processTask` itself is unexported).
        `git_shim_test.go`/`gh_shim_test.go` were trimmed to drop
        coverage of the now-removed shims (`TestGitShimsForwardDirAndTimeout`
        lost 6 of 8 subtests; `TestCloneURLShimForwardsProtocol` was
        deleted outright, its ground already covered by
        `internal/ghcli/ghcli_test.go`'s own `TestCloneURL`).
        One real test bug caught during this step, by me rather than
        review: the first draft of `engine_test.go`'s `testEnv` helper
        forgot to set `Settings.Concurrency`, so `RunTasks`'s worker pool
        started zero goroutines and silently returned an empty result
        slice instead of hanging or erroring — worth a comment in the test
        file so the next person doesn't repeat it, since this failure mode
        gives no obvious signal pointing at the actual cause.
        **Every pinned test, plus the real-git `TestRunEndToEnd`, passed
        unchanged** — the strongest evidence available that moving this
        much orchestration logic didn't change observable behavior.
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package and
        all eight `internal/` packages. Reviewed by an independent agent:
        **FAIL on the first pass**, correctly catching two things this
        entry's first draft got wrong — the missed `cloneRepo` deadness
        above, and a stale, orphaned doc-comment fragment (leftover from
        an edit collision) sitting above `buildEnv` in `main.go` with no
        connection to it. Both fixed; re-verified clean on build/vet/fmt/
        test. The review independently re-derived the exact dead-function
        count (12, not the 11 first claimed) by grepping each name itself
        rather than trusting the list — worth calling out as the review
        process working as intended. Also fixed, flagged by the same
        reviewer as a minor non-blocking aside: `deps.go`'s `ConfirmFunc`
        doc comment still said "the seam `archiveRepo` uses," naming a
        function this very step deleted.
  - [x] `internal/settings`: `Settings` (was the flat part of `config`),
        `Default`/`DefaultRoot`/`Validate`, `FileConfig`/`LoadFileConfig`/
        `ResolveConfigPath`, and the whole Phase 1 table
        (`CommandID`/`Setting`/`SettingsFor`/`BindSettings`/
        `ResolveSettings`) — moved verbatim.
        **The one real design decision in this step:** `config` keeps its
        `Deps` field and gains an `Org` field (both root-specific; neither
        is a "setting" resolvable from a flag/env/file), but it needed
        every other field (`Root`, `Concurrency`, `Protocol`, etc.) to move
        into `settings.Settings` — and those fields are read via plain
        field access (`cfg.Protocol`, `cfg.Timeout`, ...) in dozens of
        places across every file touched in this whole phase so far.
        Rather than rewrite every one of those call sites, `config` embeds
        `settings.Settings` anonymously:
        ```go
        type config struct {
            settings.Settings
            Org  string
            Deps deps
        }
        ```
        Go promotes the embedded struct's fields, so `cfg.Protocol` keeps
        working completely unchanged — confirmed by the fact that this
        step needed **zero** changes to any of the pervasive
        `cfg.<SettingField>` call sites in `git.go`/`gh.go`/`archive.go`/
        `worktree.go`/engine-shaped code in `main.go`. The only casualties
        were struct **literals** using flat field names, since Go doesn't
        allow setting a promoted field that way: exactly two existed
        (`config{Yes: true}` in `confirm_test.go`,
        `config{Root: root, Org: "testorg"}` in `main_test.go`), both
        updated to `config{Settings: settings.Settings{...}, ...}`.
        `validateConfig` now checks `Org` itself, then delegates everything
        else to `settings.Validate(cfg.Settings)`; `worktree.go`'s three
        inline root/protocol/timeout checks were replaced by the same
        `settings.Validate` call — safe because `Concurrency`/`MaxRepos`
        (the two fields `Validate` also checks but worktree commands don't
        expose) always sit at their valid defaults for a worktree `cfg`,
        confirmed by reasoning through `defaultConfig()` rather than
        assumed. One minor, intentional behavior difference: if *multiple*
        settings are simultaneously invalid, which error wins differs
        slightly from before for worktree commands, since `Validate`'s
        internal check order doesn't match the old inline order exactly;
        no test depends on this and it only matters when more than one
        flag is wrong at once.
        `settings_test.go` was deleted; its three tests (which touch
        unexported fields like `s.flagName`/`s.configKey` and so can only
        live inside the package now) moved to
        `internal/settings/settings_test.go` verbatim, except
        `TestBindSettingsRegistersExactlyDeclaredFlags` dropped its
        `--help`-specific assertions (registering `--help` is `gh.go`'s
        job via `newFlagSet`, not `BindSettings`'). Added
        `settings_shim_test.go` at the root for exactly what moved out of
        that test (`newFlagSet`+`bindSettings` together register `--help`
        plus the right settings) plus two new ones pinning the embedding
        itself: `resolveSettings`'s shim actually mutates `cfg`'s embedded
        `Settings` (not some other copy), and `validateConfig` actually
        checks both `Org` and `settings.Validate`. Verified all three new
        shim tests catch real regressions (settings applied to a scratch
        copy instead of `cfg`, and a `validateConfig` that silently skips
        `settings.Validate`) in scratch copies before trusting them.
        **Every pinned test from Phase 1** (`TestPin*`, exact `--help`
        text included) **passed unchanged**, the strongest confirmation
        available that the embedding is invisible to observable behavior.
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package and
        all seven `internal/` packages so far. Reviewed by an
        independent agent (PASS): confirmed the embedding is truly
        anonymous, confirmed no other `config{...}` literal was missed by
        grepping the whole repo, and independently reproduced both new
        shim-test regression catches.
  - [x] `internal/cli`: `Run` — the only exported name, called by the
        root `main.go`'s one-line `func main()`. Everything else that used
        to live in root `package main` moved here almost entirely
        unchanged: there was no middle ground between "a separate
        package" and "all of it," since `cli` can't import a `package
        main` to borrow its `config`/shim/dispatch machinery, and nothing
        left in root after `engine`/`settings`/etc. extraction was
        independently useful to anything else. `archive.go`, `confirm.go`,
        `deps.go`, `gh.go`, `git.go`, `plan.go`, `settings.go`, `state.go`,
        `worktree.go`, and the non-`main()` bulk of `main.go` all moved
        verbatim (`package main` → `package cli`, `run` → `Run`, nothing
        else) into `internal/cli`. Added a package doc comment to
        `internal/cli` — the only package in the tree that didn't have one
        yet, since it was assembled from pre-existing files rather than
        written fresh.
        **Still open, deferred to its own step:** the org → owner
        identifier rename across `internal/cli` (it inherited `cfg.Org`,
        `orgNamePattern`, `cmdWorktreeAdd`'s `org, repoName` locals, etc.
        verbatim from the files it was assembled from) — see that bullet
        below.
- [x] **Tests, realistically.** Turned out more mechanical than planned:
      the plan expected to "export small APIs and rewrite some setup"
      moving `main_test.go`/`worktree_test.go` across the boundary. In
      practice the only change needed anywhere, across every moved test
      file (`main_test.go`, `worktree_test.go`, `pin_test.go`,
      `confirm_test.go`, `git_test.go`, `plan_test.go`, and the
      `*_shim_test.go` files from earlier steps), was renaming `run(` →
      `Run(` call sites — one exported name was all that was needed, since
      the tests already lived in the same package as their subjects and
      moved with them unchanged. No other API needed exporting, and no
      test setup needed rewriting.
- [x] Before/after check: done via the existing pinned-test suite rather
      than a separate manual binary diff — `TestPin*`'s exact `--help`
      text and exit-code assertions, plus the real-git `TestRunEndToEnd`,
      are a stricter version of the "diff `--dry-run` output against the
      old binary" check this bullet originally asked for (byte-exact
      assertions beat an eyeballed diff), and all passed unchanged in
      their new location. Additionally smoke-tested the actual built
      binary by hand (`--help`, `worktree add --help`) after the move.
      `go build`/`vet`/`gofmt`/`test -race` clean on the root package (now
      just `func main()`) and all eight `internal/` packages. Reviewed by
      an independent agent: **FAIL on the first pass**, catching exactly
      the kind of thing a mechanical rename is prone to missing — two
      references to `run()` inside `resolveConfig`'s own doc comment, in
      the same function that was renamed to `Run` in this very change,
      left unupdated. Fixed; re-verified clean on build/vet/fmt/test, and
      the reviewer's other nine checks (no orphaned files, no import
      cycle, no mangled `runWorktree`/`runGit`/`runTasks` call sites, byte-
      identical `Run`/`resolveConfig` logic, correct new root `main.go`
      wiring, pinned tests unchanged, a hand-built binary's `--help`
      output matching the pinned text, and an accurate package doc
      comment) all passed on the first attempt.
- [ ] Rename `org` → `owner` in internal identifiers here (the regex is the
      same; orgs and users look the same to `gh`). Keep user-facing strings
      unchanged until Phase 4. Check `gh repo list <user>` against a real
      user account to confirm the output shape.
- [ ] CI green.

## Phase 3 — Workspace model (still shipped as gh-org-clone)

Each bullet group is its own jj change.

- [ ] jj: `jj new -m "feat(state): v2 state with explicit tracking and migration"`.
  - [ ] `repoState.Tracked`, v1 → v2 migration in `store`, backup of a
        corrupt file, and tests for migrate / unknown-version /
        corrupt-backup.
  - [ ] Implement the tracking rule (tracked = config lists it OR state
        marks it explicit) as one function, with a table test covering
        each combination of config-listed, owner-listed and
        state-explicit, plus a repo removed from the config.
- [ ] jj: `jj new -m "feat: clone and untrack commands"`.
  - [ ] Promote `ensureClonedForWorktree` into
        `engine.SyncOne(owner, repo, explicit=true)`. `clone` and
        `worktree add` both call it.
  - [ ] Semantics:
    - already present, `pushedAt` unchanged → skip (zero git calls)
    - already present, `pushedAt` changed → fetch + fast-forward, like
      sync
    - archived upstream → same archive rules as sync (respect `archive`)
    - already archived locally → report the tarball and don't re-clone
      (matching `worktree add` today)
  - [ ] `untrack` flips state only. It prints where local data remains and
        never deletes. It fails with an error naming the config entry if
        the config tracks the repo.
- [ ] jj: `jj new -m "feat(config): owners and repos in the workspace config"`.
  - [ ] `owners` (with per-owner overrides) and `repos` keys in the
        settings/file config, strictly validated.
  - [ ] Expand a leading `~/` in `root` and `worktreeRoot` from every
        source (flag, env, file) before the absolute-path check. Test that
        `~user/`, `$HOME` and relative paths are still rejected.
- [ ] jj: `jj new -m "feat(sync): workspace-wide and tracked-only sync"`.
  - [ ] `sync` with no args: configured owners (one listing each) +
        explicit repos outside them.
  - [ ] `ghcli.ViewRepos` GraphQL batch for explicit repos, with a
        `ghJSONFields`-equivalent selection and tests using a canned
        response via fake exec.
  - [ ] `sync --tracked-only` and `sync <owner>/<repo>`.
  - [ ] One shared worker pool across owners, with per-owner state saved
        when that owner's last task completes (see "Multi-owner sync").
  - [ ] Per-owner fail-fast locks, held from planning until that owner's
        state is saved. A locked or failed owner is reported and skipped;
        the rest finish, and the run exits `1`. Test with a pre-created
        lock file for one of two owners.
  - [ ] Bare `<owner>` alias for `sync <owner>`, applied only for
        non-reserved words. Test that an org named `sync` or `clone` is
        reachable through `sync <owner>`.
  - [ ] GraphQL batch edge cases: `null` repo with partial `data`,
        non-zero exit with usable `data`, rename (behavior as verified
        against the real API), and transfer to another owner. Each gets a
        canned-response test.
  - [ ] The "in state but not in listing" note must not fire for explicit
        repos that the owner filter excludes (e.g. forks).
- [ ] jj: `jj new -m "feat(worktree): configured worktree placement"`.
  - [ ] `worktreeRoot`/`worktreePath` settings, template expansion,
        branch sanitizing, a `..`/escape check, and an optional `path`
        argument.
  - [ ] Defaults: current directory + `{repo}`. Tests: run from a temp
        directory with no config (the worktree lands at `<cwd>/<repo>`,
        with no owner in the path), a second branch of the same repo in the
        same directory (fails with the explicit-path/`{branch}` hint, no
        renaming), a configured template with `{branch}` and a `/` in the
        branch name, a relative `worktreeRoot` in config (rejected), and an
        explicit path outside `worktreeRoot` (allowed).
  - [ ] `worktree remove <path>` (path-only form) and a workspace-wide
        `worktree list`.
- [ ] Tests for all of the above, plus a no-op cost test: fake exec
      asserts a no-op `sync` makes exactly one `gh` call per configured
      owner plus one per batch of explicit repos, and **zero** git calls.

## Phase 4 — Rename the surface

- [ ] jj: `jj new -m "feat!: rename gh-org-clone to gh-workspace"`.
- [ ] `go.mod` module path and imports.
- [ ] Env prefix `GH_ORG_CLONE_*` → `GH_WORKSPACE_*` (settings table, one
      place now). No fallback, per Phase 0.
- [ ] Default data directory `…/gh-org-clone` → `…/gh-workspace`, and config
      path `~/.config/gh-workspace/config.json`.
- [ ] Every user-facing `gh org-clone` / `gh-org-clone` string: grep for
      them, including the lock error, "requires gh on PATH" messages, and
      usage text.
- [ ] Rename the GitHub repo to `swanysimon/gh-workspace` (gh extensions
      must be named `gh-<name>`). GitHub redirects the old URL; nothing
      else depends on the old name.
- [ ] README: install, workspace model, command reference, config table
      (generated from the settings table if cheap to do), and file layout.
- [ ] AGENTS.md: rewrite for the workspace model. Keep the still-true
      invariants (no-op cost, per-repo skipping, never deleting, the
      worktree prompt, the jj-first rule).
- [ ] No MIGRATING.md (clean break). Manual steps for my own machine:
      `gh extension remove org-clone`, `gh extension install
      swanysimon/gh-workspace`, `mv ~/.local/share/gh-org-clone
      ~/.local/share/gh-workspace` (v1 state migrates on first run), move
      `~/.config/gh-org-clone/config.json` to the new path, and rename any
      `GH_ORG_CLONE_*` exports in my shell config.

## Phase 5 — Release

- [ ] jj: `jj new -m "chore: release prep"` (only if changes are needed).
- [ ] CI green on Linux and macOS.
- [ ] Manual smoke test on a scratch root:
  - [ ] fresh `sync` of an owner, then a second no-op run
  - [ ] `clone` of a repo outside any configured owner
  - [ ] `sync --tracked-only`
  - [ ] `worktree add` with no path and no config (lands under the current
        directory as `<repo>`)
  - [ ] `worktree add` with a configured template and no path
  - [ ] path-only `worktree remove`
  - [ ] archive of a repo with a live worktree (the prompt fires; the
        non-TTY run refuses)
  - [ ] v1 state migration from a real old root
  - [ ] two-owner `sync` while the other owner's lock is held (one
        skipped, exit `1`, the other owner's state saved)
  - [ ] an explicitly tracked repo that was deleted or made inaccessible
        (reported, nothing removed)
  - [ ] `~/` paths in the config file
- [ ] Tag, then confirm the precompiled binaries publish and
      `gh extension install swanysimon/gh-workspace` works from clean.
- [ ] Do the manual migration steps from Phase 4 on my own machine.

## Open questions

- [ ] Should `archive` ever be a manual verb? Leaning no: archiving means
      "archived upstream," not a local choice.
- [ ] Worktree metadata: rely purely on `git worktree list` (current
      approach, no extra state), or record worktrees in state for faster
      workspace-wide listing and `status`? Leaning git-only until `status`
      proves it's too slow.
- [ ] jj workspaces. `jj workspace add` is jj's version of a git
      worktree, and the maintainer uses jj. Should `worktree add` support
      jj-colocated clones (e.g. `--vcs jj`, or detecting `.jj` in the
      central clone)? That affects the archive-with-worktrees check too,
      since jj workspaces don't appear in `git worktree list`. Out of scope
      for this plan, but the `gitcli` worktree helpers should sit behind a
      small interface so a jj implementation can be added later.
- [ ] Multiple workspaces (e.g. work vs personal): is `--config` / `--root`
      enough, or do we want named workspaces (`--workspace work`)? Out of
      scope unless it falls out naturally from the settings table.
