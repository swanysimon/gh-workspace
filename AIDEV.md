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

- [x] jj: `jj new -m "refactor: extract internal packages"`. **Deviation,
      recorded as it happens rather than only at the end**: given how large
      this phase is, it's being done as a sequence of per-package jj
      changes (dependency order: `execx` → `ghcli`/`gitcli` →
      `store`/`plan` → `archive` → `settings` → `engine` → `cli` → shrink
      `main.go`), each reviewed before the next starts, rather than one
      single commit for the whole phase — matching how Phase 1's steps
      were handled, and keeping each reviewable on its own.
- [x] **Keep `package main` at the repo root.** `gh-extension-precompile`
      builds the root package by default. Moving `main` to `cmd/` would need
      a `build_script_override`. Verify against the action's docs before
      changing anything here. The root `main.go` becomes a few lines calling
      `internal/cli`.
  - Checked `.github/workflows/release.yml`: it invokes
    `cli/gh-extension-precompile` with no `build_script_override` and no
    path override, i.e. it relies on the action's default, which builds
    from the repo root. `main.go` stayed at the root throughout this
    phase and is now a 10-line file calling `cli.Run`; confirmed
    repeatedly (every extraction step) that `go build .` from the root
    still produces a working binary. Did not literally re-read the
    action's own source/docs beyond the workflow file that invokes it;
    worth a final confirmation at release time, not blocking this far
    out from a release.
- [x] Layout, under `internal/` per the Phase 0 library-reuse decision
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
- [x] Rename `org` → `owner` in internal identifiers here (the regex is the
      same; orgs and users look the same to `gh`). Keep user-facing strings
      unchanged until Phase 4. Check `gh repo list <user>` against a real
      user account to confirm the output shape.
  - `config.Org` → `config.Owner`; `orgNamePattern` → `ownerNamePattern`;
    `orgDir` → `ownerDir`; `parseOrgRepo` → `parseOwnerRepo` (and its
    `org, repo` return values → `owner, repo`); `loadState`'s `org`
    parameter → `owner`; every local variable named `org` in
    `cmdWorktreeAdd`/`cmdWorktreeRemove`/`cmdWorktreeList` → `owner`. Two
    comments describing internal mechanics (not user-facing text) were
    reworded to match the "owner" terminology `store`/`engine` already
    established in earlier steps: `acquireLock`'s "takes the per-org
    lock"/"mutates an org's clones" → "per-owner lock"/"an owner's
    clones", and `ensureClonedForWorktree`'s "must hold the org lock" →
    "must hold the owner's lock".
    `store.State.Org`'s field name and json tag were, correctly, left
    alone — that's a different field (on-disk state, not resolved config),
    out of scope here just as it was in the `store` extraction step. One
    place both appear together (`main_test.go`'s
    `state{Version: stateVersion, Org: cfg.Owner, ...}`) needed care to
    rename only the right half.
    **Every printed string was left exactly as it was** — flag names
    (`--root`, `--config`), error text (`"org must not be empty"`,
    `"org %q is not a valid GitHub org name"`, `"expected exactly one org
    argument, got %d"`, `"expected <org>/<repo>, got %q"`), usage lines
    (`"gh org-clone worktree add [flags] <org>/<repo> ..."`), the program
    name (`gh-org-clone`), the env var prefix (`GH_ORG_CLONE_*`), and the
    default data/config paths all still say "org," deliberately, per this
    bullet's own instruction and Phase 4's rename plan. This is enforced,
    not just promised: every `TestPin*` exact-text assertion (`--help`
    output, error strings) passed with zero changes, which would have
    failed immediately had any printed string drifted.
    Did not check `gh repo list <user>` against a real user account (no
    network access in this environment); noted as still-open verification
    before relying on the owner/org-look-the-same-to-`gh` assumption in
    Phase 3.
    `go build`/`vet`/`gofmt`/`test -race` clean on the root package and all
    eight `internal/` packages; manually re-smoke-tested the built binary's
    `--help` and `worktree add --help` output by hand after the rename.
    Reviewed by an independent agent: **FAIL on the first pass** — caught
    that the local `org` variables inside `cmdWorktreeAdd`/
    `cmdWorktreeRemove`/`cmdWorktreeList` themselves (as opposed to the
    `config.Org` field and `parseOrgRepo`'s own name/return values, both of
    which this entry had already renamed) were missed, exactly the kind of
    thing a search for the type/function names alone doesn't catch. Fixed
    across all three functions; re-verified clean on build/vet/fmt/test and
    confirmed every one of the reviewer's other six checks (no stray
    `cfg.Org`/`orgDir`/`orgNamePattern`/`parseOrgRepo` anywhere including
    tests, `store.State.Org` genuinely untouched, every "org"-containing
    printed string verified unchanged by grep and by a hand-built binary,
    full pinned-test suite passing) had already passed on the first
    attempt. Re-verified clean on a second review pass after the fix.
- [x] CI green. Confirmed via `gh run list`: the `ci` workflow run for
      `fda0ee2` ("feat(worktree): configured worktree placement", the tip of
      `main`, already pushed to `origin/main`) completed `success` on both
      `ubuntu-latest` and `macos-latest`. Every commit in this phase landed
      directly on `main` (no branch/PR was used), so this is the real
      workflow run, not just the local `gofmt`/`vet`/`test -race` signal
      each step already checked along the way.

## Phase 3 — Workspace model (still shipped as gh-org-clone)

Each bullet group is its own jj change.

- [x] jj: `jj new -m "feat(state): v2 state with explicit tracking and migration"`.
  - [x] `RepoState.Tracked` (bool), `store.Version` bumped 1 → 2, v1 → v2
        migration in `LoadState` (every v1 entry becomes `Tracked: true`,
        with the reasoning for "true, not false" written directly into
        `LoadState`'s doc comment: v1 can't distinguish an owner-swept
        entry from a `worktree add`-sourced one, and under-tracking risks
        `sync --tracked-only` silently going quiet on a repo someone is
        using, which is worse than some redundant syncing), and a corrupt
        file now gets backed up to `<path>.corrupt-<unix-timestamp>`
        (content preserved, path named in the warning) instead of just
        being discarded. Three new tests:
        `TestLoadStateMigratesV1ToV2` (migrates, preserves other fields,
        round-trips through a save/reload as v2 with no further
        migration), `TestLoadStateCorruptBacksUpFile` (backup exists,
        exact content, path named in the warning), and the existing
        `TestLoadStateCorrupt`'s "wrong version"/"nil repos" cases
        rechecked against the new logic (the latter's `version: 1` fixture
        now exercises the migration path too, harmlessly — confirmed by
        running it, not assumed).
  - [x] `store.IsTracked(ownerConfigured, repoConfigured, explicit bool)
        bool` implements the tracking rule as a plain three-way `OR`,
        taking bare bools rather than a concrete owners/repos config
        shape, since that config doesn't exist until the next Phase 3
        step — its caller will compute the first two args once it does.
        `TestIsTracked` covers all 8 combinations despite the function's
        triviality, since this one `OR` is the hinge every "never delete,
        never silently stop syncing" guarantee in the whole tool depends
        on; it's worth a named test that fails loudly if someone
        "simplifies" it into an `AND` or drops an argument later.
  - [x] **Found and fixed a real bug while wiring this up, not just adding
        the field**: `internal/cli/main.go`'s regular sync loop writes a
        brand new `repoState` literal for every repo on every run, and
        that literal didn't carry the previous entry's `Tracked` bit
        forward. Left as-is, a repo explicitly added via `worktree add`
        would have silently reverted to untracked the very next ordinary
        sync — exactly the kind of regression this field exists to
        prevent, introduced by the same step that added the field.
        Fixed by reading `st.Repos[res.Name].Tracked` forward into the new
        literal; `TestRunPreservesTrackedAcrossSync` pins it (pre-seeds a
        `Tracked: true` entry, runs a real sync, confirms it survives),
        verified against a reintroduced regression in a scratch copy
        before trusting it. Also wired `engine.EnsureCloned` (the
        clone-and-record-state path `worktree add` already uses, and a
        future `clone` command will too) to set `Tracked: true` on write
        — by construction, anything going through that path is an
        explicit add; `TestEnsureClonedMarksTracked` in
        `internal/engine` pins it, same verify-the-regression-first
        discipline.
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package and
        all eight `internal/` packages. Reviewed by an independent agent
        (PASS): independently reproduced both regressions in scratch
        copies, confirmed the "unknown version" path genuinely never
        writes a backup (by experiment, not just reading the code), and
        grepped every `RepoState`/`repoState` literal in the codebase for
        a missed carry-forward site (`archive.go`'s two constructions
        checked and confirmed harmless, since `engine.Result` has no
        `Tracked` field for that data to even flow through).
- [x] jj: `jj new -m "feat: clone and untrack commands"`.
  - [x] Added `engine.SyncOne(ctx, env, repo, explicit, stderr) (Result,
        error)`, **not** a promotion of `ensureClonedForWorktree`/
        `EnsureCloned` as literally written — real design finding, recorded
        rather than silently resolved: `worktree add`'s existing behavior
        on an archived-upstream repo (always clone it, then refuse to add
        a worktree, leaving an ordinary clone behind) and the plan's
        stated semantics for `clone` ("archived upstream → same archive
        rules as sync", i.e. tarball-and-delete when `--archive` is set)
        are genuinely different, deliberate behaviors for the same
        upstream state — `worktree add`'s whole purpose is a live checkout,
        so archiving on its behalf would be counterproductive. Retrofitting
        `worktree add` onto `SyncOne` would have silently changed that
        established, tested behavior. Resolution: `SyncOne` reuses the
        exact decision pipeline (`plan.Decide`) and single-task execution
        (`processTask`, called with a `nil` progress reporter, which it
        already handles) that `BuildTasks`/`RunTasks` use for a full sync,
        so a repo resolved one at a time gets the identical skip/fetch/
        archive/adopt decision a full sync would have made for it.
        `EnsureCloned` (unconditional clone, used by `worktree add` only)
        and `SyncOne` (decide-based, used by `clone` only) now coexist as
        two distinct single-repo paths with different, documented
        purposes, cross-referencing each other's doc comments to explain
        why the other exists. `worktree add` itself is **unchanged**.
        Six new tests in `internal/engine/engine_test.go` cover exactly
        the plan's four semantics plus two more:
        `TestSyncOneClonesWhenNothingLocal`,
        `TestSyncOneSkipsWhenUnchanged` (asserts zero git calls),
        `TestSyncOneFetchesWhenChanged`,
        `TestSyncOneArchivesWhenArchivedUpstream`,
        `TestSyncOneAdoptsExistingArchive` (zero git calls),
        `TestSyncOneDoesNotWriteStateOnFailure` (matches a full sync's
        per-repo failure handling: a failed repo's state entry is left
        alone, not overwritten with a result from a run that didn't
        happen, so it's retried next time).
  - [x] `clone <org>/<repo>...` (new `internal/cli/clone.go`): resolves
        each argument via `gh repo view`, calls `SyncOne` with
        `explicit=true` under the owner's lock, and continues past a
        failed argument to try the rest — the same per-repo
        continue-past-failure philosophy a full sync already uses —
        exiting `1` if any argument failed. Accepts `--root`/`--protocol`/
        `--timeout`/`--config` (shared with `worktree`) plus `--archive`/
        `--force` (shared with `sync`, new to this command specifically):
        `internal/settings` gained a `CmdClone` `CommandID` with exactly
        that membership, and `TestSettingsFor`/
        `TestBindSettingsRegistersExactlyDeclaredFlags` were extended
        (not just left alone) to assert `CmdClone`'s exact flag set, the
        same discipline applied to `CmdWorktree` when it was created.
  - [x] `untrack <org>/<repo>` (same file): clears `Tracked` without
        touching any local data, exactly as specified. Errors if the repo
        has no state entry at all (nothing to untrack); succeeds as a
        no-op if it exists but is already untracked. **The "fails if the
        config tracks the repo" check is deliberately not implemented
        yet** — the config's `owners`/`repos` lists don't exist until the
        next Phase 3 step — and is left as an `AIDEV:` comment at the
        exact spot it needs to go, rather than silently dropped from the
        plan or faked against a type that doesn't exist.
  - [x] `newWorktreeFlagSet`/`resolveWorktreeConfig` renamed to
        `newSubcommandFlagSet`/`resolveSubcommandConfig` (the latter now
        takes a `cmd commandID` parameter instead of hardcoding
        `cmdWorktree`), since `clone`/`untrack` need the exact same
        flags>env>file>defaults machinery `worktree`'s subcommands already
        had, just for a different settings subset. All three existing
        `worktree` subcommands' call sites updated to pass `cmdWorktree`
        explicitly; behavior unchanged (confirmed by the full existing
        `worktree_test.go` suite passing with zero assertion changes).
  - [x] `Run()`'s top-level dispatch extended from a single `if
        args[0]=="worktree"` check to a `switch` over `worktree`/`clone`/
        `untrack` — consistent with the "reserved words" tradeoff already
        documented in this file's workspace-model section (an owner
        literally named `clone` or `untrack` is now unreachable as a bare
        `sync <owner>` the same way `worktree` already was).
  - [x] **User-facing text intentionally changed, not deferred**: the
        top-level `--help` USAGE block now lists `clone`/`untrack`. This is
        new functionality, not a Phase-4 rename, so the pinned
        `TestPinTopLevelHelp` golden text was updated deliberately (and
        caught the change exactly as designed before the update).
  - [x] New test coverage in `internal/cli/clone_test.go`:
        `TestCmdCloneClonesAndTracks`, `TestCmdCloneSecondRunSkips`,
        `TestCmdCloneMultipleArgsContinuesPastFailure` (one bad arg, one
        good — both the exit code and that the good one still landed),
        `TestCmdCloneHelpExitsZero`, `TestCmdUntrackClearsTrackedBit`
        (and that nothing else in the entry changes),
        `TestCmdUntrackNoLocalDataIsAnError`,
        `TestCmdUntrackAlreadyUntrackedIsANoOp`,
        `TestCmdUntrackNeverDeletesLocalClone`, and
        `TestRunDispatchesToCloneAndUntrack` (through `Run()` itself, not
        the `cmd*` functions directly). Reused `worktree_test.go`'s
        existing `stubGhRepoView` helper rather than duplicating it (found
        by `go vet` catching the redeclaration on the first build attempt).
        Manually smoke-tested the real binary's `--help`,
        `clone --help`, `untrack --help`, and both commands' bad-usage
        output.
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package and
        all eight `internal/` packages, every `TestPin*` passing.
        **Process note:** this step was accidentally combined with the
        prior "v2 state" step into one jj commit (see the commit
        description) after forgetting to run `jj new` between them.
        Reviewed as the full combined diff accordingly, including a
        re-verification that the already-reviewed state-v2 content
        (migration, corrupt backup, the two Tracked bug fixes) wasn't
        altered by this step's edits to the same files — both bug-fix
        regressions were reproduced again in a fresh scratch copy as part
        of that re-verification. PASS.
- [x] jj: `jj new -m "feat(config): owners and repos in the workspace config"`.
  - [x] `owners` (`[]OwnerConfig{Name, IncludeForks, Archive, MaxRepos}`,
        each a per-owner override of the matching global default) and
        `repos` (`[]string`, `"<owner>/<repo>"` entries) added to
        `settings.FileConfig`. Strictly validated inside `LoadFileConfig`
        itself (not deferred to a later caller step) via a new
        `validateOwnersAndRepos`: every owner name and every repo's owner
        half must satisfy a new exported `settings.ValidOwnerName`; every
        repo's name half must satisfy `store.ValidRepoName`; duplicate
        owners and duplicate repos are both hard errors. Unknown keys
        inside an owner object are already caught for free by the
        existing top-level `json.Decoder.DisallowUnknownFields()`, which
        recurses into nested structs.
  - [x] **Deliberate refactor alongside this**: moved the owner-name regex
        that `internal/cli` had been keeping locally (`ownerNamePattern`)
        into `internal/settings` as the new exported `ValidOwnerName`,
        since the config's `owners`/`repos` validation needed the exact
        same rule and duplicating a regex that must stay in sync in two
        packages was worse than importing it. All three `cli` call sites
        (`validateConfig`, `parseOwnerRepo`, `cmdWorktreeList`) now call
        `settings.ValidOwnerName` instead; the local `ownerNamePattern` var
        and its `regexp` import were deleted from `internal/cli` entirely
        — confirmed no stale references remain anywhere, including
        comments.
  - [x] `settings.ExpandHome(path) (string, error)`: expands a leading
        literal `~/` only. `~user/` and `$HOME` are deliberately left
        untouched (and therefore still rejected by the absolute-path
        check), matching the plan's explicit requirement, not a shell's
        full tilde-expansion semantics. Wired into `internal/cli` via a
        new shared `expandConfigPaths(cfg *config) error`, called once
        after `resolveSettings` finishes and before validation in both
        `resolveConfig` (sync) and `resolveSubcommandConfig`
        (worktree/clone/untrack) — expanding the single already-resolved
        value once, after flag>env>file precedence has already picked a
        winner, has the same effect as expanding at each individual source
        and is simpler. `worktreeRoot` doesn't exist as a setting yet (it's
        the next Phase 3 step's job to introduce it), so only `root` is
        expanded for now; `expandConfigPaths` is where `worktreeRoot` will
        get the identical treatment once it exists.
  - [x] New tests: `internal/settings/settings_test.go` gained
        `TestLoadFileConfigAcceptsValidOwnersAndRepos`,
        `TestLoadFileConfigRejectsInvalidOwnersAndRepos` (9 subcases:
        invalid/duplicate owner, missing slash, empty owner/name half,
        invalid owner/name half, duplicate repo, unknown key inside an
        owner object), `TestValidOwnerName`, `TestExpandHome` (6 subcases
        including the three the plan calls out by name: `~user/`, `$HOME`,
        a plain relative path, all unchanged/still-rejected). Also had to
        add an explicit `owners`/`repos` exemption to the existing
        `TestSettingsTableConfigKeysMatchFileConfig`'s reverse-direction
        check (every `FileConfig` field must be claimed by a
        `settingsTable` entry) — found by running the suite, not
        anticipated in advance: `owners`/`repos` are structural config-only
        keys with no flag/env equivalent by design, so they were never
        going to have a `settingsTable` entry claiming them, and the test
        needed to say so explicitly rather than silently special-case them
        by accident.
        `internal/cli/main_test.go` gained three new `TestConfigRejects`
        subcases (invalid owner in config, malformed repos entry, plus the
        three not-expanded cases above re-verified at the `resolveConfig`
        level too) and three new standalone tests:
        `TestConfigExpandsHomeInRoot`, `TestConfigExpandsHomeInRootFromConfigFile`,
        `TestConfigAcceptsValidOwnersAndRepos`.
        `internal/cli/worktree_test.go` gained
        `TestResolveSubcommandConfigExpandsHomeInRoot`, pinning that the
        shared `expandConfigPaths` helper actually runs on the
        worktree/clone/untrack path too, not just sync's — confirmed this
        test is not vacuous by temporarily reverting the `expandConfigPaths`
        call in `resolveConfig` and `resolveSubcommandConfig` and watching
        both new tests fail with the expected "must be an absolute path"
        error, then restoring.
        Manually smoke-tested the real binary: a config file with
        `"root": "~/configtest-root"` plus valid `owners`/`repos` passes
        validation and proceeds to the (expected, no-network-here) `gh`
        call; a config file with an invalid owner name is rejected with
        the expected error text and exit code `2`.
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package and
        all eight `internal/` packages. Independently reviewed: PASS
        (including the reviewer independently re-verifying `ExpandHome`
        and `DisallowUnknownFields`'s nested-struct recursion with its own
        throwaway programs, not just reading the tests).
- [x] jj: `jj new -m "feat(sync): workspace-wide and tracked-only sync"`.
  - [x] `sync` with no args: every owner in config `owners[]` gets its own
        `gh repo list`, and every repo in config `repos[]` plus every
        state-`Tracked` repo under any owner directory discovered via
        `<root>/*/state.json` (whichever owner it belongs to, configured
        or not) gets refreshed through one batched `ghcli.ViewRepos` call
        spanning every owner at once. A repo an owner's own listing
        already covers is removed from that batch before it's built, so a
        fully-configured owner costs exactly one API call, same as today.
  - [x] `ghcli.ViewRepos` (new, in `internal/ghcli`): aliased
        `repository(owner:, name:)` GraphQL query, batched at 100 per `gh
        api graphql` call (`graphQLBatchSize`), same field selection as
        `JSONFields`. A `null` result for an alias is reported back as
        `RepoResult.Repo == nil` ("missing upstream"), not an error --
        only a response with no usable `data` at all is a ViewRepos error.
        `internal/execx.Run` was changed to return stdout even when the
        command exits non-zero (previously always `nil` on error), since a
        GraphQL response can legitimately come back as a non-zero gh exit
        with a perfectly parseable partial body; every existing caller
        already only reads its output when err is nil, so this is a
        strictly backward-compatible widening -- confirmed by the full
        existing suite passing unchanged. New tests:
        `TestRunPreservesStdoutOnError` (execx),
        `TestViewReposArgsAndQueryShape`,
        `TestViewReposNullEntryIsMissingNotError`,
        `TestViewReposNonZeroExitWithPartialDataStillParses`,
        `TestViewReposNoUsableDataIsAnError`,
        `TestViewReposEmptyResponseIsAnError`,
        `TestViewReposBatchesAtGraphQLBatchSize` (150 repos → 2 calls,
        100+50) (ghcli).
        **Not verified against the real API in this environment (no
        network access)**: whether `gh api graphql` ever actually behaves
        the "non-zero exit, usable partial data" way at all, versus always
        exiting 0 with `errors` present instead (which the common,
        exit-0 case already handles fine either way) -- flagged with an
        `AIDEV:`-style doc comment at the exact spot in `ghcli.go`, not
        silently assumed correct. Also not verified: GraphQL's `visibility`
        enum casing versus gh's REST-backed lowercase strings (harmless
        today -- nothing branches on `Repo.Visibility` -- but noted so a
        future caller that starts comparing it knows to check).
  - [x] `sync --tracked-only` (skips every owner's listing; only
        known-local-or-explicit repos get the batched refresh) and `sync
        <owner>/<repo>` (reuses `engine.SyncOne` with `explicit=false` --
        an ad hoc refresh, not a track action; only `clone`/`worktree add`
        mark `Tracked`). Both wired through a new `internal/cli/sync.go`
        and a new `sync` reserved subcommand
        (`gh org-clone sync [flags] [<org>[/<repo>]]`), added to the
        top-level `--help` USAGE block and `TestPinTopLevelHelp`'s golden
        text (deliberate, documented new functionality, same as the
        `clone`/`untrack` precedent). `--tracked-only` is a new
        `CmdSync`-only `settings.Setting`
        (`Settings.TrackedOnly`, no env var or config key -- a
        per-invocation choice about this run's shape, like `--force`).
        `runSingleOwnerSync` was extracted out of `Run()` (pure
        extraction, confirmed behavior-identical by the full existing
        suite passing unchanged with no test edits) so bare
        `gh org-clone <org>` and `sync <org>` share one implementation
        instead of two that could drift -- confirmed identical by
        `TestRunSyncOwnerArgMatchesBareTopLevel`, which runs both and
        diffs their stdout.
  - [x] **Found and fixed a real bug while wiring this up**: adding
        `--tracked-only` to `CmdSync`'s settings makes it available (via
        `bindSettings`) on the bare top-level path too, since that path
        resolves the exact same `CmdSync` settings scope as the `sync`
        subcommand -- but the bare path's `Run()` ignored
        `cfg.TrackedOnly` entirely, so `gh org-clone myorg --tracked-only`
        would parse successfully and then silently run a full listing
        anyway. Fixed by having `resolveConfig` also return the loaded
        `*fileConfig` (a small, mechanical signature change -- every call
        site, mostly in tests, updated) and having `Run()` branch to
        `runWorkspaceSync(ctx, cfg, fc, []string{cfg.Owner}, ...)` when
        `cfg.TrackedOnly` is set, exactly mirroring what the explicit
        `sync <org> --tracked-only` form does. Pinned by
        `TestRunTopLevelTrackedOnlySkipsListing` and
        `TestRunSyncTrackedOnlyFlagSkipsListing`
        (the explicit-form equivalent) -- both assert zero `gh repo list`
        calls while still allowing (and exercising) the batched lookup for
        the one locally-tracked repo. Regression-reproduced in a scratch
        copy before trusting the tests.
  - [x] `engine.MultiOwnerRun` (new): one shared pool of `poolSize`
        workers across every owner's tasks (`engine.OwnerWork`), routing
        each `Result` back to its owning entry via an index tag, saving
        that owner's `State` (via a new, separately-tested
        `applyResultsToState`, which carries a fresh repo ID and `Tracked`
        forward exactly like the single-owner path's own result loop) and
        calling its `Release` the moment its own task count reaches zero
        -- independent of every other owner's progress. New tests:
        `TestMultiOwnerRunSavesEachOwnerIndependently`,
        `TestApplyResultsToStateSkipsFailedAndCarriesTrackedForward`.
  - [x] Per-owner locks: a locked owner is reported and skipped, every
        other owner still finishes, and the run exits `1` --
        `TestRunSyncWorkspaceWideLockContentionSkipsOnlyThatOwner`
        pre-locks one of two configured owners and confirms the other
        still gets cloned, regression-reproduced by temporarily
        no-op'ing lock acquisition and watching the test fail with the
        locked owner cloned anyway.
        **Deliberate ordering deviation from the plan, recorded rather
        than silently diverging**: AIDEV.md's "Multi-owner sync" says a
        lock is "taken before that owner is planned (listing or batch
        lookup, then decide) and held until its state is saved." This
        implementation plans every owner first (listings, the cross-owner
        batch lookup, and `plan.Decide`) in `runWorkspaceSync`, and only
        acquires locks afterward, in `runWorkspacePlans`, immediately
        before `engine.MultiOwnerRun`. State is still only ever mutated
        after a lock is held, so this is not a correctness gap -- but
        under two concurrent runs, both could redundantly list/plan the
        same owner before only one of them wins the lock, costing an
        extra API call and giving up a little of the "fail fast before
        doing any work" benefit the stricter ordering would have. Restated
        here instead of fixed in this step: the fix requires merging
        lock/mkdir/sweep into the same per-owner planning loop while still
        skipping all three for a dry run (which must never lock), a
        larger restructure than was safe to make this late in an already
        very large step; worth revisiting.
  - [x] Bare `<owner>` alias for `sync <owner>`, non-reserved words only:
        already a direct consequence of `Run()`'s dispatch checking
        `args[0]` against `"sync"`/`"clone"`/`"untrack"`/`"worktree"`
        before ever treating it as a bare owner, extending the exact same
        pre-existing ambiguity `"worktree"` already had to `"sync"`,
        `"clone"`, and `"untrack"` -- no new code needed, nothing to
        regress.
  - [x] GraphQL batch edge cases: a `null` result and a non-zero exit with
        usable partial `data` are both handled and tested (see above). A
        transfer (the returned `nameWithOwner`'s owner half differs from
        the owner it was requested under) gets a note in the task's reason
        via `engine.BuildExplicitTasks`
        (`TestBuildExplicitTasksNotesTransfer`) -- data is never moved
        between owner directories automatically, exactly as specified.
        **Rename is not implemented for explicit repos, and is an
        explicitly flagged, deliberate gap, not an oversight**: the
        plan's own wording is conditional on real-API behavior this
        environment cannot check ("check whether `repository(owner:,
        name:)` follows redirects"). Implementing the ID-based
        reconciliation (reusing `fixupRename`, which already exists for
        exactly this purpose on the full-listing path) without being able
        to verify which of the two documented behaviors actually happens
        risks shipping an untested code path for a scenario that might not
        even arise. Current, honest behavior if GraphQL *does* follow a
        redirect for an explicitly tracked repo: `BuildExplicitTasks`
        decides against the old (locally present) name's directory and
        state entry but builds its `Task` with gh's returned (new) name,
        so `processTask` would clone into a second, new-named directory
        rather than recognizing the rename -- the same "looks missing,
        clone fresh" outcome the plan describes for the *non-redirecting*
        case. This needs revisiting once gh's actual behavior here is
        confirmed.
  - [x] The "in state but not in listing" note's exclusion for explicitly
        tracked repos the owner's own filter excludes (forks, specifically):
        **found missing during this step's own review-before-review pass,
        not carried over from anywhere** -- the workspace-wide path's
        configured-owner loop initially had no such note at all. Added it
        (mirroring `runSingleOwnerSync`'s existing one), gated so it never
        fires for a name still pending in that owner's `remaining` batch
        set (which gets a real decision via `BuildExplicitTasks` instead).
        `TestRunSyncWorkspaceInStateNotInListingNote` pins this exactly: a
        tracked fork gets cloned via the explicit path with no spurious
        note, while an untracked, no-longer-listed repo still gets the
        plain note. Regression-reproduced (removing the `remaining`
        exclusion makes the fork wrongly get both the note and a real
        clone).
  - [x] New test coverage beyond what's named above:
        `internal/engine`: `TestBuildExplicitTasksMissingUpstream`,
        `TestBuildExplicitTasksDecidesLikeAFullSync`.
        `internal/settings`: `TestApplyOwnerOverrides` (new
        `settings.ApplyOwnerOverrides`, applying an `OwnerConfig`'s
        `IncludeForks`/`Archive`/`MaxRepos` onto a copy of the global
        `Settings` -- used per owner in `runWorkspaceSync`).
        `internal/cli`: `TestRunSyncSingleRepoArg`,
        `TestRunSyncWorkspaceWideAcrossOwners` (the central claim: a
        configured owner's listing-based clone and a different owner's
        explicit-only batched clone both happen, each owner's state.json
        independently correct), `TestRunSyncHelpExitsZero`,
        `TestRunSyncTooManyPositionalArgsIsUsageError`,
        `TestRunSyncDryRunDoesNotLockOrWrite`,
        `TestRunSyncTrackedOnlyOnBrandNewOwnerIsANoOp` (an owner with
        nothing configured and nothing local is a clean no-op, not an
        error).
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package
        and all nine `internal/` packages. Manually smoke-tested the real
        binary's `sync --help`, top-level `--help` (both now list `sync`),
        and a real `gh`/`git`-shaped (if unauthenticated) invocation of
        `sync --config ... --dry-run` showing the exact `gh api graphql`
        argv it would run.
  - [x] **Independent review found one real bug, fixed before this step was
        marked done**: a failed cross-owner `ghcli.ViewRepos` batch call
        was only recorded as that owner's `planErr` when the owner had
        *zero* tasks of its own -- so a configured owner with both
        listing-derived tasks and explicitly tracked repos outside the
        listing would silently drop the latter on a batch failure, log one
        generic "error: batch repo lookup failed" line, and still exit `0`.
        Fixed by marking every plan index that contributed any entry to
        the failed batch (via the same `batch` slice already used to route
        successful results back), regardless of whether that plan also
        has listing-derived tasks. Pinned by
        `TestRunSyncWorkspaceBatchFailureFailsOwnerEvenWithListingTasks`,
        regression-reproduced in a scratch copy (reverting to the
        zero-tasks-only condition makes the test fail with `code=0`,
        exactly the bug the review found) before trusting it. Also removed
        `explicitByOwnerHasAny`, the helper the buggy condition used, now
        dead code.
- [x] jj: `jj new -m "feat(worktree): configured worktree placement"`.
  - [x] `settings.Settings` gained `WorktreeRoot`/`WorktreePath`.
        `WorktreeRoot`'s default is `""`, meaning "the current directory
        when the command runs" -- a dynamic default `settings.Default()`
        itself cannot express (it has no notion of "the directory the
        command happens to run in"), resolved instead by
        `resolveWorktreePath` (new, `internal/cli/worktreepath.go`) only
        when actually needed. `WorktreePath`'s default is the plain static
        string `"{repo}"`, so it lives in `Default()` like every other
        setting. `settings.Validate` requires `WorktreeRoot` to be
        absolute only when it's actually set (`""` is always valid) and
        requires `WorktreePath` to be non-empty. Both flags/env/config
        keys (`--worktree-root`/`--worktree-path`,
        `GH_ORG_CLONE_WORKTREE_ROOT`/`GH_ORG_CLONE_WORKTREE_PATH`,
        `worktreeRoot`/`worktreePath`) are scoped to a **new, narrower**
        `settings.CmdWorktreeAdd` commandID, not the existing shared
        `CmdWorktree` (which `worktree remove`/`worktree list` also use):
        remove/list have no use for a default-path template at all, so
        giving them these two flags anyway (which sharing `CmdWorktree`
        would have done) would have been actively misleading. `root`/
        `timeout`/`protocol` are still shared across all three via
        `inCmd(CmdSync, CmdWorktree, CmdClone, CmdWorktreeAdd)`.
        `cmdWorktreeAdd` (the dispatch function) now resolves against
        `cmdIDWorktreeAdd` (not `cmdWorktree`) -- named with the same
        `cmdID`-prefix workaround as `cmdIDClone`, to avoid colliding with
        its own dispatch function's name.
  - [x] Template expansion (`expandWorktreePathTemplate`): `{owner}`,
        `{repo}`, `{branch}` substituted by plain string replacement.
        `{branch}` has every `/` replaced with `-` *before* substitution
        (`resolveWorktreePath`), so a branch like `feat/x` can't create
        extra path levels a template didn't ask for. The joined, cleaned
        result is checked to stay under `worktreeRoot` (`strings.HasPrefix`
        after `filepath.Clean`/`filepath.Join`); an escaping template
        (e.g. `../{repo}`) is a hard error, never silently clamped.
  - [x] `path` is now optional on `worktree add` (`<org>/<repo> <branch>
        [path]`, 2 or 3 positionals instead of exactly 3) -- a genuine,
        deliberate user-facing change, reflected in the top-level
        `--help` USAGE block and every pinned test that printed the old
        `<path>`-required usage string. An explicit `path` argument still
        resolves exactly as before (`absWorktreePath`, against the
        caller's cwd, no under-`worktreeRoot` check at all) -- "the user
        always specifies where a worktree lives" (AGENTS.md): where the
        command runs, a configured template, or an explicit path are all
        the user choosing. A *computed* path that already exists gets a
        dedicated error naming the fix (an explicit path, or a
        `--worktree-path` template that includes `{branch}`) instead of
        git's own generic failure text; an *explicit* path hitting the
        same git error is left exactly as before -- never a generated
        alternative name, never a silent rename.
  - [x] Defaults confirmed end to end, not just at the unit level: current
        directory + `{repo}`, no owner segment
        (`TestWorktreeAddDefaultsToCurrentDirectoryAndRepoName`,
        `TestResolveWorktreePathDefaultsToCurrentDirectoryAndRepoName`),
        plus a **manual smoke test of the real binary** (fake `gh` on
        `PATH`, real `git`) confirming the exact same default placement
        outside of the test harness entirely.
        Second branch, same computed path, explicit-path/`{branch}` hint,
        no renaming
        (`TestWorktreeAddSecondBranchSamePathFailsWithHint`).
        Configured template with `{branch}` and a `/` in the branch name
        (`TestWorktreeAddConfiguredTemplateWithBranchSlash`,
        `TestResolveWorktreePathBranchSlashSanitized`).
        A relative `--worktree-root` is rejected
        (`TestWorktreeAddRejectsRelativeWorktreeRoot`); and an explicit
        path outside `worktreeRoot` is allowed
        (`TestWorktreeAddExplicitPathOutsideWorktreeRootAllowed`).
        An escaping template is rejected
        (`TestResolveWorktreePathRejectsEscapingTemplate`).
        A configured template with `{owner}` works too
        (`TestResolveWorktreePathConfiguredTemplateWithOwner`).
  - [x] `worktree remove <path>` (path-only form, new
        `resolveWorktreeOwnerRepo`): follows the worktree's own `.git`
        file (a plain text file naming
        `<gitdir>: <central-clone>/.git/worktrees/<name>` -- not a
        directory, the way a repository's own clone's `.git` is) back to
        the central clone, and recovers owner/repo by checking that
        directory sits at `<root>/<owner>/repos/<repo>`.
        **Found and fixed a real cross-platform bug while testing this**:
        on macOS, `t.TempDir()` (and any real invocation under `/tmp` or
        `/var`) returns an unresolved symlink path (`/var/folders/...`),
        but git resolves symlinks when it writes a worktree's gitdir
        (`/private/var/folders/...`), so comparing `cfg.Root` against the
        gitdir-derived path verbatim spuriously rejected every real
        worktree. Fixed by resolving `cfg.Root` through
        `filepath.EvalSymlinks` before comparing (falling back to the
        unresolved value if that fails, e.g. the root doesn't exist yet,
        rather than erroring out of what should be a plain lookup) --
        this is not a test-only workaround; the same mismatch would occur
        for any real root under a symlinked ancestor directory, which is
        the normal case on macOS. Caught immediately by
        `TestWorktreeRemovePathOnlyFormFindsOwningRepo` failing before the
        fix and passing after.
        Also added `TestWorktreeRemovePathOnlyRejectsNonWorktreePath` and
        `TestWorktreeRemovePathOnlyRejectsRepoCloneItself` (the central
        clone itself, whose `.git` is a directory, not a worktree's `.git`
        file).
  - [x] Workspace-wide `worktree list` (no positional at all): enumerates
        every directory under `cfg.Root` and lists each one's worktrees,
        with each repo's header qualified `<owner>/<repo>:` instead of the
        existing (unchanged) `<repo>:` the single-owner form still prints
        -- avoids ambiguity across owners without changing either
        existing form's output. An owner directory with no `repos/` at
        all (nothing ever cloned under it) is not an error for this form
        specifically, since it's a legitimate state the whole-workspace
        enumeration can run into on its own, unlike `worktree list <org>`
        naming a real, specific owner.
        `TestWorktreeListWorkspaceWideNoArg`,
        `TestWorktreeListWorkspaceWideSkipsOwnersWithNothingCloned`.
  - [x] No-op cost test:
        `TestRunSyncWorkspaceNoOpCostsExactlyOneGhCallPerOwnerOrBatch` --
        two configured owners plus one explicit repo outside them, run
        twice; the second (no-op) run must cost exactly 3 `gh` calls (one
        listing per owner, one batched lookup for the explicit repo) and
        zero `git` calls. Regression-reproduced in a scratch copy (forcing
        `BuildTasks`' `plan.Decide` call to `Force: true` made the test
        fail with 8 real `git` calls instead of 0) before trusting it --
        and in the process of writing that reproduction, first
        mis-identified `SyncOne`'s own `Force` parameter as the one to
        break (same literal text, different function, three occurrences
        in the file); corrected once the test kept passing against the
        wrong edit, which was itself a useful (if accidental) confirmation
        that `SyncOne` and `BuildTasks` are genuinely independent code
        paths, not aliases of each other.
  - [x] **Independent review found one real (if minor) issue, fixed before
        this step was marked done**: the "path already exists" hint
        message always suggested a `--worktree-path` template that
        includes `{branch}` -- but a template that *already* includes
        `{branch}` can still collide, because `/` in a branch name is
        replaced with `-` before substitution (`feat/x` and `feat-x`
        sanitize to the same string). The old message was actively
        misleading in exactly that case. Reworded to explain the real
        cause (the slash-to-dash sanitization) instead of repeating advice
        that wouldn't have helped. Not a correctness bug -- reviewer
        confirmed the collision fails safely with no overwrite/corruption,
        no worse than git's own pre-existing "path already exists"
        handling -- just an inaccurate error message.
        `go build`/`vet`/`gofmt`/`test -race` clean on the root package
        and all nine `internal/` packages.

## Phase 4 — Rename the surface

- [x] jj: `jj new -m "feat!: rename gh-org-clone to gh-workspace"`.
- [x] `go.mod` module path and imports. `go mod tidy` run after.
- [x] Env prefix `GH_ORG_CLONE_*` → `GH_WORKSPACE_*` (settings table, one
      place now). No fallback, per Phase 0.
- [x] Default data directory `…/gh-org-clone` → `…/gh-workspace`, and config
      path `~/.config/gh-workspace/config.json`.
- [x] Every user-facing `gh org-clone` / `gh-org-clone` string: grepped for
      them (the lock error, "requires gh/git on PATH" messages, usage text,
      `--help` output) and renamed mechanically across every `.go` file,
      `README.md`, and `go.mod`. **Scope decision, asked and answered
      explicitly rather than assumed**: the *tool's own name* renamed
      (`gh-org-clone`→`gh-workspace`, `gh org-clone`→`gh workspace` in
      invocation syntax), but the unrelated `<org>` placeholder/"org" wording
      in flags, error text, and usage (e.g. `"org must not be empty"`,
      `<org>/<repo>`) was deliberately **left alone** — a separate
      terminology question from the tool's name, decided not to pursue now.
      `go build`/`vet`/`gofmt`/`test -race` clean on every package
      afterward, including the full `TestPin*` suite (whose golden
      `--help` text now says `gh workspace`), with zero assertion changes
      needed beyond what the rename itself touched (the env-var list and
      literal usage strings the tests already pinned).
- [x] Renamed the GitHub repo to `swanysimon/gh-workspace` (gh extensions
      must be named `gh-<name>`). Done by the user directly (`gh repo
      rename`), not by me — this session's auto-mode permission
      classifier blocks external GitHub writes like this one, so I asked
      and the user ran it themselves. Confirmed via `gh repo view
      swanysimon/gh-workspace`. The local `origin` remote's URL was
      updated to match (`git remote set-url`); GitHub redirects the old
      URL regardless, so nothing else depends on the old name.
- [x] README: rewritten for the workspace model — install, what's tracked,
      `sync`/`clone`/`untrack`/`worktree` usage (including worktree
      placement defaults and the template syntax), the full settings table
      (including the two new worktree flags), an `owners`/`repos` config
      example, file layout, and an added note on per-owner (not
      workspace-wide) locking. Not literally generated from the settings
      table (judged not cheap enough to automate for one doc, given the
      table mixes flags with no env/config key at all) — written by hand
      against the table's actual current contents instead, and the
      command examples verified against the real `--help` output of each
      command (top-level, `sync`, `clone`, `untrack`, `worktree
      add`/`remove`/`list`) rather than assumed.
  - **Independent review found one real bug this doc rewrite exposed,
    fixed before this step was marked done**: README.md and AGENTS.md both
    stated, as current behavior, that `untrack` refuses when the config's
    `owners`/`repos` still track the repo — but that check had never
    actually been implemented. `cmdUntrack` (`internal/cli/clone.go`) still
    carried the exact `AIDEV:` comment from the "clone and untrack
    commands" step (Phase 3) saying this was deliberately deferred because
    the config's `owners`/`repos` shape didn't exist yet — except it has,
    since the very next Phase 3 step shipped it, and nobody circled back.
    The reviewer caught this by testing the real binary against the docs'
    own claim, not just reading code. Fixed: new `configTracks(fc, owner,
    repoName) (reason string, tracked bool)` in `clone.go`, checked in
    `cmdUntrack` before it clears `Tracked`, naming either the matching
    `owners` entry or the matching `repos` entry in the refusal message.
    Needed `resolveSubcommandConfig` to also return the loaded
    `*fileConfig` (it previously discarded it) — the same kind of small,
    mechanical signature change `resolveConfig` already went through in
    the sync-feature step for the same reason; all five other call sites
    (worktree add/remove/list, clone, plus one test) updated to discard
    the new return value with `_`.
    `TestCmdUntrackRefusesWhenConfigTracksRepo` (two subcases: tracked via
    `owners`, tracked via `repos`) pins it, and also asserts state is left
    untouched on refusal. Verified the test actually catches the
    regression by disabling the new check in a scratch copy and watching
    it fail before trusting it. `go build`/`vet`/`gofmt`/
    `test -race -count=1` clean on every package afterward.
  - Also fixed, found by the same review: `.gitignore` still ignored
    `/gh-org-clone` instead of `/gh-workspace` (missed by the rename sed,
    which only touched `.go`/`README.md`/`go.mod`).
- [x] AGENTS.md: rewritten for the workspace model (tracking rule, all six
      commands, the no-op-cost/never-delete/dirty-tree/worktree-placement/
      archive-prompt/per-owner-lock invariants) in the same voice as the
      original (short, declarative, not a copy of AIDEV.md's much longer
      design history — pointed at it for that instead). Kept the jj-first
      process rule verbatim, as asked.
- [x] No MIGRATING.md (clean break) — nothing to write, by design. Manual
      steps for my own machine, checked directly rather than assumed:
      `gh extension list` shows **no extensions installed at all** on this
      machine (empty list, exit 0), and neither `~/.local/share/gh-org-clone`
      nor `~/.config/gh-org-clone` exist, nor does any `GH_ORG_CLONE_*`
      export appear in `~/.zshrc`/`~/.zprofile`/`~/.bashrc`/
      `~/.bash_profile`/`~/.profile`/`~/.config/fish/config.fish`. This
      machine has never actually run the extension — there is nothing to
      remove, move, or rename. Nothing done here beyond confirming that.

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

## Open questions (resolved)

- [x] `archive` is **never** a manual verb. Archiving only happens as a
      consequence of syncing a repo that's archived upstream; there is no
      command to force-archive a repo locally. No code change needed (no
      such command exists today) — this just closes the question.
- [x] Worktree metadata stays **git-only**: no extra state tracking.
      `status` isn't built yet (it's still the optional, later command in
      the table above), so there's nothing concrete to speed up — adding
      state ahead of that need would be an unrequested abstraction.
      Revisit only once `status` exists and is shown to need it.
- [x] jj-colocated worktrees (`jj workspace add` support) stay **out of
      scope**. Real use case (the maintainer uses jj), but still only one
      consumer — no interface seam added to `gitcli`'s worktree helpers
      ahead of that work actually starting (YAGNI; an unused interface is
      exactly the kind of unrequested abstraction the gates rule out). Add
      the seam when this is actually built, not before.
- [x] Named workspaces (`--workspace work`) stay **out of scope**.
      `--config`/`--root` already cover switching between configs; nothing
      in this plan needs more than that.
