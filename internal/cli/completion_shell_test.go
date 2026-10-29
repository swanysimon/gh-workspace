package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The completion scripts only work against gh's own completer, so these
// tests drive real shells through it: a "gh" on PATH that hands
// "gh workspace ..." to this test binary (see TestMain) and everything
// else to the real gh, whose generated completion scripts are what the
// wrappers hook into. A test skips when its shell or gh is missing.

const runCLIEnv = "GH_WORKSPACE_TEST_RUN_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(runCLIEnv) == "1" {
		os.Exit(Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// completionShellEnv puts the fake gh first on PATH, over the fixture from
// completionWorkspace, and returns a temp dir the shell's own setup can use.
func completionShellEnv(t *testing.T) (worktree, dir string) {
	t.Helper()
	realGh, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("gh not installed")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, worktree = completionWorkspace(t)

	dir = t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeGh := "#!/bin/sh\n" +
		"if [ \"$1\" = workspace ]; then shift; " + runCLIEnv + "=1 exec '" + self + "' \"$@\"; fi\n" +
		"exec '" + realGh + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGh), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return worktree, dir
}

func writeCompletionScript(t *testing.T, dir, shell string) string {
	t.Helper()
	script, err := completionScripts.ReadFile("completions/gh-workspace." + shell)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gh-workspace."+shell)
	if err := os.WriteFile(path, script, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runShell(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
	return string(out)
}

// completionCases are command lines (a trailing space means "complete a
// new word") and what the shell should complete them to. gh's own
// commands are in the list to show they still complete through the
// wrapper.
func completionCases(worktree string) [][2]string {
	return [][2]string{
		{"gh workspace worktree add ac", "acme/"},
		{"gh workspace worktree add acme/a", "acme/app"},
		{"gh workspace worktree add acme/app f", "feature"},
		{"gh workspace worktree remove acme/app ", worktree},
		{"gh workspace comp", "completion"},
		{"gh pr chec", "checkout checks"},
	}
}

func TestCompletionScriptBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	var bashCompletion string
	for _, p := range []string{
		"/usr/share/bash-completion/bash_completion",
		"/opt/homebrew/etc/profile.d/bash_completion.sh",
		"/usr/local/etc/profile.d/bash_completion.sh",
	} {
		if _, err := os.Stat(p); err == nil {
			bashCompletion = p
			break
		}
	}
	if bashCompletion == "" {
		t.Skip("bash-completion not installed")
	}
	worktree, dir := completionShellEnv(t)
	script := writeCompletionScript(t, dir, "bash")

	// bash-completion's lazy loader finds gh's completer here.
	lazyDir := filepath.Join(dir, "lazy")
	if err := os.MkdirAll(filepath.Join(lazyDir, "completions"), 0o700); err != nil {
		t.Fatal(err)
	}
	ghCompletion := runShell(t, "gh", "completion", "-s", "bash")
	if err := os.WriteFile(filepath.Join(lazyDir, "completions", "gh"), []byte(ghCompletion), 0o600); err != nil {
		t.Fatal(err)
	}

	// compopt only works inside a real completion, and only sets display
	// options, never the candidates.
	harness := `
run() {
    read -r -a COMP_WORDS <<< "$1"
    [[ $1 == *" " ]] && COMP_WORDS+=("")
    COMP_CWORD=$(( ${#COMP_WORDS[@]} - 1 )); COMP_LINE=$1; COMP_POINT=${#COMP_LINE}
    COMPREPLY=()
    __start_gh
    printf '%s\n' "${COMPREPLY[*]}"
}
compopt() { :; }
`
	// Another extension wrapping the same hook the same way, after ours.
	otherWrapper := `hook=$(declare -f __gh_get_completion_results); ` +
		`eval "__gh_other_prev${hook#__gh_get_completion_results}"; ` +
		`__gh_get_completion_results() { __gh_other_prev "$@"; }`
	setups := map[string]string{
		"gh loaded first":  `eval "$(gh completion -s bash)"; source ` + script,
		"gh lazily loaded": `BASH_COMPLETION_USER_DIR=` + lazyDir + `; source ` + script,
		"gh not loaded":    `source ` + script,
		"sourced twice":    `eval "$(gh completion -s bash)"; source ` + script + `; source ` + script,
		"gh reloaded":      `source ` + script + `; eval "$(gh completion -s bash)"; source ` + script,
		"wrapped again":    `source ` + script + "; " + otherWrapper + "; source " + script,
	}
	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			cases := completionCases(worktree)
			var calls []string
			for _, c := range cases {
				calls = append(calls, "run '"+c[0]+"'")
			}
			out := runShell(t, bash, "--norc", "--noprofile", "-c",
				"source "+bashCompletion+"\n"+harness+setup+"\n"+strings.Join(calls, "\n"))
			checkCompletions(t, cases, out)
		})
	}

	t.Run("without bash-completion", func(t *testing.T) {
		out := runShell(t, bash, "--norc", "--noprofile", "-c",
			"source "+script+"; complete -p gh >/dev/null 2>&1 && echo registered || echo none")
		if strings.TrimSpace(out) != "none" {
			t.Fatalf("got %q: without bash-completion, gh's completer fails on every Tab, so nothing should be registered", out)
		}
	})
}

func TestCompletionScriptZsh(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed")
	}
	worktree, dir := completionShellEnv(t)
	script := writeCompletionScript(t, dir, "zsh")

	// compinit autoloads _gh from fpath, the usual way gh's completer is
	// installed.
	fpathDir := filepath.Join(dir, "fpath")
	if err := os.Mkdir(fpathDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ghCompletion := runShell(t, "gh", "completion", "-s", "zsh")
	if err := os.WriteFile(filepath.Join(fpathDir, "_gh"), []byte(ghCompletion), 0o600); err != nil {
		t.Fatal(err)
	}

	// zsh only completes inside its line editor, so this drives an
	// interactive zsh on a pseudo-terminal: type the line, run
	// complete-word, and print what the buffer became. The marker is
	// split in the source so the terminal's echo of it never matches.
	driver := `
zmodload zsh/zpty
zpty z "zsh -f -i"
zpty -w z "PS1=''; fpath=(` + fpathDir + ` \$fpath); autoload -U compinit; compinit -u -D; $SETUP"
zpty -w z "comp-out() { zle complete-word; print -r -- \"R\"\"ESULT[\$BUFFER]\" > /dev/tty; BUFFER=''; }; zle -N comp-out; bindkey '^T' comp-out"
for l in "$@"; do
  zpty -w -n z "$l"$'\x14'
  out=''
  for i in {1..400}; do
    zpty -r -t z line && out+=$line
    [[ $out == *'RESULT['*']'* ]] && break
    sleep 0.05
  done
  if [[ $out != *'RESULT['*']'* ]]; then
    print -r -- TIMEOUT
    continue
  fi
  out=${out##*RESULT\[}
  print -r -- ${out%%\]*}
done
zpty -d z
`
	driverPath := filepath.Join(dir, "driver.zsh")
	if err := os.WriteFile(driverPath, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}

	setups := map[string]string{
		"gh autoloaded":   "source " + script,
		"gh loaded first": `eval "$(gh completion -s zsh)"; source ` + script,
		"sourced twice":   "source " + script + "; source " + script,
		"gh reloaded":     "source " + script + `; eval "$(gh completion -s zsh)"; source ` + script,
	}
	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			cases := completionCases(worktree)
			args := []string{"-f", driverPath}
			for _, c := range cases {
				args = append(args, c[0])
			}
			t.Setenv("SETUP", setup)
			out := runShell(t, zsh, args...)
			// zsh completes in place: the buffer becomes the line plus the
			// rest of the one candidate, or the common prefix of several.
			var lines []string
			for i, got := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
				lines = append(lines, strings.TrimSpace(strings.TrimPrefix(got, wordsBefore(cases[i][0]))))
			}
			zshCases := slicesWith(cases, len(cases)-1, [2]string{cases[len(cases)-1][0], "check"})
			checkCompletions(t, zshCases, strings.Join(lines, "\n")+"\n")
		})
	}
}

func TestCompletionScriptFish(t *testing.T) {
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish not installed")
	}
	worktree, dir := completionShellEnv(t)
	script := writeCompletionScript(t, dir, "fish")

	// fish autoloads gh's completer from fish_complete_path.
	completeDir := filepath.Join(dir, "completions")
	if err := os.Mkdir(completeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ghCompletion := runShell(t, "gh", "completion", "-s", "fish")
	if err := os.WriteFile(filepath.Join(completeDir, "gh.fish"), []byte(ghCompletion), 0o600); err != nil {
		t.Fatal(err)
	}

	setups := map[string]string{
		"gh autoloaded":   "set -p fish_complete_path " + completeDir + "; source " + script,
		"gh not loaded":   "source " + script,
		"sourced twice":   "source " + script + "; source " + script,
		"gh reloaded":     "source " + script + "; gh completion -s fish | source; source " + script,
		"gh loaded first": "gh completion -s fish | source; source " + script,
	}
	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			cases := completionCases(worktree)
			var calls []string
			for _, c := range cases {
				calls = append(calls, "complete --do-complete '"+c[0]+"' | string replace -r '\\t.*' '' | string join ' '")
			}
			out := runShell(t, fish, "--no-config", "-c", setup+"\n"+strings.Join(calls, "\n"))
			checkCompletions(t, cases, out)
		})
	}
}

func checkCompletions(t *testing.T, cases [][2]string, out string) {
	t.Helper()
	results := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(results) != len(cases) {
		t.Fatalf("got %d results for %d lines:\n%s", len(results), len(cases), out)
	}
	for i, c := range cases {
		if strings.TrimSpace(got[i]) != c[1] {
			t.Errorf("%q completed to %q, want %q", c[0], got[i], c[1])
		}
	}
}

// wordsBefore is line up to and including its last space: what zsh leaves
// alone when it completes the word after it.
func wordsBefore(line string) string {
	return line[:strings.LastIndex(line, " ")+1]
}

func slicesWith[T any](s []T, i int, v T) []T {
	out := append([]T(nil), s...)
	out[i] = v
	return out
}
