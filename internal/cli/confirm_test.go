package cli

// This file tests confirm.go's one shim, defaultConfirmArchiveWithWorktrees,
// directly: the real prompt implementation now lives in and is tested by
// internal/archive (DefaultConfirm, isInteractive); what's left here is the
// shim's one piece of real logic, converting cfg.Yes into
// archive.DefaultConfirm's yes parameter correctly.

import (
	"os"
	"testing"

	"github.com/swanysimon/gh-workspace/internal/settings"
)

func TestDefaultConfirmArchiveWithWorktreesYes(t *testing.T) {
	ok, err := defaultConfirmArchiveWithWorktrees(config{Settings: settings.Settings{Yes: true}}, "repo1", []worktreeStatus{{Path: "/tmp/wt"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("cfg.Yes=true should answer yes without touching stdin")
	}
}

func TestDefaultConfirmArchiveWithWorktreesNonInteractive(t *testing.T) {
	// /dev/null is a character device but must never be treated as a
	// terminal a human could answer a prompt on; it's exactly what cron and
	// CI redirect stdin from. (The isInteractive heuristic itself now lives
	// in, and is tested directly by, internal/archive; this test exercises
	// the shim end to end instead of calling it directly.)
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	oldStdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = oldStdin })

	ok, err := defaultConfirmArchiveWithWorktrees(config{}, "repo1", []worktreeStatus{{Path: "/tmp/wt"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("a non-interactive run with cfg.Yes=false must never answer yes on its own")
	}
}
