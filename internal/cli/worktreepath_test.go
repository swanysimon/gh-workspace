package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandWorktreePathTemplate(t *testing.T) {
	cases := []struct {
		name     string
		template string
		owner    string
		repo     string
		branch   string
		want     string
	}{
		{"default template", "{repo}", "my-org", "my-repo", "feat-x", "my-repo"},
		{"all placeholders", "{owner}/{repo}/{branch}", "my-org", "my-repo", "feat-x", "my-org/my-repo/feat-x"},
		{"repeated placeholder", "{repo}-{repo}", "my-org", "my-repo", "feat-x", "my-repo-my-repo"},
		{"no placeholders", "fixed", "my-org", "my-repo", "feat-x", "fixed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandWorktreePathTemplate(tc.template, tc.owner, tc.repo, tc.branch)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveWorktreePathDefaultsToCurrentDirectoryAndRepoName(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	// WorktreeRoot left at its zero value ("") -- the current directory.
	got, err := resolveWorktreePath(cfg, "my-org", "my-repo", "feat/x")
	if err != nil {
		t.Fatalf("resolveWorktreePath: %v", err)
	}
	want := filepath.Join(wd, "my-repo")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "my-org") {
		t.Errorf("default template must not include the owner: %q", got)
	}
}

func TestResolveWorktreePathBranchSlashSanitized(t *testing.T) {
	cfg := defaultConfig()
	cfg.WorktreeRoot = t.TempDir()
	cfg.WorktreePath = "{repo}/{branch}"
	got, err := resolveWorktreePath(cfg, "my-org", "my-repo", "feat/x/y")
	if err != nil {
		t.Fatalf("resolveWorktreePath: %v", err)
	}
	want := filepath.Join(cfg.WorktreeRoot, "my-repo", "feat-x-y")
	if got != want {
		t.Errorf("got %q, want %q (branch slashes must become dashes)", got, want)
	}
}

func TestResolveWorktreePathRejectsEscapingTemplate(t *testing.T) {
	cfg := defaultConfig()
	cfg.WorktreeRoot = t.TempDir()
	cfg.WorktreePath = "../{repo}"
	if _, err := resolveWorktreePath(cfg, "my-org", "my-repo", "main"); err == nil {
		t.Fatal("expected an error escaping worktree-root")
	}
}

func TestResolveWorktreePathConfiguredTemplateWithOwner(t *testing.T) {
	cfg := defaultConfig()
	cfg.WorktreeRoot = t.TempDir()
	cfg.WorktreePath = "{owner}/{repo}/{branch}"
	got, err := resolveWorktreePath(cfg, "my-org", "my-repo", "feat/x")
	if err != nil {
		t.Fatalf("resolveWorktreePath: %v", err)
	}
	want := filepath.Join(cfg.WorktreeRoot, "my-org", "my-repo", "feat-x")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
