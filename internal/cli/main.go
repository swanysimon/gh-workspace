// Package cli is gh-org-clone's command dispatch, flag parsing, and help
// text: the only package that knows command names (sync's bare <org> form,
// "worktree add/remove/list"), and the only one that resolves this tool's
// settings struct (config, wrapping settings.Settings plus Owner and Deps)
// from flags/env/a config file. Everything below it in the dependency
// graph (execx, ghcli, gitcli, store, plan, archive, settings, engine)
// takes plain arguments and has no notion of a "command"; this package is
// where those plain pieces get wired into the thing a user actually runs.
// Run is its only exported name -- the root package's main() calls it with
// os.Args[1:]/os.Stdout/os.Stderr and nothing else, so no other package
// needs to know any of this package's internal names.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"github.com/swanysimon/gh-org-clone/internal/engine"
	"github.com/swanysimon/gh-org-clone/internal/settings"
)

const (
	exitSuccess     = 0
	exitRuntimeFail = 1
	exitUsage       = 2
	exitInterrupted = 130
)

var ownerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

type config struct {
	settings.Settings
	Owner string
	Deps  deps // subprocess + confirm-prompt seams; see deps.go
}

// fileConfig is an alias for settings.FileConfig, which now holds the real
// implementation (settings.go's old content, moved verbatim).
type fileConfig = settings.FileConfig

// errHelpRequested is resolveConfig/resolveWorktreeConfig's sentinel for
// "-h/--help was passed", replacing flag.ErrHelp: pflag.FlagSet has no
// built-in help handling (that's cobra's job), so -h/--help is a plain bool
// flag we register and check ourselves on every FlagSet.
var errHelpRequested = errors.New("help requested")

// Run is gh-org-clone's single entry point, called by the root package's
// main() with os.Args[1:]/os.Stdout/os.Stderr. It is the only exported name
// in this package: everything else -- command dispatch, config resolution,
// help text -- is this package's own business, and nothing outside it
// (including the root main.go, which can only call Run) needs to know any
// of their names.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "worktree" {
		return runWorktree(ctx, args[1:], stdout, stderr)
	}

	cfg, err := resolveConfig(args, stderr)
	if errors.Is(err, errHelpRequested) {
		return exitSuccess
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}

	if _, err := exec.LookPath("gh"); err != nil {
		fmt.Fprintln(stderr, "gh-org-clone requires the gh CLI on PATH:", err)
		return exitRuntimeFail
	}
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(stderr, "gh-org-clone requires git on PATH:", err)
		return exitRuntimeFail
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A dry run only reads: no directories, no lock file, no temp-clone
	// sweep. Without the lock it may observe a concurrent run's in-progress
	// state, which is acceptable for a report that changes nothing.
	if !cfg.DryRun {
		if err := os.MkdirAll(reposDir(cfg), 0o700); err != nil {
			fmt.Fprintln(stderr, err)
			return exitRuntimeFail
		}
		if err := os.MkdirAll(archivesDir(cfg), 0o700); err != nil {
			fmt.Fprintln(stderr, err)
			return exitRuntimeFail
		}

		release, err := acquireLock(cfg)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRuntimeFail
		}
		defer release()

		sweepTempClones(cfg)
	}

	repos, err := listRepos(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}

	st := loadState(statePath(cfg), cfg.Owner, stderr)

	// gh's --limit is a hard cap with no "there was more" signal, so a
	// listing that exactly fills it is the only hint of truncation.
	if len(repos) >= cfg.MaxRepos {
		fmt.Fprintf(stderr, "warning: gh repo list returned %d repos, the --max-repos limit; the listing may be truncated, raise --max-repos to be sure\n", len(repos))
	}

	tasks, seen, prepassFailed := buildTasks(ctx, cfg, repos, st, stderr)

	// AIDEV: absence from the listing is indistinguishable from lost access
	// to a private repo, so we only ever report it and never delete local
	// data; upgrade path is an opt-in -prune flag.
	for name := range st.Repos {
		if !seen[name] {
			fmt.Fprintf(stderr, "note: %q is in local state but was not returned by gh repo list; leaving any local data untouched\n", name)
		}
	}

	if cfg.DryRun {
		for _, t := range tasks {
			fmt.Fprintf(stdout, "%s: %s (%s)\n", t.Repo.Name, t.Action, t.Reason)
		}
		if prepassFailed > 0 {
			return exitRuntimeFail
		}
		return exitSuccess
	}

	actionable := 0
	for _, t := range tasks {
		if t.Action != actionSkip {
			actionable++
		}
	}
	fmt.Fprintf(stderr, "syncing %d repos (%d to clone/fetch/archive, %d unchanged)\n", len(tasks), actionable, len(tasks)-actionable)

	results := runTasks(ctx, cfg, tasks, stderr)

	reposByName := make(map[string]ghRepo, len(repos))
	for _, r := range repos {
		reposByName[r.Name] = r
	}

	var cloned, fetched, archived, skipped int
	failed := prepassFailed
	for _, res := range results {
		for _, n := range res.Notes {
			fmt.Fprintf(stderr, "warning: %s: %s\n", res.Name, n)
		}
		if res.Err != nil {
			fmt.Fprintf(stderr, "error: %s: %v\n", res.Name, res.Err)
			failed++
			continue
		}

		switch res.Action {
		case actionSkip:
			skipped++
		case actionClone, actionUnarchive:
			cloned++
		case actionFetch:
			fetched++
		case actionArchive, actionAdoptArchived:
			archived++
		}

		id := st.Repos[res.Name].ID
		if r, ok := reposByName[res.Name]; ok {
			id = r.ID
		}
		st.Repos[res.Name] = repoState{
			ID:          id,
			PushedAt:    res.PushedAt,
			SyncedAt:    time.Now(),
			Status:      res.Status,
			ArchivePath: res.ArchivePath,
		}
	}
	st.UpdatedAt = time.Now()

	if err := saveState(statePath(cfg), st); err != nil {
		fmt.Fprintln(stderr, "error saving state:", err)
		failed++
	}

	fmt.Fprintf(stdout, "cloned=%d fetched=%d archived=%d skipped=%d failed=%d\n", cloned, fetched, archived, skipped, failed)

	if ctx.Err() != nil {
		return exitInterrupted
	}
	if failed > 0 {
		return exitRuntimeFail
	}
	return exitSuccess
}

// buildEnv adapts cfg into the plain engine.Env every engine function
// takes, so engine has no dependency on this package's config/ghRepo
// types.
func buildEnv(cfg config) engine.Env {
	return engine.Env{
		Exec: cfg.Deps.exec,
		Confirm: func(repoName string, worktrees []worktreeStatus) (bool, error) {
			return cfg.Deps.confirm(cfg, repoName, worktrees)
		},
		Owner:    cfg.Owner,
		Settings: cfg.Settings,
	}
}

// sweepTempClones is a thin shim over engine.SweepTempClones.
func sweepTempClones(cfg config) {
	engine.SweepTempClones(buildEnv(cfg))
}

// task, result are aliases for engine.Task/engine.Result, so every existing
// t.Action/res.Name/etc. access elsewhere keeps compiling. task's fields
// are capitalized (Repo/Action/Reason/Prev) because engine.Task's are --
// unlike the old lowercase task{repo,action,reason,prev}, this is a visible
// (if small) call-site change; see AIDEV.md.
type task = engine.Task
type result = engine.Result

// buildTasks is a thin shim over engine.BuildTasks; see there for the full
// behavior.
func buildTasks(ctx context.Context, cfg config, repos []ghRepo, st state, stderr io.Writer) (tasks []task, seen map[string]bool, failed int) {
	return engine.BuildTasks(ctx, buildEnv(cfg), repos, st, stderr)
}

// runTasks is a thin shim over engine.RunTasks; see there for the full
// behavior.
func runTasks(ctx context.Context, cfg config, tasks []task, stderr io.Writer) []result {
	return engine.RunTasks(ctx, buildEnv(cfg), tasks, stderr)
}

// defaultRoot is a thin shim over settings.DefaultRoot.
func defaultRoot() string {
	return settings.DefaultRoot()
}

func defaultConfig() config {
	return config{
		Settings: settings.Default(),
		Deps:     defaultDeps(),
	}
}

// flagHelp mirrors gh's own --help formatting: long flags are always
// double-dash, a shorthand (if any) is single-dash and listed first, and a
// non-empty value type or default is appended the way gh's own flags show
// "(default 30)".
type flagHelp struct {
	long      string
	shorthand string
	valueHint string
	usage     string
	def       string
	quoteDef  bool // gh quotes string defaults but not numeric/duration ones
}

func usageFlags(d config) []flagHelp {
	return []flagHelp{
		{long: "root", valueHint: "string", usage: "root directory for cloned orgs", def: d.Root, quoteDef: true},
		{long: "concurrency", valueHint: "int", usage: "number of repos to sync in parallel", def: strconv.Itoa(d.Concurrency)},
		{long: "timeout", valueHint: "duration", usage: "per-subprocess timeout", def: d.Timeout.String()},
		{long: "max-repos", valueHint: "int", usage: "maximum repos to list from the org (gh --limit)", def: strconv.Itoa(d.MaxRepos)},
		{long: "protocol", valueHint: "string", usage: "clone protocol: ssh or https", def: d.Protocol, quoteDef: true},
		{long: "include-forks", usage: "include forked repos"},
		{long: "archive", usage: "tarball archived repos and remove their clones"},
		{long: "force", usage: "ignore stored pushedAt and re-sync every repo"},
		{long: "dry-run", usage: "print the planned actions without doing them"},
		{long: "verbose", shorthand: "v", usage: "verbose output"},
		{long: "yes", usage: "don't prompt before removing worktrees to archive a repo they belong to"},
		{long: "config", valueHint: "string", usage: "path to a JSON config file"},
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Clone and keep local mirrors of every repository in a GitHub org.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "USAGE")
	fmt.Fprintln(w, "  gh org-clone [flags] <org>")
	fmt.Fprintln(w, "  gh org-clone worktree add [flags] <org>/<repo> <branch> <path>")
	fmt.Fprintln(w, "  gh org-clone worktree remove [--force] [flags] <org>/<repo> <path>")
	fmt.Fprintln(w, "  gh org-clone worktree list [flags] <org>/<repo>|<org>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run \"gh org-clone worktree <command> --help\" for a worktree command's flags.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "FLAGS")
	printFlagTable(w, usageFlags(defaultConfig()))
}

// printFlagTable renders flags the way gh's own --help does: an optional
// "-x, " shorthand column, "--long type", then the description and default.
func printFlagTable(w io.Writer, entries []flagHelp) {
	left := make([]string, len(entries))
	width := 0
	for i, h := range entries {
		if h.shorthand != "" {
			left[i] = fmt.Sprintf("  -%s, --%s", h.shorthand, h.long)
		} else {
			left[i] = fmt.Sprintf("      --%s", h.long)
		}
		if h.valueHint != "" {
			left[i] += " " + h.valueHint
		}
		if len(left[i]) > width {
			width = len(left[i])
		}
	}
	for i, h := range entries {
		desc := h.usage
		switch {
		case h.def == "":
			// no default worth showing
		case h.quoteDef:
			desc += fmt.Sprintf(" (default %q)", h.def)
		default:
			desc += fmt.Sprintf(" (default %s)", h.def)
		}
		fmt.Fprintf(w, "%-*s   %s\n", width, left[i], desc)
	}
}

// flagHelpsFromSet describes the flags actually defined on fs, so a
// subcommand's help can't drift from what it accepts. fs's own defaults are
// zero values (real defaults are resolved later, after env and config), so
// the shown default and value type come from usageFlags when it knows the
// flag. The "help" flag every FlagSet carries (see newFlagSet) is
// deliberately excluded: it was never listed in the flags table before the
// pflag port, and it isn't now either.
func flagHelpsFromSet(fs *pflag.FlagSet, d config) []flagHelp {
	known := map[string]flagHelp{}
	for _, h := range usageFlags(d) {
		known[h.long] = h
	}
	var out []flagHelp
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" {
			return
		}
		h := flagHelp{long: f.Name, shorthand: f.Shorthand, valueHint: f.Value.Type(), usage: f.Usage}
		if k, ok := known[f.Name]; ok {
			h.valueHint, h.def, h.quoteDef = k.valueHint, k.def, k.quoteDef
		}
		out = append(out, h)
	})
	return out
}

// newFlagSet builds the pflag.FlagSet every resolveConfig/resolveWorktreeConfig
// caller starts from: output silenced (every error or usage message below is
// printed exactly once, by us, never by pflag itself), plus the "help" flag
// every command accepts. pflag has no built-in -h/--help handling -- that is
// cobra's job, and this codebase doesn't build a cobra.Command tree yet (see
// AIDEV.md) -- so it is registered like any other flag and checked
// explicitly after Parse.
func newFlagSet(name string) (*pflag.FlagSet, *bool) {
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	help := fs.BoolP("help", "h", false, "show help")
	return fs, help
}

// resolveConfig applies flags > env > file > defaults, for exactly the
// settings cmdSync declares in settingsTable.
//
// Single-dash long flags (e.g. "-root", accepted by the old stdlib-flag
// implementation purely as an accident of that package's leniency, contrary
// to this tool's own documented double-dash convention) are no longer
// accepted: pflag treats a single dash followed by more than one character
// as a cluster of shorthand flags, not a long flag. This is a deliberate,
// intentional difference -- see AIDEV.md.
func resolveConfig(args []string, stderr io.Writer) (config, error) {
	var configPath string
	fs, help := newFlagSet("gh-org-clone")
	bound := bindSettings(fs, cmdSync)
	fs.StringVar(&configPath, "config", "", "path to a JSON config file")

	// A raw parse failure (unknown flag, malformed value) and a bad
	// positional count both show the full usage block exactly once, then
	// (from Run(), after this returns) the error exactly once. The old
	// stdlib-flag implementation printed a parse failure's error text twice
	// -- once from its own internal failf, once from Run() -- and never
	// showed the usage block for a semantic validateConfig failure (e.g. a
	// bad --protocol value) at all. Both are deliberate fixes, not
	// preserved bugs -- see AIDEV.md.
	if err := fs.Parse(args); err != nil {
		printUsage(stderr)
		return config{}, err
	}
	if *help {
		printUsage(stderr)
		return config{}, errHelpRequested
	}

	positional := fs.Args()
	if len(positional) != 1 {
		printUsage(stderr)
		return config{}, fmt.Errorf("expected exactly one org argument, got %d", len(positional))
	}

	cfg := defaultConfig()

	fc, err := loadFileConfig(resolveConfigPath(configPath))
	if err != nil {
		return config{}, err
	}
	if err := resolveSettings(&cfg, cmdSync, fs, bound, fc); err != nil {
		return config{}, err
	}

	cfg.Owner = positional[0]

	if err := validateConfig(cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// resolveConfigPath is a thin shim over settings.ResolveConfigPath.
func resolveConfigPath(flagValue string) string {
	return settings.ResolveConfigPath(flagValue)
}

// loadFileConfig is a thin shim over settings.LoadFileConfig.
func loadFileConfig(path string) (*fileConfig, error) {
	return settings.LoadFileConfig(path)
}

func validateConfig(cfg config) error {
	if cfg.Owner == "" {
		return fmt.Errorf("org must not be empty")
	}
	if !ownerNamePattern.MatchString(cfg.Owner) {
		return fmt.Errorf("org %q is not a valid GitHub org name", cfg.Owner)
	}
	return settings.Validate(cfg.Settings)
}
