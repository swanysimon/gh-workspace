package cli

import (
	"github.com/spf13/pflag"
	"github.com/swanysimon/gh-org-clone/internal/settings"
)

// commandID, cmdSync, cmdWorktree, cmdIDWorktreeAdd, cmdIDClone, and
// boundFlags are aliases for internal/settings' types/consts, which now
// hold the real settings table (settings.go's old content, moved
// verbatim, operating on a plain settings.Settings instead of config --
// see config's embedding of settings.Settings in main.go). cmdIDClone and
// cmdIDWorktreeAdd (not cmdClone/cmdWorktreeAdd) specifically to avoid
// colliding with clone.go's cmdClone and worktree.go's cmdWorktreeAdd
// dispatch functions, which otherwise want the exact same names as their
// settings-table commandIDs.
type commandID = settings.CommandID
type boundFlags = settings.BoundFlags

const (
	cmdSync          = settings.CmdSync
	cmdWorktree      = settings.CmdWorktree
	cmdIDWorktreeAdd = settings.CmdWorktreeAdd
	cmdIDClone       = settings.CmdClone
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
