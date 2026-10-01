package main

import (
	"reflect"
	"testing"

	"github.com/spf13/pflag"
)

// TestSettingsTableConfigKeysMatchFileConfig cross-checks each setting's
// configKey field against fileConfig's own json tags, so configKey (which
// nothing else in settings.go reads -- see its doc comment, "documentation
// only") can't silently drift from the struct loadFileConfig actually
// decodes into. A setting whose configKey doesn't name a real fileConfig
// field, or whose fileValue closure reads a different field than configKey
// claims, is a real bug this test is meant to catch.
func TestSettingsTableConfigKeysMatchFileConfig(t *testing.T) {
	fcType := reflect.TypeOf(fileConfig{})
	jsonTagToField := map[string]string{}
	for i := 0; i < fcType.NumField(); i++ {
		f := fcType.Field(i)
		tag := f.Tag.Get("json")
		// fileConfig's tags are plain names with no ",omitempty" etc., but
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
			t.Errorf("setting %q: configKey %q names no json tag on fileConfig", s.flagName, s.configKey)
		}
	}

	// And the reverse: every fileConfig field must be claimed by exactly
	// one setting's configKey, so a new config-file key can't be added to
	// fileConfig without a matching settingsTable entry (which would make
	// it decode successfully but silently do nothing).
	claimed := map[string]bool{}
	for _, s := range settingsTable {
		if s.configKey != "" {
			claimed[s.configKey] = true
		}
	}
	for tag := range jsonTagToField {
		if !claimed[tag] {
			t.Errorf("fileConfig json tag %q has no settingsTable entry claiming it", tag)
		}
	}
}

// TestSettingsFor checks the table-driven claim this whole file rests on:
// which commands accept which settings, matching the sync/worktree surface
// documented in AIDEV.md and the README.
func TestSettingsFor(t *testing.T) {
	syncOnly := []string{"concurrency", "max-repos", "include-forks", "archive", "force", "dry-run", "verbose", "yes"}
	shared := []string{"root", "timeout", "protocol"}

	syncNames := flagNames(settingsFor(cmdSync))
	worktreeNames := flagNames(settingsFor(cmdWorktree))

	for _, name := range shared {
		if !syncNames[name] {
			t.Errorf("cmdSync is missing shared setting %q", name)
		}
		if !worktreeNames[name] {
			t.Errorf("cmdWorktree is missing shared setting %q", name)
		}
	}
	for _, name := range syncOnly {
		if !syncNames[name] {
			t.Errorf("cmdSync is missing sync-only setting %q", name)
		}
		if worktreeNames[name] {
			t.Errorf("cmdWorktree unexpectedly accepts sync-only setting %q", name)
		}
	}
	if got, want := len(syncNames), len(syncOnly)+len(shared); got != want {
		t.Errorf("cmdSync has %d settings, want %d (unexpected extra entries)", got, want)
	}
	if got, want := len(worktreeNames), len(shared); got != want {
		t.Errorf("cmdWorktree has %d settings, want %d (unexpected extra entries)", got, want)
	}
}

func flagNames(settings []setting) map[string]bool {
	out := make(map[string]bool, len(settings))
	for _, s := range settings {
		out[s.flagName] = true
	}
	return out
}

// TestBindSettingsRegistersExactlyDeclaredFlags drives bindSettings itself
// (not just settingsFor's data), so a bug in bindSettings's switch over
// settingKind -- e.g. forgetting a case, or registering a flag under the
// wrong command -- would show up here even if settingsFor's own data is
// correct.
func TestBindSettingsRegistersExactlyDeclaredFlags(t *testing.T) {
	for _, cmd := range []commandID{cmdSync, cmdWorktree} {
		fs, help := newFlagSet(string(cmd))
		bindSettings(fs, cmd)

		want := flagNames(settingsFor(cmd))
		want["help"] = true // registered by newFlagSet, not the table

		got := map[string]bool{}
		fs.VisitAll(func(f *pflag.Flag) { got[f.Name] = true })

		for name := range want {
			if !got[name] {
				t.Errorf("%s: bindSettings did not register --%s", cmd, name)
			}
		}
		for name := range got {
			if !want[name] {
				t.Errorf("%s: bindSettings registered unexpected flag --%s", cmd, name)
			}
		}
		if help == nil || *help {
			t.Errorf("%s: help flag should default to false", cmd)
		}
	}
}
