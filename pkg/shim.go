package graft

var (
	// shimmedCmd is what intercepts a local shell command and passes it through graft. Its goal
	// is to preserve the command and its arguments as much as possible.
	//
	// Note(erd): The current incantation of printf + %q and quotes appears to preserve a command
	// quite well. I couldn't explain it to you without working through it again though, so it's
	// worth documenting again at some point because it's probably not perfect.
	shimmedCmd = []byte(`#!/bin/sh
set -e
export PATH=$_GC_LOCAL_PATH
if [ "$#" -lt 1 ]; then
	args=""
else
	args="$(printf " %q" "${@}")"
fi
graft run-shimmed-cmd --pid=$GRAFT_SESSION --cwd=$(pwd) --cmd=$(basename $0) -- $args
`)

	// shimmedSudoCmd is like shimmedCmd but differs in that it's expected to always be called for all sudo commands
	// since we don't yet know if the sudo'd command in question is shimmed. If it isn't we call the command as is.
	// TODO(erd): Investigate whether sudoer profile configuration could replace this shim.
	shimmedSudoCmd = []byte(`#!/bin/sh
set -e
export PATH=$_GC_LOCAL_PATH
if [ "$#" -lt 1 ]; then
	exec $(basename $0) $@
	return
fi
if [ "$#" -lt 2 ]; then
	args=""
else
	args="$(printf " %q" "${@:2}")"
fi

shim_exists=0
cmd=$(basename $1)
PATH=$_GC_SHIMS_PATH type -P $cmd </dev/null &>/dev/null || shim_exists=$?
if [ -n "$_GC_SHIMS_PATH" ] && [ "$shim_exists" -eq 0 ]; then
	graft run-shimmed-cmd --pid=$GRAFT_SESSION --cwd=$(pwd) --sudo --cmd=$cmd -- $args
else
	exec $(basename $0) $@
fi
`)
)
