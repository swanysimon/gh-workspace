// Package settings is this tool's resolvable configuration: every value
// that can come from a flag, an env var, and/or a config-file key, with
// flags > env > file > defaults precedence, plus the config file's own
// shape and the default root/config-file path resolution. It does not
// include anything positional-arg-derived (an owner/org name) or
// runtime-wiring (exec/confirm seams) — those are the caller's concern,
// not a "setting."
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/pflag"
)

// Settings holds every value this package resolves.
type Settings struct {
	Root         string
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
}

// Default returns every setting at its built-in default, before any
// file/env/flag overlay.
func Default() Settings {
	return Settings{
		Root:         DefaultRoot(),
		Concurrency:  8,
		Timeout:      30 * time.Minute,
		MaxRepos:     10000,
		Protocol:     "ssh",
		IncludeForks: false,
		Archive:      true,
	}
}

func DefaultRoot() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "gh-org-clone")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return filepath.Join(home, ".local", "share", "gh-org-clone")
}

// Validate checks every field Settings owns. It does not check an owner/org
// name (that's the caller's job — Settings has no such field).
func Validate(s Settings) error {
	if s.Concurrency < 1 {
		return fmt.Errorf("concurrency must be >= 1, got %d", s.Concurrency)
	}
	if s.MaxRepos < 1 {
		return fmt.Errorf("max-repos must be >= 1, got %d", s.MaxRepos)
	}
	if s.Timeout <= 0 {
		return fmt.Errorf("timeout must be > 0, got %s", s.Timeout)
	}
	if s.Protocol != "ssh" && s.Protocol != "https" {
		return fmt.Errorf("protocol must be ssh or https, got %q", s.Protocol)
	}
	if !filepath.IsAbs(s.Root) {
		return fmt.Errorf("root must be an absolute path, got %q", s.Root)
	}
	return nil
}

// FileConfig is the config file's shape; a nil field means the file didn't
// set that key.
type FileConfig struct {
	Root         *string `json:"root"`
	Concurrency  *int    `json:"concurrency"`
	Timeout      *string `json:"timeout"` // parsed with time.ParseDuration
	MaxRepos     *int    `json:"maxRepos"`
	Protocol     *string `json:"protocol"`
	IncludeForks *bool   `json:"includeForks"`
	Archive      *bool   `json:"archive"`
}

func ResolveConfigPath(flagValue string) string {
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

// LoadFileConfig returns nil, nil when the file does not exist. Any other
// read or parse failure is a hard error — never guess at config intent.
func LoadFileConfig(path string) (*FileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	defer f.Close()

	var fc FileConfig
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	return &fc, nil
}

// CommandID distinguishes which commands a setting applies to. Only two
// exist today: the top-level sync command, and every "worktree" subcommand
// (add/remove/list), which all share the same reduced flag/env/file surface.
// Before this table existed, that reduced surface was a hardcoded list in
// resolveWorktreeConfig (env vars) and a separate hand-picked if-chain (file
// keys) -- two lists that had to be kept in sync by hand. Now there is one.
type CommandID string

const (
	CmdSync     CommandID = "sync"
	CmdWorktree CommandID = "worktree"
	// CmdClone is the single-repo `clone <owner>/<repo>` command: it needs
	// --archive/--force (its decision matrix is the same plan.Decide a full
	// sync uses) in addition to the root/timeout/protocol CmdWorktree
	// already needs, but not --concurrency/--max-repos/--include-forks,
	// which only make sense for a whole owner's listing.
	CmdClone CommandID = "clone"
)

// settingKind selects how a setting's flag is registered on a pflag.FlagSet
// and how its env var / config-file value is parsed before being applied to
// a Settings. Every kind ultimately produces a Go value of the matching
// type (string, int, bool, or time.Duration) passed to apply.
type settingKind int

const (
	kindString settingKind = iota
	kindInt
	kindBool
	// kindDuration flags and env vars are plain strings, parsed with
	// time.ParseDuration -- not pflag's own DurationVar -- so a bad value
	// gets this package's own error wording instead of pflag's.
	kindDuration
)

// Setting is one piece of config resolvable from a flag, an env var, and/or
// a config-file key, with flags > env > file > defaults precedence.
// commands is the single source of truth for which commands accept it: it
// drives both which flag gets registered on a command's FlagSet (via
// BindSettings) and which env vars/file keys that command's resolve pass is
// allowed to read (via ResolveSettings), so the two can never silently
// drift apart the way the hand-maintained lists they replace could.
//
// envVar and configKey are "" for a setting with no env var or config-file
// key respectively -- every bool flag unique to the sync command (--force,
// --dry-run, --verbose, --yes) is layered from flags only, so both are "".
type Setting struct {
	flagName  string
	shorthand string
	kind      settingKind
	usage     string
	envVar    string
	configKey string // documentation only (fileValue is what's actually read); kept so the setting's own definition names its file key instead of leaving it implicit in a closure
	commands  map[CommandID]bool

	// fileValue extracts this setting's value from fc, if present. It
	// returns present=false, not an error, when fc is nil or the key was
	// not set in the file; a malformed value (currently only possible for
	// the timeout setting's duration string) is the one case that returns
	// an error. nil for a setting with no config-file key.
	fileValue func(fc *FileConfig) (value any, present bool, err error)

	// parseEnv parses v (the env var's raw string value) into the same Go
	// type fileValue and the flag both produce. nil for a setting with no
	// env var.
	parseEnv func(v string) (any, error)

	// apply sets this setting's field on s from a value already parsed
	// into its Go type (string/int/bool/time.Duration, matching kind).
	apply func(s *Settings, value any)
}

func inCmd(ids ...CommandID) map[CommandID]bool {
	m := make(map[CommandID]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// parseEnvInt and parseEnvBool are shared by every int/bool setting's
// parseEnv, so the "invalid integer/bool %q" wording can't drift between
// settings the way independent copies could.
func parseEnvInt(v string) (any, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil, fmt.Errorf("invalid integer %q: %w", v, err)
	}
	return n, nil
}

func parseEnvBool(v string) (any, error) {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil, fmt.Errorf("invalid bool %q: %w", v, err)
	}
	return b, nil
}

// settingsTable is every setting gh-org-clone resolves, in the order they
// appear in --help. --config itself (and its GH_ORG_CLONE_CONFIG env var)
// is deliberately not here: it names which file to load, so it can't be a
// value *within* that file, and it has its own special-cased handling in
// the caller (identical to before this table existed). --help is likewise
// special-cased by the caller.
var settingsTable = []Setting{
	{
		flagName: "root", kind: kindString, usage: "root directory for cloned orgs",
		envVar: "GH_ORG_CLONE_ROOT", configKey: "root", commands: inCmd(CmdSync, CmdWorktree, CmdClone),
		fileValue: func(fc *FileConfig) (any, bool, error) {
			if fc.Root == nil {
				return nil, false, nil
			}
			return *fc.Root, true, nil
		},
		parseEnv: func(v string) (any, error) { return v, nil },
		apply:    func(s *Settings, v any) { s.Root = v.(string) },
	},
	{
		flagName: "concurrency", kind: kindInt, usage: "number of repos to sync in parallel",
		envVar: "GH_ORG_CLONE_CONCURRENCY", configKey: "concurrency", commands: inCmd(CmdSync),
		fileValue: func(fc *FileConfig) (any, bool, error) {
			if fc.Concurrency == nil {
				return nil, false, nil
			}
			return *fc.Concurrency, true, nil
		},
		parseEnv: parseEnvInt,
		apply:    func(s *Settings, v any) { s.Concurrency = v.(int) },
	},
	{
		flagName: "timeout", kind: kindDuration, usage: "per-subprocess timeout",
		envVar: "GH_ORG_CLONE_TIMEOUT", configKey: "timeout", commands: inCmd(CmdSync, CmdWorktree, CmdClone),
		fileValue: func(fc *FileConfig) (any, bool, error) {
			if fc.Timeout == nil {
				return nil, false, nil
			}
			d, err := time.ParseDuration(*fc.Timeout)
			if err != nil {
				return nil, false, fmt.Errorf("config file: invalid timeout %q: %w", *fc.Timeout, err)
			}
			return d, true, nil
		},
		parseEnv: func(v string) (any, error) {
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("invalid duration %q: %w", v, err)
			}
			return d, nil
		},
		apply: func(s *Settings, v any) { s.Timeout = v.(time.Duration) },
	},
	{
		flagName: "max-repos", kind: kindInt, usage: "maximum repos to list from the org (gh --limit)",
		envVar: "GH_ORG_CLONE_MAX_REPOS", configKey: "maxRepos", commands: inCmd(CmdSync),
		fileValue: func(fc *FileConfig) (any, bool, error) {
			if fc.MaxRepos == nil {
				return nil, false, nil
			}
			return *fc.MaxRepos, true, nil
		},
		parseEnv: parseEnvInt,
		apply:    func(s *Settings, v any) { s.MaxRepos = v.(int) },
	},
	{
		flagName: "protocol", kind: kindString, usage: "clone protocol: ssh or https",
		envVar: "GH_ORG_CLONE_PROTOCOL", configKey: "protocol", commands: inCmd(CmdSync, CmdWorktree, CmdClone),
		fileValue: func(fc *FileConfig) (any, bool, error) {
			if fc.Protocol == nil {
				return nil, false, nil
			}
			return *fc.Protocol, true, nil
		},
		parseEnv: func(v string) (any, error) { return v, nil },
		apply:    func(s *Settings, v any) { s.Protocol = v.(string) },
	},
	{
		flagName: "include-forks", kind: kindBool, usage: "include forked repos",
		envVar: "GH_ORG_CLONE_INCLUDE_FORKS", configKey: "includeForks", commands: inCmd(CmdSync),
		fileValue: func(fc *FileConfig) (any, bool, error) {
			if fc.IncludeForks == nil {
				return nil, false, nil
			}
			return *fc.IncludeForks, true, nil
		},
		parseEnv: parseEnvBool,
		apply:    func(s *Settings, v any) { s.IncludeForks = v.(bool) },
	},
	{
		flagName: "archive", kind: kindBool, usage: "tarball archived repos and remove their clones",
		envVar: "GH_ORG_CLONE_ARCHIVE", configKey: "archive", commands: inCmd(CmdSync, CmdClone),
		fileValue: func(fc *FileConfig) (any, bool, error) {
			if fc.Archive == nil {
				return nil, false, nil
			}
			return *fc.Archive, true, nil
		},
		parseEnv: parseEnvBool,
		apply:    func(s *Settings, v any) { s.Archive = v.(bool) },
	},
	{
		flagName: "force", kind: kindBool, usage: "ignore stored pushedAt and re-sync every repo",
		commands: inCmd(CmdSync, CmdClone),
		apply:    func(s *Settings, v any) { s.Force = v.(bool) },
	},
	{
		flagName: "dry-run", kind: kindBool, usage: "print the planned actions without doing them",
		commands: inCmd(CmdSync),
		apply:    func(s *Settings, v any) { s.DryRun = v.(bool) },
	},
	{
		flagName: "verbose", shorthand: "v", kind: kindBool, usage: "verbose output",
		commands: inCmd(CmdSync),
		apply:    func(s *Settings, v any) { s.Verbose = v.(bool) },
	},
	{
		flagName: "yes", kind: kindBool, usage: "don't prompt before removing worktrees to archive a repo they belong to",
		commands: inCmd(CmdSync),
		apply:    func(s *Settings, v any) { s.Yes = v.(bool) },
	},
}

// SettingsFor returns every setting that applies to cmd, in table order.
func SettingsFor(cmd CommandID) []Setting {
	var out []Setting
	for _, s := range settingsTable {
		if s.commands[cmd] {
			out = append(out, s)
		}
	}
	return out
}

// BoundFlags holds the pflag-bound variable for each setting applicable to
// a command, keyed by flag name, so ResolveSettings can read back what the
// user actually typed after fs.Parse. The pointer's concrete type always
// matches its setting's kind (*string for kindString/kindDuration, *int for
// kindInt, *bool for kindBool).
type BoundFlags map[string]any

// BindSettings registers every setting applicable to cmd onto fs (not
// --help or --config -- see settingsTable's doc comment) and returns the
// bound variables so ResolveSettings can read them back after fs.Parse. Any
// extra, command-specific flag unrelated to this table (e.g. worktree
// remove's own --force, whose meaning has nothing to do with the sync
// command's --force) must be registered by the caller directly on the same
// fs, which may happen before or after this call.
func BindSettings(fs *pflag.FlagSet, cmd CommandID) BoundFlags {
	bound := make(BoundFlags)
	for _, s := range SettingsFor(cmd) {
		switch s.kind {
		case kindString, kindDuration:
			bound[s.flagName] = fs.StringP(s.flagName, s.shorthand, "", s.usage)
		case kindInt:
			bound[s.flagName] = fs.IntP(s.flagName, s.shorthand, 0, s.usage)
		case kindBool:
			bound[s.flagName] = fs.BoolP(s.flagName, s.shorthand, false, s.usage)
		}
	}
	return bound
}

// ResolveSettings applies flags > env > file > defaults for exactly the
// settings cmd declares, over s (already seeded by Default()). It assumes
// fs.Parse has already succeeded (so fs.Changed/fs.Args reflect the real
// invocation) and that bound came from BindSettings(fs, cmd).
//
// A pure-flag setting (envVar == "" && fileValue == nil, e.g. --dry-run) is
// handled by exactly the same "apply only if fs.Changed" step as every
// other setting in the loop below, with no special case: when such a flag
// is left at its zero value, s already holds that same zero value from
// Default(), so skipping the apply is equivalent to always applying it.
func ResolveSettings(s *Settings, cmd CommandID, fs *pflag.FlagSet, bound BoundFlags, fc *FileConfig) error {
	applicable := SettingsFor(cmd)

	if fc != nil {
		for _, set := range applicable {
			if set.fileValue == nil {
				continue
			}
			v, present, err := set.fileValue(fc)
			if err != nil {
				return err
			}
			if present {
				set.apply(s, v)
			}
		}
	}

	for _, set := range applicable {
		if set.envVar == "" {
			continue
		}
		raw := os.Getenv(set.envVar)
		if raw == "" {
			continue
		}
		v, err := set.parseEnv(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", set.envVar, err)
		}
		set.apply(s, v)
	}

	for _, set := range applicable {
		if !fs.Changed(set.flagName) {
			continue
		}
		switch set.kind {
		case kindString:
			set.apply(s, *bound[set.flagName].(*string))
		case kindInt:
			set.apply(s, *bound[set.flagName].(*int))
		case kindBool:
			set.apply(s, *bound[set.flagName].(*bool))
		case kindDuration:
			raw := *bound[set.flagName].(*string)
			d, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("--%s: invalid duration %q: %w", set.flagName, raw, err)
			}
			set.apply(s, d)
		}
	}
	return nil
}
