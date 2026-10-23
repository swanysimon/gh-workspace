package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// completionWorkspace builds a root holding one real clone (acme/app, with
// a linked worktree and a "feature" branch on origin), a state file naming
// a repo that isn't cloned and one that is archived, an owner directory
// with nothing cloned, and a config file naming an owner and a repo that
// exist nowhere on disk. It returns the root and the worktree's path.
func completionWorkspace(t *testing.T) (root, worktree string) {
	t.Helper()
	homeFor(t)
	ctx := context.Background()

	origin := initTestRepo(t)
	if _, err := execCommand(ctx, origin, "git", "branch", "feature"); err != nil {
		t.Fatal(err)
	}

	root = t.TempDir()
	clone := filepath.Join(root, "acme", "repos", "app")
	if _, err := execCommand(ctx, "", "git", "clone", "--quiet", "file://"+origin, clone); err != nil {
		t.Fatal(err)
	}
	worktree = filepath.Join(t.TempDir(), "wt")
	if _, err := execCommand(ctx, clone, "git", "worktree", "add", "--quiet", worktree, "feature"); err != nil {
		t.Fatal(err)
	}
	worktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatal(err)
	}

	if err := saveState(filepath.Join(root, "acme", "state.json"), state{
		Version: stateVersion,
		Org:     "acme",
		Repos: map[string]repoState{
			"app": {Status: statusCloned},
			"lib": {Status: statusCloned},
			"old": {Status: statusArchived},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty", "repos"), 0o700); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	configJSON, err := json.Marshal(map[string]any{
		"owners": []map[string]string{{"name": "configured"}},
		"repos":  []string{"other/tool"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_WORKSPACE_ROOT", root)
	t.Setenv("GH_WORKSPACE_CONFIG", configPath)
	return root, worktree
}

func runComplete(t *testing.T, words ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), append([]string{"__complete"}, words...), &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("__complete %q = %d, want %d", words, code, exitSuccess)
	}
	if stderr.Len() != 0 {
		t.Fatalf("__complete %q wrote to stderr: %q", words, stderr.String())
	}
	return stdout.String()
}

func TestComplete(t *testing.T) {
	_, worktree := completionWorkspace(t)
	otherRoot := t.TempDir()

	cases := []struct {
		words []string
		want  string
	}{
		{[]string{""}, "sync\nclone\nuntrack\nworktree\ncompletion\n:4\n"},
		{[]string{"completion", ""}, "bash\nzsh\nfish\n:4\n"},
		{[]string{"completion", "bash", ""}, ":4\n"},
		{[]string{"w"}, "worktree\n:4\n"},
		{[]string{"nope", ""}, ":4\n"},
		{[]string{"worktree", ""}, "add\nremove\nlist\n:4\n"},
		{[]string{"worktree", "nope", ""}, ":4\n"},

		// Owners come from the root and the config; repos from clones, the
		// state file (never an archived one), and the config.
		{[]string{"worktree", "add", ""}, "acme/\nconfigured/\nempty/\nother/\n:6\n"},
		{[]string{"worktree", "add", "ac"}, "acme/\n:6\n"},
		{[]string{"worktree", "add", "acme/"}, "acme/app\nacme/lib\n:4\n"},
		{[]string{"worktree", "add", "other/"}, "other/tool\n:4\n"},
		{[]string{"worktree", "add", "nobody/"}, ":4\n"},
		{[]string{"worktree", "add", "acme/app", ""}, "feature\nmain\n:4\n"},
		{[]string{"worktree", "add", "acme/app", "f"}, "feature\n:4\n"},
		{[]string{"worktree", "add", "acme/lib", ""}, ":4\n"},
		{[]string{"worktree", "add", "acme/app", "main", ""}, ":16\n"},
		{[]string{"worktree", "add", "acme/app", "main", "path", ""}, ":4\n"},

		// remove and list act on clones, so only owners with one count.
		{[]string{"worktree", "remove", ""}, "acme/\n:6\n"},
		{[]string{"worktree", "remove", "acme/"}, "acme/app\n:4\n"},
		{[]string{"worktree", "remove", "./"}, ":0\n"},
		{[]string{"worktree", "remove", "acme/app", ""}, worktree + "\n:4\n"},
		{[]string{"worktree", "remove", "--force", "acme/app", ""}, worktree + "\n:4\n"},
		{[]string{"worktree", "remove", "./wt", ""}, ":4\n"},
		{[]string{"worktree", "list", ""}, "acme\nacme/\n:6\n"},
		{[]string{"worktree", "list", "acme", ""}, ":4\n"},

		{[]string{"sync", ""}, "acme\nacme/\nconfigured\nconfigured/\nempty\nempty/\nother\nother/\n:6\n"},
		{[]string{"sync", "acme", ""}, ":4\n"},
		{[]string{"clone", "acme/app", "acme/"}, "acme/app\nacme/lib\n:4\n"},
		{[]string{"untrack", "acme/"}, "acme/app\nacme/lib\n:4\n"},
		{[]string{"untrack", "acme/app", ""}, ":4\n"},

		// Flags: names on "-", the shell's own path completion for a
		// value still to come, and --root honored once given.
		{[]string{"worktree", "add", "--worktree-"}, "--worktree-path\n--worktree-root\n:4\n"},
		{[]string{"worktree", "add", "--root", ""}, ":0\n"},
		{[]string{"worktree", "add", "--root", otherRoot, ""}, "configured/\nother/\n:6\n"},
		{[]string{"worktree", "add", "--root=" + otherRoot, ""}, "configured/\nother/\n:6\n"},
		{[]string{"worktree", "remove", "--force", ""}, "acme/\n:6\n"},
		{[]string{"worktree", "add", "--vcs", "svn", ""}, ":1\n"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.words, " "), func(t *testing.T) {
			if got := runComplete(t, tc.words...); got != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestCompleteFlagsMatchCommands guards completionSpec against drifting
// from the commands themselves: a flag a command gains (or a boolean one
// completion doesn't know about, which would swallow the next positional
// as its value) shows up here.
func TestCompleteFlagsMatchCommands(t *testing.T) {
	homeFor(t)
	flagPattern := regexp.MustCompile(`(?m)^\s+(?:-\w, )?(--[\w-]+)`)

	for _, cmd := range [][]string{
		{"sync"}, {"clone"}, {"untrack"},
		{"worktree", "add"}, {"worktree", "remove"}, {"worktree", "list"},
	} {
		t.Run(strings.Join(cmd, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			Run(context.Background(), append(slices.Clone(cmd), "--help"), &stdout, &stderr)
			var want []string
			for _, m := range flagPattern.FindAllStringSubmatch(stderr.String(), -1) {
				want = append(want, m[1])
			}
			want = append(want, "--help")
			slices.Sort(want)

			got := strings.Fields(runComplete(t, append(slices.Clone(cmd), "--")...))
			got = got[:len(got)-1]
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("completed flags %v, command accepts %v", got, want)
			}
		})
	}
}
