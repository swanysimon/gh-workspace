package main

import (
	"github.com/spf13/pflag"
	"github.com/swanysimon/gh-org-clone/internal/settings"
)

// commandID, cmdSync, cmdWorktree, and boundFlags are aliases for
// internal/settings' types/consts, which now hold the real settings table
// (settings.go's old content, moved verbatim, operating on a plain
// settings.Settings instead of config -- see config's embedding of
// settings.Settings in main.go).
type commandID = settings.CommandID
type boundFlags = settings.BoundFlags

const (
	cmdSync     = settings.CmdSync
	cmdWorktree = settings.CmdWorktree
)

// bindSettings registers every setting applicable to cmd onto fs; see
// settings.BindSettings for the full behavior.
func bindSettings(fs *pflag.FlagSet, cmd commandID) boundFlags {
	return settings.BindSettings(fs, cmd)
}

// resolveSettings applies flags > env > file > defaults for exactly the
// settings cmd declares, into cfg's embedded settings.Settings; see
// settings.ResolveSettings for the full behavior.
func resolveSettings(cfg *config, cmd commandID, fs *pflag.FlagSet, bound boundFlags, fc *fileConfig) error {
	return settings.ResolveSettings(&cfg.Settings, cmd, fs, bound, fc)
}
