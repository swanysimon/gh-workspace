package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/pflag"
)

// commandID distinguishes which commands a setting applies to. Only two
// exist today: the top-level sync command, and every "worktree" subcommand
// (add/remove/list), which all share the same reduced flag/env/file surface.
// Before this table existed, that reduced surface was a hardcoded list in
// resolveWorktreeConfig (env vars) and a separate hand-picked if-chain (file
// keys) -- two lists that had to be kept in sync by hand. Now there is one.
type commandID string

const (
	cmdSync     commandID = "sync"
	cmdWorktree commandID = "worktree"
)

// settingKind selects how a setting's flag is registered on a pflag.FlagSet
// and how its env var / config-file value is parsed before being applied to
// a config. Every kind ultimately produces a Go value of the matching type
// (string, int, bool, or time.Duration) passed to apply.
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

// setting is one piece of config resolvable from a flag, an env var, and/or
// a config-file key, with flags > env > file > defaults precedence.
// commands is the single source of truth for which commands accept it: it
// drives both which flag gets registered on a command's FlagSet (via
// bindSettings) and which env vars/file keys that command's resolve pass is
// allowed to read (via resolveSettings), so the two can never silently
// drift apart the way the hand-maintained lists they replace could.
//
// envVar and configKey are "" for a setting with no env var or config-file
// key respectively -- every bool flag unique to the sync command (--force,
// --dry-run, --verbose, --yes) is layered from flags only, so both are "".
type setting struct {
	flagName  string
	shorthand string
	kind      settingKind
	usage     string
	envVar    string
	configKey string // documentation only (fileValue is what's actually read); kept so the setting's own definition names its file key instead of leaving it implicit in a closure
	commands  map[commandID]bool

	// fileValue extracts this setting's value from fc, if present. It
	// returns present=false, not an error, when fc is nil or the key was
	// not set in the file; a malformed value (currently only possible for
	// the timeout setting's duration string) is the one case that returns
	// an error. nil for a setting with no config-file key.
	fileValue func(fc *fileConfig) (value any, present bool, err error)

	// parseEnv parses v (the env var's raw string value) into the same Go
	// type fileValue and the flag both produce. nil for a setting with no
	// env var.
	parseEnv func(v string) (any, error)

	// apply sets this setting's field on cfg from a value already parsed
	// into its Go type (string/int/bool/time.Duration, matching kind).
	apply func(cfg *config, value any)
}

func inCmd(ids ...commandID) map[commandID]bool {
	m := make(map[commandID]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// parseEnvInt and parseEnvBool are shared by every int/bool setting's
// parseEnv, so the "invalid integer/bool %q" wording can't drift between
// settings the way three independent copies could.
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
// resolveConfig/resolveWorktreeConfig (identical to before this table
// existed). --help is likewise special-cased (see newFlagSet).
var settingsTable = []setting{
	{
		flagName: "root", kind: kindString, usage: "root directory for cloned orgs",
		envVar: "GH_ORG_CLONE_ROOT", configKey: "root", commands: inCmd(cmdSync, cmdWorktree),
		fileValue: func(fc *fileConfig) (any, bool, error) {
			if fc.Root == nil {
				return nil, false, nil
			}
			return *fc.Root, true, nil
		},
		parseEnv: func(v string) (any, error) { return v, nil },
		apply:    func(cfg *config, v any) { cfg.Root = v.(string) },
	},
	{
		flagName: "concurrency", kind: kindInt, usage: "number of repos to sync in parallel",
		envVar: "GH_ORG_CLONE_CONCURRENCY", configKey: "concurrency", commands: inCmd(cmdSync),
		fileValue: func(fc *fileConfig) (any, bool, error) {
			if fc.Concurrency == nil {
				return nil, false, nil
			}
			return *fc.Concurrency, true, nil
		},
		parseEnv: parseEnvInt,
		apply:    func(cfg *config, v any) { cfg.Concurrency = v.(int) },
	},
	{
		flagName: "timeout", kind: kindDuration, usage: "per-subprocess timeout",
		envVar: "GH_ORG_CLONE_TIMEOUT", configKey: "timeout", commands: inCmd(cmdSync, cmdWorktree),
		fileValue: func(fc *fileConfig) (any, bool, error) {
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
		apply: func(cfg *config, v any) { cfg.Timeout = v.(time.Duration) },
	},
	{
		flagName: "max-repos", kind: kindInt, usage: "maximum repos to list from the org (gh --limit)",
		envVar: "GH_ORG_CLONE_MAX_REPOS", configKey: "maxRepos", commands: inCmd(cmdSync),
		fileValue: func(fc *fileConfig) (any, bool, error) {
			if fc.MaxRepos == nil {
				return nil, false, nil
			}
			return *fc.MaxRepos, true, nil
		},
		parseEnv: parseEnvInt,
		apply:    func(cfg *config, v any) { cfg.MaxRepos = v.(int) },
	},
	{
		flagName: "protocol", kind: kindString, usage: "clone protocol: ssh or https",
		envVar: "GH_ORG_CLONE_PROTOCOL", configKey: "protocol", commands: inCmd(cmdSync, cmdWorktree),
		fileValue: func(fc *fileConfig) (any, bool, error) {
			if fc.Protocol == nil {
				return nil, false, nil
			}
			return *fc.Protocol, true, nil
		},
		parseEnv: func(v string) (any, error) { return v, nil },
		apply:    func(cfg *config, v any) { cfg.Protocol = v.(string) },
	},
	{
		flagName: "include-forks", kind: kindBool, usage: "include forked repos",
		envVar: "GH_ORG_CLONE_INCLUDE_FORKS", configKey: "includeForks", commands: inCmd(cmdSync),
		fileValue: func(fc *fileConfig) (any, bool, error) {
			if fc.IncludeForks == nil {
				return nil, false, nil
			}
			return *fc.IncludeForks, true, nil
		},
		parseEnv: parseEnvBool,
		apply:    func(cfg *config, v any) { cfg.IncludeForks = v.(bool) },
	},
	{
		flagName: "archive", kind: kindBool, usage: "tarball archived repos and remove their clones",
		envVar: "GH_ORG_CLONE_ARCHIVE", configKey: "archive", commands: inCmd(cmdSync),
		fileValue: func(fc *fileConfig) (any, bool, error) {
			if fc.Archive == nil {
				return nil, false, nil
			}
			return *fc.Archive, true, nil
		},
		parseEnv: parseEnvBool,
		apply:    func(cfg *config, v any) { cfg.Archive = v.(bool) },
	},
	{
		flagName: "force", kind: kindBool, usage: "ignore stored pushedAt and re-sync every repo",
		commands: inCmd(cmdSync),
		apply:    func(cfg *config, v any) { cfg.Force = v.(bool) },
	},
	{
		flagName: "dry-run", kind: kindBool, usage: "print the planned actions without doing them",
		commands: inCmd(cmdSync),
		apply:    func(cfg *config, v any) { cfg.DryRun = v.(bool) },
	},
	{
		flagName: "verbose", shorthand: "v", kind: kindBool, usage: "verbose output",
		commands: inCmd(cmdSync),
		apply:    func(cfg *config, v any) { cfg.Verbose = v.(bool) },
	},
	{
		flagName: "yes", kind: kindBool, usage: "don't prompt before removing worktrees to archive a repo they belong to",
		commands: inCmd(cmdSync),
		apply:    func(cfg *config, v any) { cfg.Yes = v.(bool) },
	},
}

// settingsFor returns every setting that applies to cmd, in table order.
func settingsFor(cmd commandID) []setting {
	var out []setting
	for _, s := range settingsTable {
		if s.commands[cmd] {
			out = append(out, s)
		}
	}
	return out
}

// boundFlags holds the pflag-bound variable for each setting applicable to
// a command, keyed by flag name, so resolveSettings can read back what the
// user actually typed after fs.Parse. The pointer's concrete type always
// matches its setting's kind (*string for kindString/kindDuration, *int for
// kindInt, *bool for kindBool).
type boundFlags map[string]any

// bindSettings registers every setting applicable to cmd onto fs (not
// --help or --config -- see settingsTable's doc comment) and returns the
// bound variables so resolveSettings can read them back after fs.Parse. Any
// extra, command-specific flag unrelated to this table (e.g. worktree
// remove's own --force, whose meaning has nothing to do with the sync
// command's --force) must be registered by the caller directly on the same
// fs, which may happen before or after this call.
func bindSettings(fs *pflag.FlagSet, cmd commandID) boundFlags {
	bound := make(boundFlags)
	for _, s := range settingsFor(cmd) {
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

// resolveSettings applies flags > env > file > defaults for exactly the
// settings cmd declares, over a cfg already seeded by defaultConfig(). It
// assumes fs.Parse has already succeeded (so fs.Changed/fs.Args reflect the
// real invocation) and that bound came from bindSettings(fs, cmd).
//
// A pure-flag setting (envVar == "" && fileValue == nil, e.g. --dry-run) is
// handled by exactly the same "apply only if fs.Changed" step as every
// other setting in the loop below, with no special case: when such a flag
// is left at its zero value, cfg already holds that same zero value from
// defaultConfig(), so skipping the apply is equivalent to always applying
// it.
func resolveSettings(cfg *config, cmd commandID, fs *pflag.FlagSet, bound boundFlags, fc *fileConfig) error {
	applicable := settingsFor(cmd)

	if fc != nil {
		for _, s := range applicable {
			if s.fileValue == nil {
				continue
			}
			v, present, err := s.fileValue(fc)
			if err != nil {
				return err
			}
			if present {
				s.apply(cfg, v)
			}
		}
	}

	for _, s := range applicable {
		if s.envVar == "" {
			continue
		}
		raw := os.Getenv(s.envVar)
		if raw == "" {
			continue
		}
		v, err := s.parseEnv(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", s.envVar, err)
		}
		s.apply(cfg, v)
	}

	for _, s := range applicable {
		if !fs.Changed(s.flagName) {
			continue
		}
		switch s.kind {
		case kindString:
			s.apply(cfg, *bound[s.flagName].(*string))
		case kindInt:
			s.apply(cfg, *bound[s.flagName].(*int))
		case kindBool:
			s.apply(cfg, *bound[s.flagName].(*bool))
		case kindDuration:
			raw := *bound[s.flagName].(*string)
			d, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("--%s: invalid duration %q: %w", s.flagName, raw, err)
			}
			s.apply(cfg, d)
		}
	}
	return nil
}
