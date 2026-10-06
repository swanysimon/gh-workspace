package archive

import (
	"os"
	"testing"
)

func TestDefaultConfirmYes(t *testing.T) {
	ok, err := DefaultConfirm(true)("repo1", []WorktreeStatus{{Path: "/tmp/wt"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("yes=true should answer yes without touching stdin")
	}
}

func TestDefaultConfirmNonInteractive(t *testing.T) {
	// /dev/null is a character device but must never be treated as a
	// terminal a human could answer a prompt on; it's exactly what cron
	// and CI redirect stdin from.
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isInteractive(f) {
		t.Fatalf("/dev/null must never be treated as interactive")
	}

	oldStdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = oldStdin })

	ok, err := DefaultConfirm(false)("repo1", []WorktreeStatus{{Path: "/tmp/wt"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("a non-interactive run with yes=false must never answer yes on its own")
	}
}
