package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spf13/pflag"
)

// TestSettingsTableConfigKeysMatchFileConfig cross-checks each setting's
// configKey field against FileConfig's own json tags, so configKey (which
// nothing else in this package reads -- see Setting's doc comment,
// "documentation only") can't silently drift from the struct
// LoadFileConfig actually decodes into. A setting whose configKey doesn't
// name a real FileConfig field, or whose fileValue closure reads a
// different field than configKey claims, is a real bug this test is meant
// to catch.
func TestSettingsTableConfigKeysMatchFileConfig(t *testing.T) {
	fcType := reflect.TypeOf(FileConfig{})
	jsonTagToField := map[string]string{}
	for i := 0; i < fcType.NumField(); i++ {
		f := fcType.Field(i)
		tag := f.Tag.Get("json")
		// FileConfig's tags are plain names with no ",omitempty" etc., but
		// strip a trailing option list defensively rather than assume that
		// forever.
		for i, c := range tag {
			if c == ',' {
				tag = tag[:i]
				break
			}
		}
		jsonTagToField[tag] = f.Name
	}

	for _, s := range settingsTable {
		if s.configKey == "" {
			if s.fileValue != nil {
				t.Errorf("setting %q has a fileValue but no configKey", s.flagName)
			}
			continue
		}
		if s.fileValue == nil {
			t.Errorf("setting %q has configKey %q but no fileValue", s.flagName, s.configKey)
			continue
		}
		if _, ok := jsonTagToField[s.configKey]; !ok {
			t.Errorf("setting %q: configKey %q names no json tag on FileConfig", s.flagName, s.configKey)
		}
	}

	// And the reverse: every FileConfig field must be claimed by exactly
	// one setting's configKey, so a new config-file key can't be added to
	// FileConfig without a matching settingsTable entry (which would make
	// it decode successfully but silently do nothing).
	claimed := map[string]bool{}
	for _, s := range settingsTable {
		if s.configKey != "" {
			claimed[s.configKey] = true
		}
	}
	// "owners" and "repos" are deliberately exempt: unlike every other
	// FileConfig field, they're structural (lists), have no flag/env
	// equivalent, and aren't resolved through settingsTable at all --
	// see FileConfig's doc comment.
	exempt := map[string]bool{"owners": true, "repos": true}
	for tag := range jsonTagToField {
		if exempt[tag] {
			continue
		}
		if !claimed[tag] {
			t.Errorf("FileConfig json tag %q has no settingsTable entry claiming it", tag)
		}
	}
}

// TestSettingsFor checks the table-driven claim this whole package rests
// on: which commands accept which settings, matching the sync/worktree
// surface documented in AIDEV.md and the README.
func TestSettingsFor(t *testing.T) {
	syncOnly := []string{"concurrency", "max-repos", "include-forks", "dry-run", "verbose", "yes", "tracked-only"}
	syncAndClone := []string{"archive", "force"}
	shared := []string{"root", "timeout", "protocol"}
	worktreeOnly := []string{"worktree-root", "worktree-path"}

	syncNames := flagNames(SettingsFor(CmdSync))
	worktreeNames := flagNames(SettingsFor(CmdWorktree))
	worktreeAddNames := flagNames(SettingsFor(CmdWorktreeAdd))
	cloneNames := flagNames(SettingsFor(CmdClone))

	for _, name := range shared {
		if !syncNames[name] {
			t.Errorf("CmdSync is missing shared setting %q", name)
		}
		if !worktreeNames[name] {
			t.Errorf("CmdWorktree is missing shared setting %q", name)
		}
		if !worktreeAddNames[name] {
			t.Errorf("CmdWorktreeAdd is missing shared setting %q", name)
		}
		if !cloneNames[name] {
			t.Errorf("CmdClone is missing shared setting %q", name)
		}
	}
	// worktreeOnly is CmdWorktreeAdd-only, not CmdWorktree's: "worktree
	// remove"/"worktree list" (which share CmdWorktree with "worktree add"
	// for root/timeout/protocol) have no use for a default-path template
	// at all, so they must not get these two flags, unlike "worktree add"
	// itself, which gets its own narrower CmdWorktreeAdd on top of
	// CmdWorktree's shared settings.
	for _, name := range worktreeOnly {
		if !worktreeAddNames[name] {
			t.Errorf("CmdWorktreeAdd is missing worktree-only setting %q", name)
		}
		if worktreeNames[name] {
			t.Errorf("CmdWorktree unexpectedly accepts worktree-only setting %q", name)
		}
		if syncNames[name] {
			t.Errorf("CmdSync unexpectedly accepts worktree-only setting %q", name)
		}
		if cloneNames[name] {
			t.Errorf("CmdClone unexpectedly accepts worktree-only setting %q", name)
		}
	}
	for _, name := range syncAndClone {
		if !syncNames[name] {
			t.Errorf("CmdSync is missing setting %q", name)
		}
		if !cloneNames[name] {
			t.Errorf("CmdClone is missing setting %q", name)
		}
		if worktreeNames[name] {
			t.Errorf("CmdWorktree unexpectedly accepts setting %q", name)
		}
	}
	for _, name := range syncOnly {
		if !syncNames[name] {
			t.Errorf("CmdSync is missing sync-only setting %q", name)
		}
		if worktreeNames[name] {
			t.Errorf("CmdWorktree unexpectedly accepts sync-only setting %q", name)
		}
		if cloneNames[name] {
			t.Errorf("CmdClone unexpectedly accepts sync-only setting %q", name)
		}
	}
	if got, want := len(syncNames), len(syncOnly)+len(syncAndClone)+len(shared); got != want {
		t.Errorf("CmdSync has %d settings, want %d (unexpected extra entries)", got, want)
	}
	if got, want := len(worktreeNames), len(shared); got != want {
		t.Errorf("CmdWorktree has %d settings, want %d (unexpected extra entries)", got, want)
	}
	if got, want := len(worktreeAddNames), len(shared)+len(worktreeOnly); got != want {
		t.Errorf("CmdWorktreeAdd has %d settings, want %d (unexpected extra entries)", got, want)
	}
	if got, want := len(cloneNames), len(syncAndClone)+len(shared); got != want {
		t.Errorf("CmdClone has %d settings, want %d (unexpected extra entries)", got, want)
	}
}

func flagNames(settings []Setting) map[string]bool {
	out := make(map[string]bool, len(settings))
	for _, s := range settings {
		out[s.flagName] = true
	}
	return out
}

// TestBindSettingsRegistersExactlyDeclaredFlags drives BindSettings itself
// (not just SettingsFor's data), so a bug in BindSettings's switch over
// settingKind -- e.g. forgetting a case, or registering a flag under the
// wrong command -- would show up here even if SettingsFor's own data is
// correct. This does not cover "help", since registering --help is the
// caller's job (gh.go's newFlagSet), not BindSettings'; see
// settings_shim_test.go at the repo root for that integration-level check.
func TestBindSettingsRegistersExactlyDeclaredFlags(t *testing.T) {
	for _, cmd := range []CommandID{CmdSync, CmdWorktree, CmdClone, CmdWorktreeAdd} {
		fs := pflag.NewFlagSet(string(cmd), pflag.ContinueOnError)
		BindSettings(fs, cmd)

		want := flagNames(SettingsFor(cmd))

		got := map[string]bool{}
		fs.VisitAll(func(f *pflag.Flag) { got[f.Name] = true })

		for name := range want {
			if !got[name] {
				t.Errorf("%s: BindSettings did not register --%s", cmd, name)
			}
		}
		for name := range got {
			if !want[name] {
				t.Errorf("%s: BindSettings registered unexpected flag --%s", cmd, name)
			}
		}
	}
}

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFileConfigAcceptsValidOwnersAndRepos(t *testing.T) {
	path := writeConfigFile(t, `{
		"owners": [
			{"name": "my-org"},
			{"name": "other-org", "includeForks": true, "archive": false}
		],
		"repos": ["someone/useful-lib", "cli/cli"]
	}`)
	fc, err := LoadFileConfig(path)
	if err != nil {
		t.Fatalf("LoadFileConfig: %v", err)
	}
	if len(fc.Owners) != 2 || fc.Owners[0].Name != "my-org" || fc.Owners[1].Name != "other-org" {
		t.Fatalf("Owners = %+v", fc.Owners)
	}
	if fc.Owners[1].IncludeForks == nil || !*fc.Owners[1].IncludeForks {
		t.Fatalf("Owners[1].IncludeForks = %v, want true", fc.Owners[1].IncludeForks)
	}
	if fc.Owners[1].Archive == nil || *fc.Owners[1].Archive {
		t.Fatalf("Owners[1].Archive = %v, want false", fc.Owners[1].Archive)
	}
	if len(fc.Repos) != 2 || fc.Repos[0] != "someone/useful-lib" || fc.Repos[1] != "cli/cli" {
		t.Fatalf("Repos = %+v", fc.Repos)
	}
}

func TestLoadFileConfigRejectsInvalidOwnersAndRepos(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"invalid owner name", `{"owners": [{"name": "not valid!"}]}`},
		{"duplicate owner", `{"owners": [{"name": "my-org"}, {"name": "my-org"}]}`},
		{"repo missing slash", `{"repos": ["noslash"]}`},
		{"repo empty owner", `{"repos": ["/repo"]}`},
		{"repo empty name", `{"repos": ["owner/"]}`},
		{"repo invalid owner", `{"repos": ["not valid!/repo"]}`},
		{"repo invalid name", `{"repos": ["owner/not valid!"]}`},
		{"duplicate repo", `{"repos": ["owner/repo", "owner/repo"]}`},
		{"unknown owner key", `{"owners": [{"name": "my-org", "bogus": true}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFile(t, tc.body)
			if _, err := LoadFileConfig(path); err == nil {
				t.Fatalf("LoadFileConfig(%s): want error, got nil", tc.body)
			}
		})
	}
}

func TestValidOwnerName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"my-org", true},
		{"a", true},
		{"my-org-123", true},
		{"", false},
		{"-leading-dash", false},
		{"has space", false},
		{"has/slash", false},
	}
	for _, tc := range cases {
		if got := ValidOwnerName(tc.name); got != tc.want {
			t.Errorf("ValidOwnerName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory available in this environment")
	}

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"tilde slash expands", "~/src/.workspace", filepath.Join(home, "src/.workspace")},
		{"bare tilde unchanged", "~", "~"},
		{"other user's home unchanged", "~someone/path", "~someone/path"},
		{"dollar HOME unchanged", "$HOME/path", "$HOME/path"},
		{"relative path unchanged", "relative/path", "relative/path"},
		{"already absolute unchanged", "/already/absolute", "/already/absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandHome(tc.input)
			if err != nil {
				t.Fatalf("ExpandHome(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("ExpandHome(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestApplyOwnerOverrides(t *testing.T) {
	base := Settings{IncludeForks: false, Archive: true, MaxRepos: 10000, Root: "/unrelated"}

	t.Run("no overrides set leaves base unchanged", func(t *testing.T) {
		got := ApplyOwnerOverrides(base, OwnerConfig{Name: "x"})
		if got != base {
			t.Errorf("got %+v, want unchanged %+v", got, base)
		}
	})

	t.Run("each override field replaces its matching Settings field", func(t *testing.T) {
		includeForks := true
		archive := false
		maxRepos := 5
		got := ApplyOwnerOverrides(base, OwnerConfig{Name: "x", IncludeForks: &includeForks, Archive: &archive, MaxRepos: &maxRepos})
		if got.IncludeForks != true || got.Archive != false || got.MaxRepos != 5 {
			t.Errorf("got %+v, want overrides applied", got)
		}
		if got.Root != base.Root {
			t.Errorf("Root should be untouched: got %q", got.Root)
		}
	})
}
