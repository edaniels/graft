package graft

import (
	"maps"
	"slices"
	"sync"

	"github.com/edaniels/graft/errors"
)

// A Session represents an active shell session on this daemon's host. It keeps track
// of the current working directory and desired/actual forwarded commands. The session
// manager takes the desired forwardings and turns them into actual forwardings. The actual
// forwardings eventually get created as files that are in this session's PATH, if the shimming
// utility is installed into the shell.
type Session struct {
	sessMu      sync.Mutex
	desiredFwds map[string][]ForwardCommandIntent
	actualFwds  map[string]ForwardedCommand
	dir         string
	shimPath    string
	cwd         string
}

// A ForwardedCommand is a tuple of a path to a command and a connection that command can run on.
type ForwardedCommand struct {
	conn *Connection
	path string
}

// CWD returns the most up-to-date known current working directory of the session.
func (sess *Session) CWD() string {
	sess.sessMu.Lock()
	defer sess.sessMu.Unlock()

	return sess.cwd
}

// ShimPath returns the directory that shims are added to.
func (sess *Session) ShimPath() string {
	return sess.shimPath
}

// ActualForwardings returns the currently set command forwardings.
func (sess *Session) ActualForwardings() map[string]ForwardedCommand {
	sess.sessMu.Lock()
	defer sess.sessMu.Unlock()

	return maps.Clone(sess.actualFwds)
}

// DesiredForwardings returns the currently set desired forwardings for a reconciler to use.
func (sess *Session) DesiredForwardings() map[string][]ForwardCommandIntent {
	sess.sessMu.Lock()
	defer sess.sessMu.Unlock()

	desCopy := make(map[string][]ForwardCommandIntent, len(sess.desiredFwds))
	for key, value := range sess.desiredFwds {
		desCopy[key] = slices.Clone(value)
	}

	return desCopy
}

var errForwardingNotFound = errors.NewBare("no forwarding found")

// Which returns which forwarding a command maps to based on the current state
// of active connectons.
func (sess *Session) Which(command string) (*ForwardedCommand, error) {
	if command == "" {
		return nil, errors.New("empty command")
	}

	sess.sessMu.Lock()
	defer sess.sessMu.Unlock()

	var ok bool

	fwdToConn, ok := sess.actualFwds[command]
	if !ok {
		return nil, errors.WrapSuffix(errForwardingNotFound, command)
	}

	return &fwdToConn, nil
}

// UpdateActualForwardCommands updates the current actually forwarded commands.
func (sess *Session) UpdateActualForwardCommands(newActual map[string]ForwardedCommand) {
	sess.sessMu.Lock()
	defer sess.sessMu.Unlock()

	sess.actualFwds = newActual
}

// UpdateDesiredForwardCommands updates the current desired forwardings for a reconciler to use.
func (sess *Session) UpdateDesiredForwardCommands(toDestination string, commands []ForwardCommandIntent) {
	sess.sessMu.Lock()
	defer sess.sessMu.Unlock()

	sess.desiredFwds[toDestination] = append(sess.desiredFwds[toDestination], commands...)
}
