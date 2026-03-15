package graft

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/edaniels/graft/errors"
)

const (
	sudoCommandName           = "sudo"
	currentConnectionFileName = "current_connection"
)

// The SessionManager is responsible for the lifetime of [Session]s. It is essentially a controller
// that periodically brings the state of local user sessions in line with their configuration, specifically
// for command shimming and running shimmed/direct commands.
type SessionManager struct {
	sessions  map[uint64]*Session
	connMgr   *ConnectionManager
	rootPath  string
	sessMgrMu sync.Mutex
}

// NewSessionManager returns a non-started SessionManager that has access to the [ConnectionManager].
func NewSessionManager(connMgr *ConnectionManager) (*SessionManager, error) {
	rootPath, err := SessionsRoot()
	if err != nil {
		return nil, err
	}

	return &SessionManager{
		sessions: map[uint64]*Session{},
		connMgr:  connMgr,
		rootPath: rootPath,
	}, nil
}

// Run handles the controller logic for SessionManager until runCtx is canceled.
func (mgr *SessionManager) Run(runCtx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastSum uint64

	for {
		select {
		case <-ticker.C:
		case <-runCtx.Done():
			err := context.Cause(runCtx)
			if err != nil {
				if errors.Is(err, ErrShuttingDown) {
					slog.DebugContext(runCtx, "stopping session manager")
				} else {
					slog.ErrorContext(runCtx, "stopping session manager", "error", context.Cause(runCtx))
				}
			}

			return
		}

		lastSum = mgr.tick(runCtx, lastSum)
	}
}

func (mgr *SessionManager) tick(runCtx context.Context, lastSum uint64) uint64 {
	mgr.sessMgrMu.Lock()
	defer mgr.sessMgrMu.Unlock()

	var newSum uint64
	for pid := range mgr.sessions {
		newSum += pid
	}

	if newSum != lastSum {
		slog.DebugContext(runCtx, "sessions", "count", len(mgr.sessions))
	}

	mgr.tickPruneSessions(runCtx)

	for _, sess := range mgr.sessions {
		mgr.tickReconcileSession(runCtx, sess)
	}

	return newSum
}

// tickPruneSessions iterates over all sessions and removes from memory/filesystem any that:
// - are not found on the FS
// - found on the FS but corresponding process PID is no longer around.
func (mgr *SessionManager) tickPruneSessions(runCtx context.Context) {
	sessDirs, err := os.ReadDir(mgr.rootPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.DebugContext(runCtx, "error reading sessions directory", "error", err)
		}

		return
	}

	for _, dir := range sessDirs {
		fullPath := SessionPathFromRoot(mgr.rootPath, dir.Name())

		sessPid, parseErr := strconv.ParseUint(dir.Name(), 10, 64)
		if parseErr != nil {
			// ignore; remove the FS reference
			mgr.removeSession(0, fullPath)

			continue
		}

		if sessPid > math.MaxInt {
			slog.DebugContext(runCtx, "malformed pid from session path", "pid", dir.Name())

			continue
		}
		// todo(erd): may need to move if this becomes fsnotify based
		proc, findErr := os.FindProcess(int(sessPid))
		if findErr != nil {
			if !errors.Is(findErr, os.ErrProcessDone) {
				slog.DebugContext(runCtx, "removing session", "pid", sessPid, "error", findErr)
			}

			mgr.removeSession(sessPid, fullPath)

			continue
		}

		// Process may be found but signaling it may then determine it's not actually running
		sigErr := proc.Signal(syscall.Signal(0))
		if sigErr != nil {
			if !errors.Is(sigErr, os.ErrProcessDone) {
				slog.DebugContext(runCtx, "removing session", "pid", sessPid, "error", sigErr)
			}

			mgr.removeSession(sessPid, fullPath)

			continue
		}

		// TODO(erd): Verify whether this call is necessary; remove after confirming sessions/shimming works correctly.
		if _, getErr := mgr.getOrCreateSession(sessPid); getErr != nil {
			slog.ErrorContext(runCtx, "error establishing existing session", "error", getErr)
		}
	}
}

// tickReconcileSession updates the actual state of the session to reflect the desired state based
// on requested forwardings (intents) and what's available on established connections. This is a
// simple reconciliation "tick" -- get and save prior state, generate new state with additions, calculate
// deletions from additions and prior state, process deletions, process additions, set final state.
//
// TODO(erd): ideally severed connections could not remove shims but instead stick around with
// error messages from the daemon.
// Note(erd): this feels like a critical section for the file system and in-memory state; maybe it
// doesn't actually matter if the state of the system isn't updated atomically here?
func (mgr *SessionManager) tickReconcileSession(runCtx context.Context, sess *Session) {
	// Collect existing shims into potential deletions.
	var existingShimsToDelete map[string]struct{}

	entries, err := os.ReadDir(sess.ShimPath())
	if err == nil {
		existingShimsToDelete = make(map[string]struct{}, len(entries))
		for _, entry := range entries {
			name := entry.Name()
			if name == sudoCommandName {
				continue
			}

			existingShimsToDelete[entry.Name()] = struct{}{}
		}
	} else {
		slog.DebugContext(runCtx, "error reading session directory", "error", err)
	}

	newForwardings, err := mgr.gatherDesiredShimsForSession(runCtx, sess)
	if err != nil {
		slog.DebugContext(runCtx, "error gathering desired shims for session", "error", err)

		return
	}
	// keep these shims from being deleted; we could replace each file but it can cause temporary
	// file access issues.
	maps.DeleteFunc(existingShimsToDelete, func(k string, _ struct{}) bool {
		_, ok := newForwardings[k]

		return ok
	})

	// remove old shims
	if len(existingShimsToDelete) != 0 {
		slog.DebugContext(runCtx, "removing", "shim_path", sess.ShimPath(), "shims", existingShimsToDelete)
	}

	for entry := range existingShimsToDelete {
		fileName := filepath.Join(sess.ShimPath(), entry)

		err := os.Remove(fileName)
		if err != nil {
			slog.ErrorContext(runCtx, "error removing shim", "error", err)
		}
	}

	// update actual so that run command requests are up to date
	sess.UpdateActualForwardCommands(newForwardings)

	shimPath := sess.ShimPath()
	for shimmedName := range newForwardings {
		shimCommand(runCtx, shimPath, shimmedName)
	}

	shimCommand(runCtx, shimPath, sudoCommandName)

	// Update current connection file for shell prompt
	connName := ""

	if cwd := sess.CWD(); cwd != "" {
		if conn, ok := mgr.connMgr.ConnectionByCWD(runCtx, cwd); ok {
			connName = conn.Name()
		}
	}

	mgr.writeSessionConnectionFile(sess, connName)
}

func (mgr *SessionManager) writeSessionConnectionFile(sess *Session, connName string) {
	filePath := filepath.Join(filepath.Dir(sess.ShimPath()), currentConnectionFileName)

	existing, err := os.ReadFile(filePath)
	if err == nil && string(existing) == connName {
		return
	}

	if err := os.WriteFile(filePath, []byte(connName), FilePerms); err != nil {
		slog.Error("error writing current connection file", "error", err)
	}
}

// gatherDesiredShimsForSession collects all available commands that a session could run based on established connections and
// the session's desired state.
func (mgr *SessionManager) gatherDesiredShimsForSession(runCtx context.Context, sess *Session) (map[string]ForwardedCommand, error) {
	newForwardings := map[string]ForwardedCommand{}

	for destination, toFwdIntents := range mgr.DesiredForwardingsForSession(runCtx, sess) {
		conn, err := mgr.connMgr.Connection(destination)
		if err != nil {
			slog.DebugContext(runCtx, "cannot get connection for forwarding", "error", err)

			return nil, err
		}

		// make a mapping of base(path_to_command) => path_to_command (e.g. bash => /usr/bin/bash)
		// This is helpful when running a command through a shell wrapper that may not source its PATH
		// in the same way in which we originally found the command.
		availableCommandsFromConn := conn.daemon.AvailableCommands()

		localRemoteCommands := make(map[string]string, len(availableCommandsFromConn))
		for _, cmd := range availableCommandsFromConn {
			local := filepath.Base(cmd)
			localRemoteCommands[local] = cmd
		}

		for _, toFwd := range toFwdIntents {
			localRemoteCmd, ok := localRemoteCommands[toFwd.Name]
			if !ok {
				// unavailable
				continue
			}

			shimmedName := toFwd.LocalName(conn.Name())
			// slogContext.runCtx,Debug("shimmed", "name", name, "local_path", fileName, "remote_path", localRemoteCommands[name])
			if localRemoteCmd == "" {
				slog.ErrorContext(runCtx, "invariant: no local remote command", "name", toFwd.Name)

				return nil, err
			}

			newForwardings[shimmedName] = ForwardedCommand{conn: conn, path: localRemoteCmd}
		}
	}

	return newForwardings, nil
}

// shimCommand shims a command if it is not already shimmed with the shim wrapper contents.
func shimCommand(runCtx context.Context, shimPath string, cmdName string) {
	cmdShim := shimmedCmd
	if cmdName == sudoCommandName {
		cmdShim = shimmedSudoCmd
	}

	fileName := filepath.Join(shimPath, cmdName)

	var rewriteFile bool
	if _, err := os.Stat(fileName); err != nil {
		rewriteFile = true
	} else {
		currentVal, err := os.ReadFile(fileName)
		if err != nil || !bytes.Equal(currentVal, cmdShim) {
			rewriteFile = true
		}
	}

	if !rewriteFile {
		return
	}

	slog.DebugContext(runCtx, "adding shim", "shim_path", shimPath, "cmd", cmdName)

	filePerm := fs.FileMode(ExecFilePerms)
	if err := os.WriteFile(fileName, cmdShim, filePerm); err != nil {
		slog.ErrorContext(runCtx, "error writing shim", "for", cmdName, "error", err)

		return
	}

	if err := os.Chmod(fileName, filePerm); err != nil {
		slog.ErrorContext(runCtx, "error changing mode on shim", "for", cmdName, "error", err)

		return
	}
}

// ForwardCommandIntent is a request to forward a command to a destination that may be optionally
// prefxied (e.g. python->linux1-python).
type ForwardCommandIntent struct {
	Name   string
	Prefix bool
	Global bool
}

// LocalName returns the fully qualified name of a command that a user would need to execute based
// on the given connection name (e.g. name=python,prefix=true,connName=linux1 => linux1-python).
func (i ForwardCommandIntent) LocalName(connName string) string {
	localName := i.Name
	if i.Prefix {
		// TODO(erd): make sure this is a valid file name.
		// TODO(erd): make sure that if prefixes are on that you can have multi-conns
		localName = fmt.Sprintf("%s-%s", connName, localName)
	}

	return localName
}

// DesiredForwardingsForSession returns all the commands a session wants forwarded based on all established
// connections and the sessions own ephemeral forwardings. This isn't that helpful until we utilize the
// tracked cwd to figure out what to forward.
//
// TODO(erd): but really, this needs some product design.
func (mgr *SessionManager) DesiredForwardingsForSession(ctx context.Context, sess *Session) map[string][]ForwardCommandIntent {
	// Connections
	sess.sessMu.Lock()
	cwd := sess.cwd
	sess.sessMu.Unlock()

	flatFwds := mgr.connMgr.forwardings(ctx, cwd)

	// Session - Ephemeral
	for sessDest, sessFwdList := range sess.DesiredForwardings() {
		for _, fwd := range sessFwdList {
			if _, ok := flatFwds[fwd]; ok {
				// TODO(erd): Verify this log message is correct with multiple connections; check prefix handling.
				slog.DebugContext(ctx, "overwriting forwards from session", "name", fwd.Name, "to", sessDest)
			}

			flatFwds[fwd] = append(flatFwds[fwd], sessDest)
		}
	}

	allFwds := map[string][]ForwardCommandIntent{}

	for fwds, dests := range flatFwds {
		for _, dest := range dests {
			allFwds[dest] = append(allFwds[dest], fwds)
		}
	}

	return allFwds
}

// removeSession removes a session and its shimmings for the given session. This doesn't affect running
// commands.
func (mgr *SessionManager) removeSession(sessPid uint64, fullPath string) {
	delete(mgr.sessions, sessPid)

	err := os.RemoveAll(fullPath)
	if err != nil {
		slog.Error("error removing session", "error", err)
	}
}

// getOrCreateSession returns an existing session associated with the PID or makes a new one
// in memory and on the filesystem.
//
// Assumes lock is held for duration of call.
func (mgr *SessionManager) getOrCreateSession(sessionPID uint64) (*Session, error) {
	sess, ok := mgr.sessions[sessionPID]
	if ok {
		return sess, nil
	}

	sessPath, err := SessionPath(sessionPID)
	if err != nil {
		return nil, err
	}

	shimPath := filepath.Join(sessPath, "shims")
	// TODO(erd): Define cleanup strategy for session shim directories.

	// make the session directory
	if _, err := os.Stat(sessPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.Wrap(err)
		}

		err := os.MkdirAll(sessPath, DirPerms)
		if err != nil {
			return nil, errors.Wrap(err)
		}
	}

	// make the session shim directory
	if _, err := os.Stat(shimPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.Wrap(err)
		}

		err := os.MkdirAll(shimPath, DirPerms)
		if err != nil {
			return nil, errors.Wrap(err)
		}
	}

	sess = &Session{
		desiredFwds: map[string][]ForwardCommandIntent{},
		actualFwds:  map[string]ForwardedCommand{},
		dir:         sessPath,
		shimPath:    shimPath,
	}
	mgr.sessions[sessionPID] = sess

	return sess, nil
}

// UpdateSessionCWD is used to update the last known cwd of a session identified by its PID.
// It immediately reconciles shims so that the shell picks up changes in the same prompt cycle.
func (mgr *SessionManager) UpdateSessionCWD(ctx context.Context, sessionPID uint64, cwd string) error {
	mgr.sessMgrMu.Lock()
	defer mgr.sessMgrMu.Unlock()

	sess, err := mgr.getOrCreateSession(sessionPID)
	if err != nil {
		return err
	}

	// TODO(erd): Protect against race condition on sess.cwd (concurrent read/write).
	sess.cwd = cwd

	mgr.tickReconcileSession(ctx, sess)

	return nil
}

// Close does nothing for now.
func (mgr *SessionManager) Close() {
}

// SessionByPID returns a session by its PID.
func (mgr *SessionManager) SessionByPID(sessionPID uint64) (*Session, error) {
	mgr.sessMgrMu.Lock()
	sess, err := mgr.getOrCreateSession(sessionPID)
	mgr.sessMgrMu.Unlock()

	if err != nil {
		return nil, err
	}

	return sess, nil
}

// Which returns the current forwarding for a command, if found.
func (mgr *SessionManager) Which(
	sess *Session,
	command string,
) (*ForwardedCommand, error) {
	return sess.Which(command)
}

// selectConnection figures out which connection to use based on the current state of the system.
//
// TODO(erd): this needs work like DesiredForwardingsForSession does in terms of the best UX
// for how to accomplish this. In the past, we just took the first session/connection found which
// works in the simplest of cases. Ideally we start with something like a hierarchy of:
// - connection name specified
// - session set a default
// - determine via cwd.
func (mgr *SessionManager) selectConnection(ctx context.Context, connName string, cwd string) (*Connection, error) {
	slog.DebugContext(ctx, "lookup connection by name", "name", connName)

	if connName != "" {
		return mgr.connMgr.Connection(connName)
	}

	slog.DebugContext(ctx, "lookup connection by cwd", "cwd", cwd)

	if cwd != "" {
		selectedConn, haveConn := mgr.connMgr.ConnectionByCWD(ctx, cwd)
		if haveConn {
			return selectedConn, nil
		}
	}

	return nil, errors.New("no connection to start a shell with by default")
}

func (mgr *SessionManager) selectConnectionForCommand(ctx context.Context, connName, command, cwd string) (ForwardedCommand, error) {
	conn, selectErr := mgr.selectConnection(ctx, connName, cwd)
	if selectErr != nil {
		return ForwardedCommand{}, selectErr
	}

	fwdCmd := ForwardedCommand{conn, command}

	if command == "" || filepath.IsAbs(command) {
		return fwdCmd, nil
	}

	// we want to select the command we found from the PATH, if possible
	availableCommandsFromConn := conn.daemon.AvailableCommands()

	var newCommand string

	for _, avail := range availableCommandsFromConn {
		local := filepath.Base(avail)
		if local == command {
			newCommand = avail

			break
		}
	}

	if newCommand == "" {
		slog.DebugContext(ctx, "unable to match command name to available in PATH; using provided", "command", command)
	} else {
		fwdCmd.path = newCommand
	}

	return fwdCmd, nil
}

var errConnectionNotConnected = errors.NewBare("connection not connected")

// RunCommand figures out where to send a command issued by a session.
//
// TODO(erd): there's way too many parameters here to document and understand
// their combinatoric effects, clearly shown by the confusing conditional logic below.
func (mgr *SessionManager) RunCommand(
	runCtx context.Context,
	sessionPID uint64, // local pid at invocation site; required
	connectionName string, // connection name to send command to; optional; inferred if unset

	shell bool, // start a shell; optional, mutually exclusive with command/arguments; required

	exactCommand bool, // whether or not to use shims to determine actual command; optional
	command string, // the command to execute, mutually exclusive with shell; required
	arguments []string, // arguments for the command
	extraEnv []string,

	sudo bool, // execute with sudo

	// whether or not to allocate a pseudo terminal.
	allocatePty bool,

	// this redirects the stdout of a pty to the bidi command itself.
	redirectStdout bool,

	// this redirects the stderr of a pty to the bidi command itself.
	redirectStderr bool,
) (RunningCommand, error) {
	if shell == (command != "") {
		return nil, errors.New("can either start a shell or run a command")
	}

	sess, err := mgr.SessionByPID(sessionPID)
	if err != nil {
		return nil, err
	}

	var resolvedFwdCmd ForwardedCommand

	// If a commad was explicitly provided (e.g. graft run) or a shell was requested,
	// select the best connection to use and resolve the command to the absolute
	// path to the command on the remote side.
	if exactCommand || shell {
		// TODO(erd): Protect against race condition when reading sess.cwd.
		selected, selectErr := mgr.selectConnectionForCommand(runCtx, connectionName, command, sess.cwd)
		if selectErr != nil {
			return nil, selectErr
		}

		resolvedFwdCmd = selected
	} else {
		// Otherwise, this command comes from a shimmed request and we don't know where to send the command yet,
		// so ask the manager which to send it to based on its reconciled state.
		whichFwdToConn, whichErr := mgr.Which(sess, command)
		if whichErr != nil {
			return nil, whichErr
		}

		resolvedFwdCmd = *whichFwdToConn
	}

	if state, _ := resolvedFwdCmd.conn.State(); state != ConnectionStateConnected {
		return nil, errors.WrapSuffix(errConnectionNotConnected, resolvedFwdCmd.conn.Name())
	}

	// TODO(erd): race; old comment: i think the race was referring to synchronization
	// not being established yet and we could choose a bad cwd.

	// find the proper cwd to run from.
	var cwd string

	cwdMatch, ok := resolvedFwdCmd.conn.MatchCWD(sess.cwd)
	if ok {
		cwd = cwdMatch
	}

	slog.DebugContext(runCtx, "run command",
		"shell", shell,
		"sudo", sudo,
		"allocatePty", allocatePty,
		"redirectStdout", redirectStdout,
		"redirectStderr", redirectStderr,
		"requested_command", command,
		"actual_command", resolvedFwdCmd.path,
		"cwd", cwd,
		"arguments", arguments)

	runningCmd, err := resolvedFwdCmd.conn.RunCommand(
		runCtx,
		cwd,
		shell,
		resolvedFwdCmd.path,
		arguments,
		extraEnv,
		sudo,
		allocatePty,
		redirectStdout,
		redirectStderr,
	)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return runningCmd, nil
}
