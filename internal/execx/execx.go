// Package execx is the one place that actually starts a subprocess. Every
// other internal package takes an Exec value instead of calling exec.Command
// directly, so none of them need a package-level test seam of their own —
// that was the whole point of deps.go's Phase 1 refactor, carried through
// into the package split it was done to enable.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Exec runs name with args, in dir (the current directory if dir is ""),
// and returns its stdout. A non-nil error's text includes the command's
// stderr, trimmed, so callers don't need to capture it themselves.
type Exec func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

// Run is Exec's real, production implementation. GIT_TERMINAL_PROMPT=0 and
// GIT_ADVICE=0 keep a hung credential prompt or an advice message from ever
// reaching a human who isn't there to see it -- every caller through this
// package gets both, regardless of whether the command is git or gh.
//
// Stdout is returned even when err is non-nil (a change from this
// function's original behavior, which discarded it on any non-zero exit).
// This matters for ghcli.ViewRepos: a GraphQL query can come back with a
// non-zero exit from gh while still printing a usable partial response body
// (data for some aliases, errors for others) on stdout -- discarding it
// unconditionally would make that case unparseable by construction. Every
// existing caller already structures its own error handling to never read
// the output on a non-nil error, so returning it now rather than nil is a
// strictly backward-compatible widening, not a behavior change for them.
func Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ADVICE=0")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// WithTimeout runs one command bounded by timeout, replacing the
// "ctx, cancel := context.WithTimeout(...); defer cancel(); return
// exec(ctx, ...)" boilerplate every gitcli/ghcli call site used to repeat
// for itself.
func WithTimeout(ctx context.Context, timeout time.Duration, run Exec, dir, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return run(ctx, dir, name, args...)
}
