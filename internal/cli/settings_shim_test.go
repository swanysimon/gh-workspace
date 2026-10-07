package cli

import (
	"testing"

	"github.com/spf13/pflag"
)

// This file tests settings.go's shims directly (not internal/settings' own
// logic, which internal/settings/settings_test.go covers), confirming the
// root-level integration points -- newFlagSet registering "help" alongside
// whatever bindSettings adds, and resolveSettings/validateConfig correctly
// reaching cfg's embedded settings.Settings -- actually work end to end.

// TestNewFlagSetPlusBindSettingsRegistersHelpAndSettings is the
// integration-level counterpart to internal/settings'
// TestBindSettingsRegistersExactlyDeclaredFlags, which deliberately does
// not cover "help" since registering it is this package's job
// (newFlagSet), not BindSettings'.
func TestNewFlagSetPlusBindSettingsRegistersHelpAndSettings(t *testing.T) {
	for _, cmd := range []commandID{cmdSync, cmdWorktree} {
		fs, help := newFlagSet(string(cmd))
		bindSettings(fs, cmd)

		got := map[string]bool{}
		fs.VisitAll(func(f *pflag.Flag) { got[f.Name] = true })

		if !got["help"] {
			t.Errorf("%s: newFlagSet did not register --help", cmd)
		}
		if help == nil || *help {
			t.Errorf("%s: help flag should default to false", cmd)
		}
	}
}

// TestResolveSettingsShimReachesEmbeddedSettings confirms resolveSettings's
// shim (settings.ResolveSettings(&cfg.Settings, ...)) actually mutates
// cfg's embedded settings.Settings, not some other copy -- the one way this
// shim's single line of real logic (&cfg.Settings) could silently do
// nothing is if a caller passed cfg by value somewhere upstream, which
// would make this test fail by leaving cfg.Concurrency at its default.
func TestResolveSettingsShimReachesEmbeddedSettings(t *testing.T) {
	fs, _ := newFlagSet("test")
	bound := bindSettings(fs, cmdSync)
	if err := fs.Parse([]string{"--concurrency", "42"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cfg := defaultConfig()
	if err := resolveSettings(&cfg, cmdSync, fs, bound, nil); err != nil {
		t.Fatalf("resolveSettings: %v", err)
	}
	if cfg.Concurrency != 42 {
		t.Fatalf("cfg.Concurrency = %d, want 42 (resolveSettings did not reach the embedded Settings)", cfg.Concurrency)
	}
}

// TestValidateConfigChecksOrgThenSettings confirms validateConfig's shim
// actually checks both halves: the Org field it owns directly, and
// settings.Validate(cfg.Settings) for everything else.
func TestValidateConfigChecksOrgThenSettings(t *testing.T) {
	cfg := defaultConfig()
	cfg.Org = ""
	if err := validateConfig(cfg); err == nil {
		t.Fatalf("expected an error for an empty org")
	}

	cfg = defaultConfig()
	cfg.Org = "myorg"
	cfg.Protocol = "ftp"
	if err := validateConfig(cfg); err == nil {
		t.Fatalf("expected an error for an invalid protocol (settings.Validate not reached)")
	}

	cfg = defaultConfig()
	cfg.Org = "myorg"
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("valid org and settings should not error: %v", err)
	}
}
