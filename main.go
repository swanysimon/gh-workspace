package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	exitSuccess     = 0
	exitRuntimeFail = 1
	exitUsage       = 2
	exitInterrupted = 130
)

var orgNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

type config struct {
	Org          string
	Root         string // org dir is <Root>/<Org>
	Concurrency  int
	Timeout      time.Duration // per subprocess
	MaxRepos     int           // gh --limit
	Protocol     string        // "ssh" | "https"
	IncludeForks bool
	Archive      bool
	Force        bool
	DryRun       bool
	Verbose      bool
	Yes          bool // skip the archive-with-live-worktrees confirmation prompt
	Deps         deps // subprocess + confirm-prompt seams; see deps.go
}

type fileConfig struct {
	Root         *string `json:"root"`
	Concurrency  *int    `json:"concurrency"`
	Timeout      *string `json:"timeout"` // parsed with time.ParseDuration
	MaxRepos     *int    `json:"maxRepos"`
	Protocol     *string `json:"protocol"`
	IncludeForks *bool   `json:"includeForks"`
	Archive      *bool   `json:"archive"`
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "worktree" {
		return runWorktree(ctx, args[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("gh-org-clone", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printUsage(stderr) }

	cfg, err := resolveConfig(fs, args, stderr)
	if errors.Is(err, flag.ErrHelp) {
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

	st := loadState(statePath(cfg), cfg.Org, stderr)

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
			fmt.Fprintf(stdout, "%s: %s (%s)\n", t.repo.Name, t.action, t.reason)
		}
		if prepassFailed > 0 {
			return exitRuntimeFail
		}
		return exitSuccess
	}

	actionable := 0
	for _, t := range tasks {
		if t.action != actionSkip {
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

// sweepTempClones removes leftover .tmp-* directories from a previous
// interrupted run. These are ours by construction and can never contain
// user data.
func sweepTempClones(cfg config) {
	entries, err := os.ReadDir(reposDir(cfg))
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			os.RemoveAll(filepath.Join(reposDir(cfg), e.Name()))
		}
	}
}

type task struct {
	repo   ghRepo
	action action
	reason string
	prev   repoState
}

// buildTasks is the sequential pre-pass: fork filtering, name validation,
// case-collision detection, ID-based rename fix-up, then decide() per repo.
// It runs single-threaded, before any worker goroutine starts. failed counts
// repos the pre-pass refused (invalid or colliding names); they are reported
// here and must fail the run, since they will never be synced.
func buildTasks(ctx context.Context, cfg config, repos []ghRepo, st state, stderr io.Writer) (tasks []task, seen map[string]bool, failed int) {
	seen = make(map[string]bool, len(repos))

	stateNameByID := map[string]string{}
	for name, rs := range st.Repos {
		if rs.ID != "" {
			stateNameByID[rs.ID] = name
		}
	}

	// APFS is case-insensitive: an org holding both Foo and foo maps to one
	// directory. Skip both sides rather than interleaving two repos into it.
	// Only repos that would otherwise be synced count: an excluded fork or
	// an invalid name never gets a directory to collide over.
	lowerNames := map[string][]string{}
	for _, r := range repos {
		if (r.IsFork && !cfg.IncludeForks) || !validRepoName(r.Name) {
			continue
		}
		lower := strings.ToLower(r.Name)
		lowerNames[lower] = append(lowerNames[lower], r.Name)
	}
	collided := map[string]bool{}
	for lower, names := range lowerNames {
		if len(names) > 1 {
			fmt.Fprintf(stderr, "error: repos %v collide on a case-insensitive filesystem (%q); skipping all of them\n", names, lower)
			for _, n := range names {
				collided[n] = true
			}
			failed += len(names)
		}
	}

	for _, repo := range repos {
		if repo.IsFork && !cfg.IncludeForks {
			if cfg.Verbose {
				fmt.Fprintf(stderr, "skipping fork %q\n", repo.Name)
			}
			continue
		}
		if !validRepoName(repo.Name) {
			fmt.Fprintf(stderr, "error: %q is not a valid repo name, skipping\n", repo.Name)
			failed++
			continue
		}
		if collided[repo.Name] {
			continue
		}
		seen[repo.Name] = true

		// In a dry run a pending rename is planned, not performed: the
		// decision is made against the old directory and state entry, as
		// if the rename had already happened.
		renamedFrom := ""
		if oldName, ok := stateNameByID[repo.ID]; ok && oldName != repo.Name {
			if cfg.DryRun {
				if renameApplies(cfg, oldName, repo.Name) {
					renamedFrom = oldName
				}
			} else {
				fixupRename(ctx, cfg, st, oldName, repo, stderr)
			}
		}

		prev, known := st.Repos[repo.Name]
		dir := filepath.Join(reposDir(cfg), repo.Name)
		if renamedFrom != "" {
			seen[renamedFrom] = true
			prev, known = st.Repos[renamedFrom]
			dir = filepath.Join(reposDir(cfg), renamedFrom)
		}
		_, dirErr := os.Stat(dir)
		dirExists := dirErr == nil
		_, gitErr := os.Stat(filepath.Join(dir, ".git"))
		isGitDir := gitErr == nil
		archiveExists := localArchiveExists(cfg, repo.Name)

		act, reason := decide(repo, prev, known, dirExists, isGitDir, archiveExists, cfg)
		if renamedFrom != "" {
			reason = fmt.Sprintf("rename from %q, then: %s", renamedFrom, reason)
		}
		tasks = append(tasks, task{repo: repo, action: act, reason: reason, prev: prev})
	}
	return tasks, seen, failed
}

// fixupRename moves a renamed repo's directory and state entry without an
// orphaned directory plus a full re-clone.
func fixupRename(ctx context.Context, cfg config, st state, oldName string, repo ghRepo, stderr io.Writer) {
	if !renameApplies(cfg, oldName, repo.Name) {
		return
	}
	oldDir := filepath.Join(reposDir(cfg), oldName)
	newDir := filepath.Join(reposDir(cfg), repo.Name)
	if err := os.Rename(oldDir, newDir); err != nil {
		fmt.Fprintf(stderr, "error: renaming %q to %q: %v\n", oldName, repo.Name, err)
		return
	}
	st.Repos[repo.Name] = st.Repos[oldName]
	delete(st.Repos, oldName)
	if url := cloneURL(repo, cfg); url != "" {
		if err := setRemoteURL(ctx, cfg, newDir, url); err != nil {
			fmt.Fprintf(stderr, "warning: could not update remote url for renamed repo %q: %v\n", repo.Name, err)
		}
	}
}

// renameApplies reports whether a renamed repo's old directory exists and
// its new one does not, i.e. whether fixupRename would move anything.
func renameApplies(cfg config, oldName, newName string) bool {
	if _, err := os.Stat(filepath.Join(reposDir(cfg), oldName)); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(reposDir(cfg), newName))
	return os.IsNotExist(err)
}

// result carries a worker's outcome. Errors travel in this struct, never
// out of a worker, so one repo's failure can never abort another's work.
type result struct {
	Name        string
	Action      action
	PushedAt    time.Time
	Status      repoStatus
	ArchivePath string
	Notes       []string
	Err         error
}

// progressReporter prints one line per non-skip task as a worker picks it
// up, so a long sync run shows activity instead of going silent until the
// final summary. It is safe for concurrent use by cfg.Concurrency workers.
type progressReporter struct {
	mu      sync.Mutex
	w       io.Writer
	total   int
	started int
}

func newProgressReporter(w io.Writer, total int) *progressReporter {
	return &progressReporter{w: w, total: total}
}

var actionVerbs = map[action]string{
	actionClone:         "cloning",
	actionFetch:         "fetching",
	actionArchive:       "archiving",
	actionAdoptArchived: "adopting existing archive",
	actionUnarchive:     "unarchiving (repo live again upstream)",
	actionNotARepo:      "checking",
}

func (p *progressReporter) starting(name string, act action) {
	if p == nil {
		return
	}
	verb, ok := actionVerbs[act]
	if !ok {
		verb = string(act)
	}
	p.mu.Lock()
	p.started++
	fmt.Fprintf(p.w, "[%d/%d] %s: %s\n", p.started, p.total, name, verb)
	p.mu.Unlock()
}

// runTasks fans work out to cfg.Concurrency workers and fans results back in
// through a single collector loop. State is mutated only by that loop, in
// run() — never here — so there is no mutex and no data race.
func runTasks(ctx context.Context, cfg config, tasks []task, stderr io.Writer) []result {
	actionable := 0
	for _, t := range tasks {
		if t.action != actionSkip {
			actionable++
		}
	}
	progress := newProgressReporter(stderr, actionable)

	taskCh := make(chan task)
	resultCh := make(chan result)

	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range taskCh {
				resultCh <- processTask(ctx, cfg, t, progress)
			}
		}()
	}

	go func() {
		for _, t := range tasks {
			taskCh <- t
		}
		close(taskCh)
	}()

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	results := make([]result, 0, len(tasks))
	for res := range resultCh {
		results = append(results, res)
	}
	return results
}

func processTask(ctx context.Context, cfg config, t task, progress *progressReporter) result {
	repo := t.repo
	res := result{Name: repo.Name, Action: t.action}

	if t.action != actionSkip {
		progress.starting(repo.Name, t.action)
	}

	switch t.action {
	case actionSkip:
		res.PushedAt = t.prev.PushedAt
		res.Status = t.prev.Status
		res.ArchivePath = t.prev.ArchivePath
		return res

	case actionClone, actionFetch:
		dir := filepath.Join(reposDir(cfg), repo.Name)
		if t.action == actionClone {
			if err := cloneRepo(ctx, cfg, repo); err != nil {
				res.Err = err
				return res
			}
		} else if err := fetchRepo(ctx, cfg, dir); err != nil {
			res.Err = err
			return res
		}

		defaultBranch := ""
		if repo.DefaultBranch != nil {
			defaultBranch = repo.DefaultBranch.Name
		}
		warn, dirty, err := updateWorktree(ctx, cfg, dir, defaultBranch)
		if err != nil {
			res.Err = err
			return res
		}
		if warn != "" {
			res.Notes = append(res.Notes, warn)
		}
		res.Status = statusCloned
		// AIDEV: a dirty repo never gets its pushedAt recorded, so the
		// warning repeats every run instead of silently serving a stale
		// tree forever; the cost is one wasted fetch per run.
		if !dirty {
			res.PushedAt = repo.PushedAt
		}
		return res

	case actionArchive, actionAdoptArchived:
		rs, notes, err := archiveRepo(ctx, cfg, repo)
		res.Notes = notes
		if err != nil {
			res.Err = err
			return res
		}
		res.PushedAt = rs.PushedAt
		res.Status = rs.Status
		res.ArchivePath = rs.ArchivePath
		return res

	case actionUnarchive:
		if err := cloneRepo(ctx, cfg, repo); err != nil {
			res.Err = err
			return res
		}
		res.Notes = append(res.Notes, fmt.Sprintf(
			"repo %q is live upstream again; stale archive at archives/%s.tar.gz and archives/%s.json left in place",
			repo.Name, repo.Name, repo.Name))
		res.Status = statusCloned
		res.PushedAt = repo.PushedAt
		return res

	case actionNotARepo:
		dir := filepath.Join(reposDir(cfg), repo.Name)
		res.Err = fmt.Errorf("%s exists but is not a git repository; left untouched", dir)
		return res
	}

	res.Err = fmt.Errorf("unknown action %q for repo %q", t.action, repo.Name)
	return res
}

func defaultRoot() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "gh-org-clone")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return filepath.Join(home, ".local", "share", "gh-org-clone")
}

func defaultConfig() config {
	return config{
		Root:         defaultRoot(),
		Concurrency:  8,
		Timeout:      30 * time.Minute,
		MaxRepos:     10000,
		Protocol:     "ssh",
		IncludeForks: false,
		Archive:      true,
		Deps:         defaultDeps(),
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
// flag.
func flagHelpsFromSet(fs *flag.FlagSet, d config) []flagHelp {
	known := map[string]flagHelp{}
	for _, h := range usageFlags(d) {
		known[h.long] = h
	}
	var out []flagHelp
	fs.VisitAll(func(f *flag.Flag) {
		hint, usage := flag.UnquoteUsage(f)
		h := flagHelp{long: f.Name, valueHint: hint, usage: usage}
		if k, ok := known[f.Name]; ok {
			h.valueHint, h.def, h.quoteDef = k.valueHint, k.def, k.quoteDef
		}
		out = append(out, h)
	})
	return out
}

// resolveConfig applies flags > env > file > defaults. fs.Visit reports only
// flags the caller actually typed, so an unset flag never clobbers a value
// already set by the env or the config file.
func resolveConfig(fs *flag.FlagSet, args []string, stderr io.Writer) (config, error) {
	var (
		root         string
		concurrency  int
		timeoutStr   string
		maxRepos     int
		protocol     string
		includeForks bool
		archive      bool
		force        bool
		dryRun       bool
		verbose      bool
		yes          bool
		configPath   string
	)
	fs.StringVar(&root, "root", "", "root directory for cloned orgs")
	fs.IntVar(&concurrency, "concurrency", 0, "number of repos to sync in parallel")
	fs.StringVar(&timeoutStr, "timeout", "", "per-subprocess timeout")
	fs.IntVar(&maxRepos, "max-repos", 0, "maximum repos to list from the org (gh --limit)")
	fs.StringVar(&protocol, "protocol", "", "clone protocol: ssh or https")
	fs.BoolVar(&includeForks, "include-forks", false, "include forked repos")
	fs.BoolVar(&archive, "archive", false, "tarball archived repos and remove their clones")
	fs.BoolVar(&force, "force", false, "ignore stored pushedAt and re-sync every repo")
	fs.BoolVar(&dryRun, "dry-run", false, "print the planned actions without doing them")
	fs.BoolVar(&verbose, "v", false, "verbose output")
	fs.BoolVar(&verbose, "verbose", false, "verbose output")
	fs.BoolVar(&yes, "yes", false, "don't prompt before removing worktrees to archive a repo they belong to")
	fs.StringVar(&configPath, "config", "", "path to a JSON config file")

	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return config{}, err
	}
	if len(positional) != 1 {
		fs.Usage()
		return config{}, fmt.Errorf("expected exactly one org argument, got %d", len(positional))
	}

	cfg := defaultConfig()

	fc, err := loadFileConfig(resolveConfigPath(configPath))
	if err != nil {
		return config{}, err
	}
	if fc != nil {
		if fc.Root != nil {
			cfg.Root = *fc.Root
		}
		if fc.Concurrency != nil {
			cfg.Concurrency = *fc.Concurrency
		}
		if fc.Timeout != nil {
			d, err := time.ParseDuration(*fc.Timeout)
			if err != nil {
				return config{}, fmt.Errorf("config file: invalid timeout %q: %w", *fc.Timeout, err)
			}
			cfg.Timeout = d
		}
		if fc.MaxRepos != nil {
			cfg.MaxRepos = *fc.MaxRepos
		}
		if fc.Protocol != nil {
			cfg.Protocol = *fc.Protocol
		}
		if fc.IncludeForks != nil {
			cfg.IncludeForks = *fc.IncludeForks
		}
		if fc.Archive != nil {
			cfg.Archive = *fc.Archive
		}
	}

	if err := overlayEnv(&cfg); err != nil {
		return config{}, err
	}

	var flagErr error
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "root":
			cfg.Root = root
		case "concurrency":
			cfg.Concurrency = concurrency
		case "timeout":
			d, err := time.ParseDuration(timeoutStr)
			if err != nil {
				flagErr = fmt.Errorf("--timeout: invalid duration %q: %w", timeoutStr, err)
				return
			}
			cfg.Timeout = d
		case "max-repos":
			cfg.MaxRepos = maxRepos
		case "protocol":
			cfg.Protocol = protocol
		case "include-forks":
			cfg.IncludeForks = includeForks
		case "archive":
			cfg.Archive = archive
		}
	})
	if flagErr != nil {
		return config{}, flagErr
	}

	cfg.Org = positional[0]
	cfg.Force = force
	cfg.DryRun = dryRun
	cfg.Verbose = verbose
	cfg.Yes = yes

	if err := validateConfig(cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// parseInterspersed parses flags wherever they appear among the positional
// args, as gh (and cobra) do, rather than stopping at the first positional
// arg like the standard flag package. A "--" still ends flag parsing, so a
// positional arg that starts with "-" can be passed after it.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func resolveConfigPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("GH_ORG_CLONE_CONFIG"); env != "" {
		return env
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "gh-org-clone", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return filepath.Join(home, ".config", "gh-org-clone", "config.json")
}

// loadFileConfig returns nil, nil when the file does not exist. Any other
// read or parse failure is a hard error — never guess at config intent.
func loadFileConfig(path string) (*fileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	defer f.Close()

	var fc fileConfig
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	return &fc, nil
}

// envSettings maps each GH_ORG_CLONE_* variable to how it applies to a
// config. overlayEnv applies them in this order.
var envSettings = []struct {
	name  string
	apply func(cfg *config, v string) error
}{
	{"GH_ORG_CLONE_ROOT", func(cfg *config, v string) error { cfg.Root = v; return nil }},
	{"GH_ORG_CLONE_CONCURRENCY", func(cfg *config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid integer %q: %w", v, err)
		}
		cfg.Concurrency = n
		return nil
	}},
	{"GH_ORG_CLONE_TIMEOUT", func(cfg *config, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", v, err)
		}
		cfg.Timeout = d
		return nil
	}},
	{"GH_ORG_CLONE_MAX_REPOS", func(cfg *config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid integer %q: %w", v, err)
		}
		cfg.MaxRepos = n
		return nil
	}},
	{"GH_ORG_CLONE_PROTOCOL", func(cfg *config, v string) error { cfg.Protocol = v; return nil }},
	{"GH_ORG_CLONE_INCLUDE_FORKS", func(cfg *config, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid bool %q: %w", v, err)
		}
		cfg.IncludeForks = b
		return nil
	}},
	{"GH_ORG_CLONE_ARCHIVE", func(cfg *config, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid bool %q: %w", v, err)
		}
		cfg.Archive = b
		return nil
	}},
}

// overlayEnv applies the named GH_ORG_CLONE_* variables (all of them if
// none are named). Commands pass only the settings they accept, so a bad
// value in a variable a command ignores can't make it fail.
func overlayEnv(cfg *config, only ...string) error {
	for _, e := range envSettings {
		if len(only) > 0 && !slices.Contains(only, e.name) {
			continue
		}
		v := os.Getenv(e.name)
		if v == "" {
			continue
		}
		if err := e.apply(cfg, v); err != nil {
			return fmt.Errorf("%s: %w", e.name, err)
		}
	}
	return nil
}

func validateConfig(cfg config) error {
	if cfg.Org == "" {
		return fmt.Errorf("org must not be empty")
	}
	if !orgNamePattern.MatchString(cfg.Org) {
		return fmt.Errorf("org %q is not a valid GitHub org name", cfg.Org)
	}
	if cfg.Concurrency < 1 {
		return fmt.Errorf("concurrency must be >= 1, got %d", cfg.Concurrency)
	}
	if cfg.MaxRepos < 1 {
		return fmt.Errorf("max-repos must be >= 1, got %d", cfg.MaxRepos)
	}
	if cfg.Timeout <= 0 {
		return fmt.Errorf("timeout must be > 0, got %s", cfg.Timeout)
	}
	if cfg.Protocol != "ssh" && cfg.Protocol != "https" {
		return fmt.Errorf("protocol must be ssh or https, got %q", cfg.Protocol)
	}
	if !filepath.IsAbs(cfg.Root) {
		return fmt.Errorf("root must be an absolute path, got %q", cfg.Root)
	}
	return nil
}
