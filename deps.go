package main

import "context"

// Exec is the subprocess seam every gh/git call goes through. Production
// code uses execCommand; tests inject a fake, either via cfg.Deps.exec
// directly (for tests that build a config and call an internal function) or
// via execDefault (for black-box tests that exercise run()/runWorktree(),
// which build their own config internally and so have no cfg to set
// beforehand).
type Exec func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

// ConfirmFunc is the seam archiveRepo uses to ask before removing worktrees,
// instead of calling defaultConfirmArchiveWithWorktrees (and so os.Stdin)
// directly. Same two injection points as Exec: cfg.Deps.confirm, or
// confirmDefault for black-box tests.
type ConfirmFunc func(cfg config, repoName string, worktrees []worktreeStatus) (bool, error)

// deps carries every side-effecting dependency a config-threading function
// needs, so none of them read a package-level global directly. It is a
// field of config (see defaultConfig) rather than a separate parameter
// threaded everywhere, since config is already threaded through every
// entry point.
//
// Adding deps to config means config is no longer a comparable type: a
// struct with a function-typed field (Exec, ConfirmFunc) cannot be compared
// with ==/!=, at compile time, regardless of the values inside it. Tests
// that used to compare whole configs (TestConfigDefaults) now compare
// individual settings fields instead.
type deps struct {
	exec    Exec
	confirm ConfirmFunc
}

// execDefault and confirmDefault are the only package-level seams left, and
// they are consulted in exactly one place: defaultDeps, at config
// construction time. Every function that actually runs a subprocess or
// prompts a user takes its exec/confirm from cfg.Deps, not from a global,
// so a test that builds a config literal can inject a fake straight into
// it. Only black-box tests that exercise run()/runWorktree() via
// args-style parsing -- which construct cfg themselves, internally -- need
// to override these two before calling in.
var (
	execDefault    Exec        = execCommand
	confirmDefault ConfirmFunc = defaultConfirmArchiveWithWorktrees
)

func defaultDeps() deps {
	return deps{exec: execDefault, confirm: confirmDefault}
}
