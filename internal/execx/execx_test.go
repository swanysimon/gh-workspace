package execx

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunCapturesStdout(t *testing.T) {
	out, err := Run(context.Background(), "", "echo", "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestRunErrorIncludesStderr(t *testing.T) {
	_, err := Run(context.Background(), "", "sh", "-c", "printf '  boom  \\n' >&2; exit 7")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The error's stderr portion is everything after the last ": ", so an
	// args-portion false positive (the shell script text itself contains
	// "boom") can't make this pass by accident.
	if !strings.HasSuffix(err.Error(), ": boom") {
		t.Fatalf("error %q does not end with stderr trimmed of surrounding whitespace", err)
	}
	if !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("error %q does not include the exit status", err)
	}
}

func TestRunUsesDir(t *testing.T) {
	out, err := Run(context.Background(), "/", "pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "/" {
		t.Fatalf("got %q, want %q", got, "/")
	}
}

func TestWithTimeoutEnforcesDeadline(t *testing.T) {
	_, err := WithTimeout(context.Background(), 10*time.Millisecond, Run, "", "sleep", "5")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "signal: killed") && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error %q does not look like a timeout", err)
	}
}

func TestWithTimeoutPassesArgsThrough(t *testing.T) {
	var gotDir, gotName string
	var gotArgs []string
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotDir, gotName, gotArgs = dir, name, args
		return []byte("ok"), nil
	}
	out, err := WithTimeout(context.Background(), time.Second, fake, "/tmp", "git", "status")
	if err != nil {
		t.Fatalf("WithTimeout: %v", err)
	}
	if string(out) != "ok" || gotDir != "/tmp" || gotName != "git" || len(gotArgs) != 1 || gotArgs[0] != "status" {
		t.Fatalf("got dir=%q name=%q args=%v out=%q", gotDir, gotName, gotArgs, out)
	}
}
