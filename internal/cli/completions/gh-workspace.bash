# bash completion for gh workspace, from "gh workspace completion bash".
#
# gh completes the names of installed extensions but nothing after them
# (cli/cli#5309). This wraps __gh_get_completion_results, the hook gh's own
# completer uses to ask a program for candidates, so that "gh workspace
# ..." is asked of gh workspace; gh's completer still does everything else.

__gh_workspace_exec() { gh workspace "$@"; }

__gh_workspace_install() {
    if ! declare -F __gh_get_completion_results >/dev/null && declare -F _completion_loader >/dev/null; then
        _completion_loader gh >/dev/null 2>&1
    fi
    if ! declare -F __gh_get_completion_results >/dev/null; then
        # gh's completer calls bash-completion's helpers; loading it without
        # them would break Tab for gh's own commands too.
        declare -F _get_comp_words_by_ref >/dev/null || return
        eval "$(gh completion -s bash 2>/dev/null)"
        declare -F __gh_get_completion_results >/dev/null || return
    fi

    local hook
    hook=$(declare -f __gh_get_completion_results)
    [[ $hook == *__gh_workspace_prev* ]] && return
    # Re-sourced after another extension wrapped this one: it calls into
    # this wrapper already, and saving it as ours would make a loop. Only
    # gh's own, freshly reloaded hook is safe to save over the old copy.
    if declare -F __gh_workspace_prev >/dev/null && [[ $hook != *" __complete "* ]]; then
        return
    fi

    eval "__gh_workspace_prev${hook#__gh_get_completion_results}"
    __gh_get_completion_results() {
        if [[ ${#words[@]} -ge 3 && ${words[1]} == workspace ]]; then
            local -a words=(__gh_workspace_exec "${words[@]:2}")
            __gh_workspace_prev "$@"
            return
        fi
        __gh_workspace_prev "$@"
    }
}

__gh_workspace_install
unset -f __gh_workspace_install
