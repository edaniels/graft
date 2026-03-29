package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
)

const grAlias = "gr"

func generateCompletion(shellName string) string {
	var buf bytes.Buffer

	switch shellName {
	case "zsh":
		rootCmd.GenZshCompletion(&buf) //nolint:errcheck
	case "bash":
		rootCmd.GenBashCompletion(&buf) //nolint:errcheck
	case "fish":
		rootCmd.GenFishCompletion(&buf, true) //nolint:errcheck
	}

	return buf.String()
}

// generateActivateScript builds the shell activation script for the given shell.
// Writing to a buffer instead of directly to stdout makes this testable and avoids
// the custom printf linter's ban on fmt.Fprintf(os.Stdout, ...).
func generateActivateScript(shellName, exePath string) (string, error) {
	completionScript := generateCompletion(shellName)

	var buf bytes.Buffer

	switch shellName {
	case "zsh":
		fmt.Fprintf(&buf,
			`
if [ -z "$HOME" ]; then
  echo "graft: HOME is not set; cannot activate" >&2
else
export GRAFT_SESSION=$$
export _GC_LOCAL_PATH=%[1]s:$PATH
GRAFT_STATE_HOME=${XDG_STATE_HOME:-$HOME/.local/state}
_GC_SHIMS_PATH=$GRAFT_STATE_HOME/graft/local/sessions/$GRAFT_SESSION/shims
# TODO(erd): Implement cleanup of orphaned session shim directories.
mkdir -p $_GC_SHIMS_PATH
export PATH=$_GC_SHIMS_PATH:$PATH
alias %[3]s=graft
function _graft_preexec () {
  rehash
  local cmd="$1"
  local vars=()

  # Strip leading VAR=val pairs from the command string
  while [[ "$cmd" =~ '^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)=[^[:space:]]*(.*)$' ]]; do
    vars+=("${match[1]}")
    cmd="${match[2]}"
  done

  # Security: Export env vars directly input from the command line, not the environment
  if (( ${#vars} )); then
    export __INLINE_VARS="${(j:,:)vars}"
  else
    unset __INLINE_VARS
  fi
}

function _graft_resolve_connection () {
  local _gc_conn_file=$GRAFT_STATE_HOME/graft/local/sessions/$GRAFT_SESSION/current_connection
  GRAFT_CONNECTION=""
  if [[ -f "$_gc_conn_file" ]]; then
    GRAFT_CONNECTION=$(<"$_gc_conn_file")
  fi
  export GRAFT_CONNECTION
}

function _graft_precmd () {
  %[2]s report-cwd --pid=$GRAFT_SESSION `+"`pwd`"+` </dev/null &>/dev/null
  _graft_resolve_connection
}

autoload -Uz add-zsh-hook # what does this do
add-zsh-hook preexec _graft_preexec
export PATH=$_GC_SHIMS_PATH:$_GC_LOCAL_PATH
add-zsh-hook precmd _graft_precmd
if [[ -z "$GRAFT_PROMPT_DISABLE" ]]; then
  PS1='${GRAFT_CONNECTION:+[$GRAFT_CONNECTION] }'$PS1
fi
fi
`, filepath.Dir(exePath), exePath, grAlias)
		buf.WriteString(completionScript)
		fmt.Fprintf(&buf, "compdef _graft graft %s\n", grAlias)
	case "bash":
		fmt.Fprintf(&buf,
			`
if [ -z "$HOME" ]; then
  echo "graft: HOME is not set; cannot activate" >&2
else
export GRAFT_SESSION=$$
export _GC_LOCAL_PATH=%[1]s:$PATH
GRAFT_STATE_HOME=${XDG_STATE_HOME:-$HOME/.local/state}
_GC_SHIMS_PATH=$GRAFT_STATE_HOME/graft/local/sessions/$GRAFT_SESSION/shims
mkdir -p $_GC_SHIMS_PATH
export PATH=$_GC_SHIMS_PATH:$PATH
alias %[3]s=graft

_graft_preexec_fired=0
_graft_in_precmd=0

_graft_preexec () {
  if [[ "$_graft_in_precmd" == "1" ]]; then
    return
  fi
  if [[ "$_graft_preexec_fired" == "1" ]]; then
    return
  fi
  _graft_preexec_fired=1
  hash -r
  local full_cmd
  full_cmd=$(HISTTIMEFORMAT= history 1)
  full_cmd="${full_cmd#*[0-9] }"
  local cmd="$full_cmd"
  local vars=()

  while [[ "$cmd" =~ ^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)=[^[:space:]]*(.*) ]]; do
    vars+=("${BASH_REMATCH[1]}")
    cmd="${BASH_REMATCH[2]}"
  done

  if [[ ${#vars[@]} -gt 0 ]]; then
    local IFS=,
    export __INLINE_VARS="${vars[*]}"
  else
    unset __INLINE_VARS
  fi
}

_graft_resolve_connection () {
  local _gc_conn_file=$GRAFT_STATE_HOME/graft/local/sessions/$GRAFT_SESSION/current_connection
  GRAFT_CONNECTION=""
  if [[ -f "$_gc_conn_file" ]]; then
    GRAFT_CONNECTION=$(< "$_gc_conn_file")
  fi
  export GRAFT_CONNECTION
}

_graft_precmd () {
  _graft_in_precmd=1
  _graft_preexec_fired=0
  %[2]s report-cwd --pid=$GRAFT_SESSION $(pwd) </dev/null &>/dev/null
  _graft_resolve_connection
  _graft_in_precmd=0
}

trap '_graft_preexec' DEBUG
export PATH=$_GC_SHIMS_PATH:$_GC_LOCAL_PATH
PROMPT_COMMAND="_graft_precmd;${PROMPT_COMMAND:+$PROMPT_COMMAND}"
if [[ -z "$GRAFT_PROMPT_DISABLE" ]]; then
  PS1='${GRAFT_CONNECTION:+[$GRAFT_CONNECTION] }'"$PS1"
fi
fi
`, filepath.Dir(exePath), exePath, grAlias)
		buf.WriteString(completionScript)
		fmt.Fprintf(&buf, "complete -o default -o nospace -F __start_graft %s\n", grAlias)
	case "fish":
		//nolint:dupword
		fmt.Fprintf(&buf,
			`
if not set -q HOME
  echo "graft: HOME is not set; cannot activate" >&2
else
set -gx GRAFT_SESSION $fish_pid
set -gx _GC_LOCAL_PATH %[1]s:$PATH
if set -q XDG_STATE_HOME
  set _GC_STATE_HOME $XDG_STATE_HOME
else
  set _GC_STATE_HOME $HOME/.local/state
end
set _GC_SHIMS_PATH $_GC_STATE_HOME/graft/local/sessions/$GRAFT_SESSION/shims
command mkdir -p $_GC_SHIMS_PATH
set -gx PATH $_GC_SHIMS_PATH $PATH
function %[3]s; command graft $argv; end

function _graft_preexec --on-event fish_preexec
  set -l cmd $argv[1]
  set -l vars

  while string match -rq '^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)=[^[:space:]]*(.*)' -- $cmd
    set -a vars (string match -r '^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)=[^[:space:]]*(.*)' -- $cmd)[2]
    set cmd (string match -r '^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)=[^[:space:]]*(.*)' -- $cmd)[3]
  end

  if test (count $vars) -gt 0
    set -gx __INLINE_VARS (string join , $vars)
  else
    set -e __INLINE_VARS
  end
end

function _graft_postcmd --on-event fish_postexec
  %[2]s report-cwd --pid=$GRAFT_SESSION (pwd) </dev/null &>/dev/null
end

function _graft_update_connection
  set -l _gc_conn_file $_GC_STATE_HOME/graft/local/sessions/$GRAFT_SESSION/current_connection
  set -gx GRAFT_CONNECTION ""
  if test -f $_gc_conn_file
    set -gx GRAFT_CONNECTION (cat $_gc_conn_file)
  end
end

if not set -q GRAFT_PROMPT_DISABLE
  functions -q _graft_original_fish_prompt; or functions -c fish_prompt _graft_original_fish_prompt
  function fish_prompt
    _graft_update_connection
    if test -n "$GRAFT_CONNECTION"
      echo -n "[$GRAFT_CONNECTION] "
    end
    _graft_original_fish_prompt
  end
else
  function _graft_precmd_connection --on-event fish_prompt
    _graft_update_connection
  end
end

set -gx PATH $_GC_SHIMS_PATH (string match -v -- $_GC_SHIMS_PATH $PATH)
end
`, filepath.Dir(exePath), exePath, grAlias)
		buf.WriteString(completionScript)
		fmt.Fprintf(&buf, "complete -c %s -w graft\n", grAlias)
	default:
		return "", errors.Errorf("unknown shell '%s'", shellName)
	}

	return buf.String(), nil
}

var activateCmd = &cobra.Command{
	Use:       "activate <shell>",
	Short:     "Print shell activation script",
	Args:      cobra.ExactArgs(1),
	ValidArgs: []string{"bash", "zsh", "fish"},
	RunE: func(_ *cobra.Command, args []string) error {
		// TODO(erd): can look at https://github.com/jdx/mise/blob/3e382b34b6bf7d7b1a0efb8fdd8ea10c84498adb/src/shell/zsh.rs
		// for inspiration from mise + direnv
		//
		// mise/direnv eval scripts in response to precmd and chpwd whereas we
		// report the cwd and wait for shim dir to change. maybe their method is superior.
		exePath, err := os.Executable()
		if err != nil {
			exePath = os.Args[0]
		}

		script, err := generateActivateScript(args[0], exePath)
		if err != nil {
			return err
		}

		os.Stdout.WriteString(script)

		return nil
	},
}

func init() {
	rootCmd.AddCommand(activateCmd)
}
