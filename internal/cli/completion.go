package cli

import (
	"embed"
	"fmt"
	"io"
	"slices"
)

//go:embed completions
var completionScripts embed.FS

var completionShells = []string{"bash", "zsh", "fish"}

const completionUsage = "gh workspace completion <bash|zsh|fish>"

func printCompletionUsage(w io.Writer) {
	fmt.Fprintln(w, "Print a script that makes Tab complete gh workspace's commands, orgs, repos,")
	fmt.Fprintln(w, "branches and worktrees.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "USAGE")
	fmt.Fprintln(w, "  "+completionUsage)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "gh completes extension names but nothing after them, so the script wraps gh's")
	fmt.Fprintln(w, "own completion: set that up first (see \"gh completion --help\"), then:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  bash  add to ~/.bashrc, after bash-completion is loaded:")
	fmt.Fprintln(w, "          eval \"$(gh workspace completion bash)\"")
	fmt.Fprintln(w, "  zsh   add to ~/.zshrc, after compinit:")
	fmt.Fprintln(w, "          eval \"$(gh workspace completion zsh)\"")
	fmt.Fprintln(w, "  fish  run once:")
	fmt.Fprintln(w, "          gh workspace completion fish > ~/.config/fish/conf.d/gh-workspace.fish")
}

func cmdCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printCompletionUsage(stderr)
		return exitSuccess
	}
	if len(args) != 1 || !slices.Contains(completionShells, args[0]) {
		fmt.Fprintln(stderr, "usage: "+completionUsage)
		return exitUsage
	}
	script, err := completionScripts.ReadFile("completions/gh-workspace." + args[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntimeFail
	}
	stdout.Write(script)
	return exitSuccess
}
