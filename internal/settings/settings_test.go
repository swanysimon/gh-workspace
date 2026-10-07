package settings

import (
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
	for tag := range jsonTagToField {
		if !claimed[tag] {
			t.Errorf("FileConfig json tag %q has no settingsTable entry claiming it", tag)
		}
	}
}

// TestSettingsFor checks the table-driven claim this whole package rests
// on: which commands accept which settings, matching the sync/worktree
// surface documented in AIDEV.md and the README.
func TestSettingsFor(t *testing.T) {
	syncOnly := []string{"concurrency", "max-repos", "include-forks", "dry-run", "verbose", "yes"}
	syncAndClone := []string{"archive", "force"}
	shared := []string{"root", "timeout", "protocol"}

	syncNames := flagNames(SettingsFor(CmdSync))
	worktreeNames := flagNames(SettingsFor(CmdWorktree))
	cloneNames := flagNames(SettingsFor(CmdClone))

	for _, name := range shared {
		if !syncNames[name] {
			t.Errorf("CmdSync is missing shared setting %q", name)
		}
		if !worktreeNames[name] {
			t.Errorf("CmdWorktree is missing shared setting %q", name)
		}
		if !cloneNames[name] {
			t.Errorf("CmdClone is missing shared setting %q", name)
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
	for _, cmd := range []CommandID{CmdSync, CmdWorktree, CmdClone} {
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
