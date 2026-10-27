# zsh completion for gh workspace, from "gh workspace completion zsh".
#
# gh completes the names of installed extensions but nothing after them
# (cli/cli#5309). This wraps _gh, gh's own completer, so that "gh
# workspace ..." is asked of gh workspace; _gh still does everything else.

__gh_workspace_exec() { gh workspace "$@" }

__gh_workspace_install() {
    # compinit leaves _gh an autoload stub until its first use, and loading
    # gh's "#compdef" file makes _gh's body the whole file: running that
    # once defines the real _gh, which is the one to wrap.
    if [[ $functions[_gh] == 'builtin autoload'* ]]; then
        autoload -Uz +X _gh 2>/dev/null
    fi
    if [[ $functions[_gh] == *'compdef _gh gh'* ]]; then
        eval "$functions[_gh]"
    fi
    if ! (( $+functions[_gh] )); then
        (( $+functions[compdef] )) || return
        eval "$(gh completion -s zsh 2>/dev/null)"
        (( $+functions[_gh] )) || return
    fi

    [[ $functions[_gh] == *__gh_workspace_prev* ]] && return
    # Re-sourced after another extension wrapped this one: it calls into
    # this wrapper already, and saving it as ours would make a loop. Only
    # gh's own, freshly reloaded _gh is safe to save over the old copy.
    if (( $+functions[__gh_workspace_prev] )) && [[ $functions[_gh] != *' __complete '* ]]; then
        return
    fi

    functions[__gh_workspace_prev]=$functions[_gh]
    _gh() {
        if (( CURRENT > 2 )) && [[ $words[2] == workspace ]]; then
            words=(__gh_workspace_exec "${(@)words[3,-1]}")
            (( CURRENT-- ))
        fi
        __gh_workspace_prev "$@"
    }
}

__gh_workspace_install
unfunction __gh_workspace_install
