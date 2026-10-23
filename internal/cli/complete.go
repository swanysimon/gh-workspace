package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/swanysimon/gh-workspace/internal/gitcli"
	"github.com/swanysimon/gh-workspace/internal/settings"
	"github.com/swanysimon/gh-workspace/internal/store"
	"github.com/swanysimon/gh-workspace/internal/vcs"
)

// Cobra's ShellCompDirective values. This tool has no cobra command tree,
// but the completion scripts hand __complete's output to gh's own
// cobra-generated completer, which parses exactly these.
const (
	compDefault    = 0
	compError      = 1
	compNoSpace    = 2
	compNoFileComp = 4
	compFilterDirs = 16
)

// completionTimeout bounds each subprocess __complete runs: a Tab that
// hangs is worse than one that offers nothing.
const completionTimeout = 2 * time.Second

// cmdComplete answers "gh workspace __complete <word>... <partial>" in
// cobra's protocol: one candidate per line, then ":<directive>". It
// always exits 0 and never writes to stderr, since anything it prints
// lands under the user's cursor mid-Tab.
func cmdComplete(ctx context.Context, args []string, stdout io.Writer) int {
	candidates, directive := complete(ctx, args)
	for _, c := range candidates {
		fmt.Fprintln(stdout, c)
	}
	fmt.Fprintf(stdout, ":%d\n", directive)
	return exitSuccess
}

func complete(ctx context.Context, args []string) ([]string, int) {
	if len(args) == 0 {
		return nil, compNoFileComp
	}
	words, partial := args[:len(args)-1], args[len(args)-1]
	if len(words) == 0 {
		return withPrefix([]string{"sync", "clone", "untrack", "worktree", "completion"}, partial), compNoFileComp
	}

	var spec completionSpec
	switch words[0] {
	case "worktree":
		if len(words) == 1 {
			return withPrefix([]string{"add", "remove", "list"}, partial), compNoFileComp
		}
		var ok bool
		if spec, ok = worktreeCompletionSpecs[words[1]]; !ok {
			return nil, compNoFileComp
		}
		words = words[2:]
	case "completion":
		if len(words) == 1 {
			return withPrefix(completionShells, partial), compNoFileComp
		}
		return nil, compNoFileComp
	default:
		var ok bool
		if spec, ok = completionSpecs[words[0]]; !ok {
			return nil, compNoFileComp
		}
		words = words[1:]
	}
	return spec.complete(ctx, words, partial)
}

// completionSpec is how one command's arguments complete: the same flag
// set and config resolution the command itself uses (so --root, --config
// and env vars already on the line are honored), and a completer per
// positional argument.
type completionSpec struct {
	resolve    func(fs *pflag.FlagSet, help *bool, args []string) (config, *fileConfig, []string, error)
	flags      func(fs *pflag.FlagSet)
	positional func(ctx context.Context, c completer, args []string, partial string) ([]string, int)
}

func subcommandResolver(cmd commandID) func(*pflag.FlagSet, *bool, []string) (config, *fileConfig, []string, error) {
	return func(fs *pflag.FlagSet, help *bool, args []string) (config, *fileConfig, []string, error) {
		return resolveSubcommandConfig(fs, help, cmd, args)
	}
}

var completionSpecs = map[string]completionSpec{
	"sync": {
		resolve: resolveSyncConfig,
		positional: func(ctx context.Context, c completer, args []string, partial string) ([]string, int) {
			if len(args) > 0 {
				return nil, compNoFileComp
			}
			return c.ownerRepos(partial, true, false)
		},
	},
	"clone": {
		resolve: subcommandResolver(cmdIDClone),
		positional: func(ctx context.Context, c completer, args []string, partial string) ([]string, int) {
			return c.ownerRepos(partial, false, false)
		},
	},
	"untrack": {
		resolve: subcommandResolver(cmdWorktree),
		positional: func(ctx context.Context, c completer, args []string, partial string) ([]string, int) {
			if len(args) > 0 {
				return nil, compNoFileComp
			}
			return c.ownerRepos(partial, false, false)
		},
	},
}

var worktreeCompletionSpecs = map[string]completionSpec{
	"add": {
		resolve: subcommandResolver(cmdIDWorktreeAdd),
		positional: func(ctx context.Context, c completer, args []string, partial string) ([]string, int) {
			switch len(args) {
			case 0:
				return c.ownerRepos(partial, false, false)
			case 1:
				return c.branches(ctx, args[0], partial)
			case 2:
				return nil, compFilterDirs
			}
			return nil, compNoFileComp
		},
	},
	"remove": {
		resolve: subcommandResolver(cmdWorktree),
		flags: func(fs *pflag.FlagSet) {
			fs.Bool("force", false, "")
		},
		positional: func(ctx context.Context, c completer, args []string, partial string) ([]string, int) {
			switch len(args) {
			case 0:
				// The path-only form shares this position: when nothing
				// in the workspace matches, the word is a path.
				candidates, directive := c.ownerRepos(partial, false, true)
				if len(candidates) == 0 {
					return nil, compDefault
				}
				return candidates, directive
			case 1:
				return c.worktreePaths(ctx, args[0], partial)
			}
			return nil, compNoFileComp
		},
	},
	"list": {
		resolve: subcommandResolver(cmdWorktree),
		positional: func(ctx context.Context, c completer, args []string, partial string) ([]string, int) {
			if len(args) > 0 {
				return nil, compNoFileComp
			}
			return c.ownerRepos(partial, true, true)
		},
	},
}

func (s completionSpec) complete(ctx context.Context, words []string, partial string) ([]string, int) {
	fs, help := newSubcommandFlagSet("", "", io.Discard)
	if s.flags != nil {
		s.flags(fs)
	}
	cfg, fc, positional, err := s.resolve(fs, help, words)

	// resolve has registered every flag by now, whether or not parsing
	// succeeded: a value flag with its value still to come fails to parse,
	// and is exactly the case where the shell's own path completion is
	// the right answer.
	if flagAwaitsValue(fs, words) {
		return nil, compDefault
	}
	if strings.HasPrefix(partial, "-") {
		return flagNames(fs, partial), compNoFileComp
	}
	if err != nil {
		return nil, compError
	}
	return s.positional(ctx, completer{cfg: cfg, fc: fc}, positional, partial)
}

func flagAwaitsValue(fs *pflag.FlagSet, words []string) bool {
	if len(words) == 0 {
		return false
	}
	last := words[len(words)-1]
	var f *pflag.Flag
	switch {
	case strings.Contains(last, "=") || last == "--":
		return false
	case strings.HasPrefix(last, "--"):
		f = fs.Lookup(last[2:])
	case strings.HasPrefix(last, "-") && len(last) == 2:
		f = fs.ShorthandLookup(last[1:])
	}
	return f != nil && f.NoOptDefVal == ""
}

func flagNames(fs *pflag.FlagSet, partial string) []string {
	var names []string
	fs.VisitAll(func(f *pflag.Flag) {
		names = append(names, "--"+f.Name)
	})
	return withPrefix(names, partial)
}

type completer struct {
	cfg config
	fc  *fileConfig
}

// ownerRepos completes an <owner>/<repo> argument from what is already on
// disk and in the config, never from GitHub: a network round trip per Tab
// is too slow, and a repo the workspace has never seen can still be typed
// out in full. Before the slash it offers "<owner>/" (and the bare owner
// too where the command accepts one); after it, that owner's repos.
// clonedOnly is for commands that act on an existing clone.
func (c completer) ownerRepos(partial string, ownerAlone, clonedOnly bool) ([]string, int) {
	owner, repoPrefix, hasSlash := strings.Cut(partial, "/")
	if !hasSlash {
		var candidates []string
		for _, o := range withPrefix(c.owners(clonedOnly), partial) {
			if ownerAlone {
				candidates = append(candidates, o)
			}
			candidates = append(candidates, o+"/")
		}
		return candidates, compNoSpace | compNoFileComp
	}

	var candidates []string
	for _, r := range withPrefix(c.repos(owner, clonedOnly), repoPrefix) {
		candidates = append(candidates, owner+"/"+r)
	}
	return candidates, compNoFileComp
}

func (c completer) owners(clonedOnly bool) []string {
	onDisk, _ := store.Owners(c.cfg.Root)
	var owners []string
	for _, o := range onDisk {
		if clonedOnly {
			if repos, _ := store.ClonedRepos(c.cfg.Root, o); len(repos) == 0 {
				continue
			}
		}
		owners = append(owners, o)
	}
	if !clonedOnly && c.fc != nil {
		for _, oc := range c.fc.Owners {
			owners = append(owners, oc.Name)
		}
		for _, rc := range c.fc.Repos {
			if o, _, ok := strings.Cut(rc.Name, "/"); ok {
				owners = append(owners, o)
			}
		}
	}
	return sortedValid(owners, settings.ValidOwnerName)
}

func (c completer) repos(owner string, clonedOnly bool) []string {
	if !settings.ValidOwnerName(owner) {
		return nil
	}
	repos, _ := store.ClonedRepos(c.cfg.Root, owner)
	if !clonedOnly {
		st := loadState(store.StatePath(c.cfg.Root, owner), owner, io.Discard)
		for name, rs := range st.Repos {
			if rs.Status != statusArchived {
				repos = append(repos, name)
			}
		}
		if c.fc != nil {
			for _, rc := range c.fc.Repos {
				if o, r, ok := strings.Cut(rc.Name, "/"); ok && o == owner {
					repos = append(repos, r)
				}
			}
		}
	}
	return sortedValid(repos, validRepoName)
}

func (c completer) branches(ctx context.Context, ownerRepo, partial string) ([]string, int) {
	dir, ok := c.cloneDir(ownerRepo)
	if !ok {
		return nil, compNoFileComp
	}
	branches, err := gitcli.Branches(ctx, c.cfg.Deps.exec, c.timeout(), dir)
	if err != nil {
		return nil, compNoFileComp
	}
	return withPrefix(branches, partial), compNoFileComp
}

func (c completer) worktreePaths(ctx context.Context, ownerRepo, partial string) ([]string, int) {
	dir, ok := c.cloneDir(ownerRepo)
	if !ok {
		return nil, compNoFileComp
	}
	worktrees, err := vcs.WorktreePaths(ctx, c.cfg.Deps.exec, c.timeout(), dir)
	if err != nil {
		return nil, compNoFileComp
	}
	paths := make([]string, len(worktrees))
	for i, w := range worktrees {
		paths[i] = w.Path
	}
	return withPrefix(paths, partial), compNoFileComp
}

func (c completer) cloneDir(ownerRepo string) (string, bool) {
	owner, repo, err := parseOwnerRepo(ownerRepo)
	if err != nil {
		return "", false
	}
	dir := filepath.Join(store.ReposDir(c.cfg.Root, owner), repo)
	_, err = os.Stat(filepath.Join(dir, ".git"))
	return dir, err == nil
}

func (c completer) timeout() time.Duration {
	return min(c.cfg.Timeout, completionTimeout)
}

func withPrefix(candidates []string, prefix string) []string {
	var out []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func sortedValid(names []string, valid func(string) bool) []string {
	slices.Sort(names)
	names = slices.Compact(names)
	return slices.DeleteFunc(names, func(n string) bool { return !valid(n) })
}
