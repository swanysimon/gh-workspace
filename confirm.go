package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

// confirmMu serializes prompts across concurrent workers. Archiving runs
// cfg.Concurrency-wide in parallel; without this, two repos hitting a live
// worktree at once would interleave their prompts into unreadable output.
var confirmMu sync.Mutex

// defaultConfirmArchiveWithWorktrees is confirmDefault's production value
// (see deps.go). It never blocks a non-interactive run: cfg.Yes answers
// "yes" unconditionally (for scripted use where the caller has already
// decided), and anything else answers "no" unless stdin is a terminal, so
// cron/CI runs fail closed instead of hanging forever on a prompt nobody
// can see.
func defaultConfirmArchiveWithWorktrees(cfg config, repoName string, worktrees []worktreeStatus) (bool, error) {
	if cfg.Yes {
		return true, nil
	}
	if !isInteractive(os.Stdin) {
		return false, nil
	}

	confirmMu.Lock()
	defer confirmMu.Unlock()

	fmt.Fprintf(os.Stderr, "%q has %d live worktree(s):\n", repoName, len(worktrees))
	for _, w := range worktrees {
		suffix := ""
		if w.Dirty {
			suffix = " (uncommitted changes will be discarded)"
		}
		fmt.Fprintf(os.Stderr, "  %s%s\n", w.Path, suffix)
	}
	fmt.Fprint(os.Stderr, "remove them and continue archiving? [y/N] ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}

// isInteractive is a stdlib-only tty heuristic (no golang.org/x/term
// dependency): a character device is the closest approximation of "there is
// a human who can see and answer this prompt" available without an ioctl,
// except that /dev/null is *also* a character device and is exactly what
// cron and CI redirect stdin from, so it gets an explicit carve-out.
func isInteractive(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if nullInfo, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, nullInfo) {
		return false
	}
	return true
}
