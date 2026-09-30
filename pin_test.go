package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file pins exactly the behavior AIDEV.md's Phase 1 cobra-migration
// step calls out as "must not change": exit codes, --help content,
// interspersed/"--"-terminated flag parsing, and explicit boolean flag
// values, all driven through the real entry points (run, runWorktree, and
// the cmdWorktree* functions) rather than internal helpers like
// parseInterspersed that won't survive the port. A test here failing after
// the cobra port is exactly the "expected, deliberate difference" the plan
// asks to record -- or a real regression, if it wasn't expected.

// homeFor gives a deterministic $HOME (and clears every GH_ORG_CLONE_* env
// var, since a real one leaking from the test runner's shell would make the
// "default root" assertions below flaky) so the "(default ...)" text in
// --help output, which embeds defaultRoot(), is a fixed string across runs
// and machines.
func homeFor(t *testing.T) (home, defaultDataRoot string) {
	t.Helper()
	clearConfigEnv(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	return home, filepath.Join(home, ".local", "share", "gh-org-clone")
}

func TestPinTopLevelHelp(t *testing.T) {
	_, root := homeFor(t)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("run(--help) = %d, want %d; stderr=%s", code, exitSuccess, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("--help wrote to stdout, want stderr only: %q", stdout.String())
	}

	want := "Clone and keep local mirrors of every repository in a GitHub org.\n\n" +
		"USAGE\n" +
		"  gh org-clone [flags] <org>\n" +
		"  gh org-clone worktree add [flags] <org>/<repo> <branch> <path>\n" +
		"  gh org-clone worktree remove [--force] [flags] <org>/<repo> <path>\n" +
		"  gh org-clone worktree list [flags] <org>/<repo>|<org>\n\n" +
		"Run \"gh org-clone worktree <command> --help\" for a worktree command's flags.\n\n" +
		"FLAGS\n" +
		"      --root string        root directory for cloned orgs (default \"" + root + "\")\n" +
		"      --concurrency int    number of repos to sync in parallel (default 8)\n" +
		"      --timeout duration   per-subprocess timeout (default 30m0s)\n" +
		"      --max-repos int      maximum repos to list from the org (gh --limit) (default 10000)\n" +
		"      --protocol string    clone protocol: ssh or https (default \"ssh\")\n" +
		"      --include-forks      include forked repos\n" +
		"      --archive            tarball archived repos and remove their clones\n" +
		"      --force              ignore stored pushedAt and re-sync every repo\n" +
		"      --dry-run            print the planned actions without doing them\n" +
		"  -v, --verbose            verbose output\n" +
		"      --yes                don't prompt before removing worktrees to archive a repo they belong to\n" +
		"      --config string      path to a JSON config file\n"
	if stderr.String() != want {
		t.Fatalf("--help output changed.\ngot:\n%s\nwant:\n%s", stderr.String(), want)
	}
}

func TestPinWorktreeSubcommandHelp(t *testing.T) {
	_, root := homeFor(t)

	cases := []struct {
		args []string
		want string
	}{
		{
			args: []string{"worktree", "add", "--help"},
			want: "USAGE\n" +
				"  gh org-clone worktree add [flags] <org>/<repo> <branch> <path>\n\n" +
				"FLAGS\n" +
				"      --config string      path to a JSON config file\n" +
				"      --protocol string    clone protocol: ssh or https (default \"ssh\")\n" +
				"      --root string        root directory for cloned orgs (default \"" + root + "\")\n" +
				"      --timeout duration   per-subprocess timeout (default 30m0s)\n",
		},
		{
			args: []string{"worktree", "remove", "--help"},
			want: "USAGE\n" +
				"  gh org-clone worktree remove [--force] [flags] <org>/<repo> <path>\n\n" +
				"FLAGS\n" +
				"      --config string      path to a JSON config file\n" +
				"      --force              remove even if the worktree has uncommitted changes\n" +
				"      --protocol string    clone protocol: ssh or https (default \"ssh\")\n" +
				"      --root string        root directory for cloned orgs (default \"" + root + "\")\n" +
				"      --timeout duration   per-subprocess timeout (default 30m0s)\n",
		},
		{
			args: []string{"worktree", "list", "--help"},
			want: "USAGE\n" +
				"  gh org-clone worktree list [flags] <org>/<repo>|<org>\n\n" +
				"FLAGS\n" +
				"      --config string      path to a JSON config file\n" +
				"      --protocol string    clone protocol: ssh or https (default \"ssh\")\n" +
				"      --root string        root directory for cloned orgs (default \"" + root + "\")\n" +
				"      --timeout duration   per-subprocess timeout (default 30m0s)\n",
		},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr)
			if code != exitSuccess {
				t.Fatalf("run(%v) = %d, want %d; stderr=%s", tc.args, code, exitSuccess, stderr.String())
			}
			if stderr.String() != tc.want {
				t.Fatalf("%v output changed.\ngot:\n%s\nwant:\n%s", tc.args, stderr.String(), tc.want)
			}
		})
	}
}

func TestPinWorktreeUsageOnBadInvocation(t *testing.T) {
	homeFor(t)

	worktreeUsage := "USAGE\n" +
		"  gh org-clone worktree add [flags] <org>/<repo> <branch> <path>\n" +
		"  gh org-clone worktree remove [--force] [flags] <org>/<repo> <path>\n" +
		"  gh org-clone worktree list [flags] <org>/<repo>|<org>\n"

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no verb", []string{"worktree"}, worktreeUsage},
		{"unknown verb", []string{"worktree", "bogus"}, "unknown worktree subcommand \"bogus\"\n" + worktreeUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr)
			if code != exitUsage {
				t.Fatalf("run(%v) = %d, want %d; stderr=%s", tc.args, code, exitUsage, stderr.String())
			}
			if stderr.String() != tc.want {
				t.Fatalf("%v usage output changed.\ngot:\n%s\nwant:\n%s", tc.args, stderr.String(), tc.want)
			}
		})
	}
}

// TestPinExitCodes locks down every documented exit code (see the
// exitSuccess/exitUsage/exitRuntimeFail/exitInterrupted constants in
// main.go) against the real entry points. exitSuccess already has coverage
// elsewhere (TestHelpExitsZero); this adds every case that had none:
// exitUsage from run() itself (as opposed to from resolveConfig in
// isolation, which TestConfigRejects already checks), exitInterrupted (no
// prior coverage at all), and a second exitRuntimeFail case for symmetry
// with the pre-existing TestRunNoGh (kept here so all four exit codes are
// visible in one table rather than split across files).
func TestPinExitCodes(t *testing.T) {
	t.Run("usage: no org argument", func(t *testing.T) {
		homeFor(t)
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{}, &stdout, &stderr)
		if code != exitUsage {
			t.Fatalf("run([]) = %d, want %d; stderr=%s", code, exitUsage, stderr.String())
		}
		if !strings.Contains(stderr.String(), "expected exactly one org argument, got 0") {
			t.Fatalf("stderr missing the expected error: %s", stderr.String())
		}
	})

	t.Run("usage: two positional arguments", func(t *testing.T) {
		homeFor(t)
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"a", "b"}, &stdout, &stderr)
		if code != exitUsage {
			t.Fatalf("run(a, b) = %d, want %d; stderr=%s", code, exitUsage, stderr.String())
		}
	})

	t.Run("usage: unknown flag", func(t *testing.T) {
		homeFor(t)
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"--nope", "myorg"}, &stdout, &stderr)
		if code != exitUsage {
			t.Fatalf("run(--nope) = %d, want %d; stderr=%s", code, exitUsage, stderr.String())
		}
	})

	t.Run("usage: invalid flag value", func(t *testing.T) {
		homeFor(t)
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"--protocol", "ftp", "myorg"}, &stdout, &stderr)
		if code != exitUsage {
			t.Fatalf("run(--protocol ftp) = %d, want %d; stderr=%s", code, exitUsage, stderr.String())
		}
	})

	t.Run("runtime failure: gh missing from PATH", func(t *testing.T) {
		homeFor(t)
		t.Setenv("PATH", t.TempDir())
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"--root", t.TempDir(), "myorg"}, &stdout, &stderr)
		if code != exitRuntimeFail {
			t.Fatalf("run() = %d, want %d; stderr=%s", code, exitRuntimeFail, stderr.String())
		}
	})

	t.Run("interrupted: canceled context", func(t *testing.T) {
		homeFor(t)

		old := execDefault
		t.Cleanup(func() { execDefault = old })
		execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			if name == "gh" {
				return []byte("[]"), nil // an org with zero repos: nothing to clone/fetch/archive
			}
			return old(ctx, dir, name, args...)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already canceled before run() ever starts

		var stdout, stderr bytes.Buffer
		code := run(ctx, []string{"--root", t.TempDir(), "myorg"}, &stdout, &stderr)
		if code != exitInterrupted {
			t.Fatalf("run() with a pre-canceled context = %d, want %d; stdout=%s stderr=%s",
				code, exitInterrupted, stdout.String(), stderr.String())
		}
	})
}

// TestPinDashDashEndsFlagParsing exercises "--" through the real entry
// points (run and cmdWorktreeRemove), not the parseInterspersed helper
// directly (TestParseInterspersed already pins that unit in isolation, but
// parseInterspersed itself is expected to disappear in the cobra port). If
// "--" stopped being honored, an argument that looks like a flag would
// either be rejected by the flag parser ("flag provided but not defined")
// or silently consumed as a flag's value; both are distinguishable from the
// expected outcome below by the resulting error text.
func TestPinDashDashEndsFlagParsing(t *testing.T) {
	t.Run("top-level org", func(t *testing.T) {
		homeFor(t)
		var stdout, stderr bytes.Buffer
		// A single token after "--" doesn't discriminate: stdlib flag.Parse
		// itself already stops at a bare "--", so this alone would pass even
		// if the custom post-"--" loop in parseInterspersed were deleted
		// outright. Two tokens do discriminate: if "--" were not honored
		// past the first one, "-second" would be parsed as an unknown flag
		// instead of reaching the "too many positional args" check.
		code := run(context.Background(), []string{"--", "validorg", "-second"}, &stdout, &stderr)
		if code != exitUsage {
			t.Fatalf("run(-- validorg -second) = %d, want %d; stderr=%s", code, exitUsage, stderr.String())
		}
		if strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Fatalf("\"-second\" was parsed as a flag instead of a second positional arg: %s", stderr.String())
		}
		if !strings.Contains(stderr.String(), "expected exactly one org argument, got 2") {
			t.Fatalf("expected a too-many-positional-args error (proving both tokens after \"--\" were treated as positional), got: %s", stderr.String())
		}
	})

	t.Run("worktree remove path", func(t *testing.T) {
		homeFor(t)
		root := t.TempDir()
		var stdout, stderr bytes.Buffer
		// A path that looks like a flag must be treated as the positional
		// <path> argument, not rejected as an unknown flag.
		code := cmdWorktreeRemove(context.Background(), []string{"--root", root, "--", "myorg/repo1", "-weirdpath"}, &stdout, &stderr)
		if code != exitRuntimeFail {
			t.Fatalf("cmdWorktreeRemove = %d, want %d; stderr=%s", code, exitRuntimeFail, stderr.String())
		}
		if strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Fatalf("\"-weirdpath\" was parsed as a flag instead of the positional path: %s", stderr.String())
		}
		if !strings.Contains(stderr.String(), "no local clone") {
			t.Fatalf("expected a \"no local clone\" error (proving -weirdpath reached path resolution), got: %s", stderr.String())
		}
	})
}

// TestPinExplicitBoolFlagFalse pins "--flag=false" syntax for a boolean
// flag whose default is true. This is the one boolean flag (--archive)
// where the default isn't the zero value, so it's the only one where
// "=false" is distinguishable from "flag simply absent".
func TestPinExplicitBoolFlagFalse(t *testing.T) {
	homeFor(t)

	cfg, err := resolveConfig(newFlagSet(), []string{"--archive=false", "myorg"}, os.Stderr)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Archive {
		t.Fatalf("--archive=false did not clear cfg.Archive")
	}

	// And the default (flag absent) is still true.
	cfg, err = resolveConfig(newFlagSet(), []string{"myorg"}, os.Stderr)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if !cfg.Archive {
		t.Fatalf("default cfg.Archive changed from true")
	}
}

// TestPinArchiveFalseEndToEnd pins the --archive=false flag's actual
// effect, not just that it parses: an archived repo with --archive=false
// is planned as an ordinary clone, not an archive-and-delete.
func TestPinArchiveFalseEndToEnd(t *testing.T) {
	homeFor(t)

	repos := []ghRepo{{
		ID: "R_archived", Name: "archived", NameWithOwner: "myorg/archived",
		URL: "https://example.invalid/myorg/archived.git", SSHURL: "git@example.invalid:myorg/archived.git",
		IsArchived: true, PushedAt: time.Now(), ArchivedAt: time.Now(),
		DefaultBranch: &ghRefName{Name: "main"},
	}}
	reposJSON, err := json.Marshal(repos)
	if err != nil {
		t.Fatal(err)
	}

	old := execDefault
	t.Cleanup(func() { execDefault = old })
	execDefault = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return reposJSON, nil
	}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--root", t.TempDir(), "--dry-run", "--archive=false", "myorg"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("run() = %d, want %d; stderr=%s", code, exitSuccess, stderr.String())
	}
	if !strings.Contains(stdout.String(), "archived: clone (") {
		t.Fatalf("--archive=false should plan a clone for an archived repo, got:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "archived: archive (") {
		t.Fatalf("--archive=false must not plan an archive, got:\n%s", stdout.String())
	}
}
