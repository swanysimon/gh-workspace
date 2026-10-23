# fish completion for gh workspace, from "gh workspace completion fish".
#
# gh completes the names of installed extensions but nothing after them
# (cli/cli#5309). This wraps __gh_perform_completion, the hook gh's own
# completer uses to ask a program for candidates, so that "gh workspace
# ..." is asked of gh workspace; gh's completer still does everything else.
#
# It belongs in conf.d, not completions: gh's own completions file erases
# every other "gh" completion when it loads, so this has to load it first
# and wrap it afterwards.

function __gh_workspace_install
    if not functions -q __gh_perform_completion
        complete --do-complete "gh " >/dev/null 2>&1
    end
    if not functions -q __gh_perform_completion
        gh completion -s fish 2>/dev/null | source
    end
    functions -q __gh_perform_completion; or return

    set -l hook (functions __gh_perform_completion | string collect)
    string match -q -- '*__gh_workspace_prev*' $hook; and return
    # Re-sourced after another extension wrapped this one: it calls into
    # this wrapper already, and saving it as ours would make a loop. Only
    # gh's own, freshly reloaded hook is safe to save over the old copy.
    if functions -q __gh_workspace_prev; and not string match -q -- '* __complete *' $hook
        return
    end

    functions -e __gh_workspace_prev
    functions -c __gh_perform_completion __gh_workspace_prev
    function __gh_perform_completion
        set -l args (commandline -opc)
        if test (count $args) -lt 2; or test $args[2] != workspace
            __gh_workspace_prev
            return
        end
        set -l results (eval gh workspace __complete $args[3..-1] (string escape -- (commandline -ct)) 2>/dev/null)
        if test (count $results) -eq 0
            printf ':1\n'
            return
        end
        printf '%s\n' $results
    end
end

__gh_workspace_install
functions -e __gh_workspace_install
