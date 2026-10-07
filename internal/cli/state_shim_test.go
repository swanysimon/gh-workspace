package cli

import (
	"testing"
)

// This file tests state.go's shims directly (not internal/store's own
// logic, which internal/store/store_test.go covers), confirming cfg.Root
// and cfg.Org actually reach the computed paths -- the same category of
// gap an earlier review found and fixed for gh.go's shims (see
// gh_shim_test.go and AIDEV.md's ghcli entry). orgDir/reposDir/
// archivesDir/statePath/lockPath are the one place in this file with real
// logic of their own (joining cfg.Root and cfg.Org); acquireLock/
// loadState/saveState/validRepoName are pure forwards with no risk of a
// "right value, wrong field" mix-up.
func TestPathShimsForwardRootAndOrg(t *testing.T) {
	cfg := defaultConfig()
	cfg.Root = "/shimroot"
	cfg.Org = "shimorg"

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"orgDir", orgDir(cfg), "/shimroot/shimorg"},
		{"reposDir", reposDir(cfg), "/shimroot/shimorg/repos"},
		{"archivesDir", archivesDir(cfg), "/shimroot/shimorg/archives"},
		{"statePath", statePath(cfg), "/shimroot/shimorg/state.json"},
		{"lockPath", lockPath(cfg), "/shimroot/shimorg/lock"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestPathShimsUseDifferentOrgsAndRoots(t *testing.T) {
	cfgA := defaultConfig()
	cfgA.Root = "/rootA"
	cfgA.Org = "orgA"

	cfgB := defaultConfig()
	cfgB.Root = "/rootB"
	cfgB.Org = "orgB"

	if orgDir(cfgA) == orgDir(cfgB) {
		t.Fatalf("different root/org should produce different orgDir, both got %q", orgDir(cfgA))
	}
}
