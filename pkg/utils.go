package graft

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/term"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/edaniels/graft/errors"
)

const (
	// DirPerms are the standard directory permissions to use when creating directories. (rwx-rx-).
	DirPerms = 0o750
	// FilePerms are the standard directory permissions to use when creating non-executable files. (rw--).
	FilePerms = 0o600
	// ExecFilePerms are the standard directory permissions to use when creating executable files. (rwx-rwx-).
	ExecFilePerms = 0o770

	// initializeTimeout is the default timeout to use for long-running initialization operations.
	initializeTimeout = 5 * time.Minute

	// graftDirName is the directory name used for Graft's config, state, and runtime directories.
	graftDirName = "graft"
)

// hasPathPrefix checks whether path has the given prefix on a directory boundary.
// It returns the remaining suffix and true if the prefix matches.
// For example, hasPathPrefix("/home/user/proj", "/home/user") returns ("/proj", true)
// but hasPathPrefix("/home/user/project", "/home/user/proj") returns ("", false).
func hasPathPrefix(path, prefix string) (string, bool) {
	after, ok := strings.CutPrefix(path, prefix)
	if !ok {
		return "", false
	}

	if after != "" && after[0] != filepath.Separator {
		return "", false
	}

	return after, true
}

// BinaryName returns the graft binary name for the given OS and architecture (e.g. "graft-linux-amd64").
func BinaryName(osName, archName string) string {
	return "graft-" + osName + "-" + archName
}

// BufferedLineWriter retains the last N (MaxLines) lines written to it.
type BufferedLineWriter struct {
	MaxLines int

	mu    sync.Mutex
	lines []string
}

func (blw *BufferedLineWriter) Write(data []byte) (int, error) {
	blw.mu.Lock()
	defer blw.mu.Unlock()

	// I'm just hoping that loggers write one line at a time :)
	if len(blw.lines)+1 > blw.MaxLines {
		// trim
		blw.lines = blw.lines[1:]
	}

	blw.lines = append(blw.lines, string(data))

	return len(data), nil
}

func (blw *BufferedLineWriter) Lines() []string {
	blw.mu.Lock()
	defer blw.mu.Unlock()

	return slices.Clone(blw.lines)
}

// IsCanceledError returns if the given error is some type of "cancelation" error, be it from
// the stdlib or gRPC.
func IsCanceledError(err error) bool {
	return errors.Is(err, context.Canceled) || status.Convert(err).Code() == codes.Canceled
}

// SSHPath returns the expected ssh configuration path for the current user.
//
// TODO(erd): verify how universal this is or if there's some better binary to call
// to figure this out, like ssh itself (or some library).
func SSHPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(err)
	}

	return filepath.Join(homeDir, ".ssh"), nil
}

// graftConfigHome returns the config directory for Graft (~/.config/graft).
// Checks GRAFT_CONFIG_HOME first, then XDG_CONFIG_HOME/graft, then ~/.config/graft.
func graftConfigHome(homeDir string) string {
	if val := os.Getenv("GRAFT_CONFIG_HOME"); val != "" {
		return val
	}

	if val := os.Getenv("XDG_CONFIG_HOME"); val != "" {
		return filepath.Join(val, graftDirName)
	}

	return filepath.Join(homeDir, ".config", graftDirName)
}

// graftStateHome returns the state directory for Graft (~/.local/state/graft).
// Checks GRAFT_STATE_HOME first, then XDG_STATE_HOME/graft, then ~/.local/state/graft.
func graftStateHome(homeDir string) string {
	if val := os.Getenv("GRAFT_STATE_HOME"); val != "" {
		return val
	}

	if val := os.Getenv("XDG_STATE_HOME"); val != "" {
		return filepath.Join(val, graftDirName)
	}

	return filepath.Join(homeDir, ".local", "state", graftDirName)
}

// graftCacheHome returns the cache directory for Graft (~/.cache/graft).
// Checks GRAFT_CACHE_HOME first, then XDG_CACHE_HOME/graft, then ~/.cache/graft.
func graftCacheHome(homeDir string) string {
	if val := os.Getenv("GRAFT_CACHE_HOME"); val != "" {
		return val
	}

	if val := os.Getenv("XDG_CACHE_HOME"); val != "" {
		return filepath.Join(val, graftDirName)
	}

	return filepath.Join(homeDir, ".cache", graftDirName)
}

// DataDirectories outline where graft stores data.
type DataDirectories struct {
	Config string
	State  string
}

// ResolvedGraftDirs returns the resolved config and state directories for Graft.
func ResolvedGraftDirs() (DataDirectories, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return DataDirectories{}, errors.Wrap(err)
	}

	return DataDirectories{Config: graftConfigHome(homeDir), State: graftStateHome(homeDir)}, nil
}

// DaemonCacheDir returns the cache directory for daemon binaries.
// Uses GRAFT_CACHE_HOME or XDG_CACHE_HOME, falling back to ~/.cache/graft/binaries.
func DaemonCacheDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(err)
	}

	return filepath.Join(graftCacheHome(homeDir), "binaries"), nil
}

func daemonSocketPath(stateHome string, role ServerRole, identity string) (string, error) {
	roleDir, err := roleSubdir(role)
	if err != nil {
		return "", err
	}

	if identity != "" {
		return filepath.Join(stateHome, roleDir, identity, "graftd.sock"), nil
	}

	return filepath.Join(stateHome, roleDir, "graftd.sock"), nil
}

// DaemonSocketPathForCurrentHost returns the expected graft daemon socket path for the current user.
// Socket is stored in the state directory: $GRAFT_STATE_HOME/{local,remote}/graftd.sock.
func DaemonSocketPathForCurrentHost(role ServerRole) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(err)
	}

	return daemonSocketPath(graftStateHome(homeDir), role, "")
}

// DaemonSocketPathForRemote returns the expected graft daemon socket path for a remote user.
// Socket is stored in the state directory: ~/.local/state/graft/remote/graftd.sock.
// If identity is provided, the socket is namespaced under that identity.
func DaemonSocketPathForRemote(homeDir, identity string) (string, error) {
	return daemonSocketPath(graftStateHome(homeDir), ServerRoleRemote, identity)
}

func rootConfigPath(configHome string, role ServerRole) (string, error) {
	roleDir, err := roleSubdir(role)
	if err != nil {
		return "", err
	}

	return filepath.Join(configHome, roleDir, "config.yml"), nil
}

// RootConfigPathForCurrentHost returns the expected graft config path for the current user.
// Uses GRAFT_CONFIG_HOME or XDG_CONFIG_HOME, falling back to ~/.config/graft/{local,remote}/config.yml.
func RootConfigPathForCurrentHost(role ServerRole) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(err)
	}

	return rootConfigPath(graftConfigHome(homeDir), role)
}

// RootConfigPathForRemote returns the expected graft config path for a remote user.
// Uses GRAFT_CONFIG_HOME or XDG_CONFIG_HOME, falling back to ~/.config/graft/remote/config.yml.
func RootConfigPathForRemote(homeDir string) (string, error) {
	return rootConfigPath(graftConfigHome(homeDir), ServerRoleRemote)
}

func daemonLogsPath(stateHome string, role ServerRole) (string, error) {
	roleDir, err := roleSubdir(role)
	if err != nil {
		return "", err
	}

	return filepath.Join(stateHome, roleDir, "logs"), nil
}

// DaemonLogsPathForCurrentHost returns the expected graft daemon logs path for the current user.
// Uses GRAFT_STATE_HOME or XDG_STATE_HOME, falling back to ~/.local/state/graft/{local,remote}/logs.
func DaemonLogsPathForCurrentHost(role ServerRole) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(err)
	}

	return daemonLogsPath(graftStateHome(homeDir), role)
}

// DaemonLogsPathForRemote returns the expected graft daemon logs path for a remote user.
// Uses GRAFT_STATE_HOME or XDG_STATE_HOME, falling back to ~/.local/state/graft/remote/logs.
func DaemonLogsPathForRemote(homeDir string) (string, error) {
	return daemonLogsPath(graftStateHome(homeDir), ServerRoleRemote)
}

// SessionsRoot returns the expected graft sessions path for the current user.
// Uses GRAFT_STATE_HOME or XDG_STATE_HOME, falling back to ~/.local/state/graft/local/sessions.
func SessionsRoot() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(err)
	}

	roleDir, err := roleSubdir(ServerRoleLocal)
	if err != nil {
		return "", err
	}

	return filepath.Join(graftStateHome(homeDir), roleDir, "sessions"), nil
}

// roleSubdir returns the subdirectory name for the given server role.
func roleSubdir(role ServerRole) (string, error) {
	switch role {
	case ServerRoleLocal:
		return "local", nil
	case ServerRoleRemote:
		return "remote", nil
	default:
		return "", errors.Errorf("unknown role %s", role)
	}
}

// SessionPath returns the expected session path for the given session PID of the current user.
func SessionPath(sessionPID uint64) (string, error) {
	sessionsRoot, err := SessionsRoot()
	if err != nil {
		return "", err
	}

	return SessionPathFromRoot(sessionsRoot, strconv.FormatUint(sessionPID, 10)), nil
}

// SessionPathFromRoot returns the expected session path for the given session root and pid of the current user.
//
// TODO(erd): why is this needed as exported/factored?
func SessionPathFromRoot(sessionsRoot, sessionPID string) string {
	return filepath.Join(sessionsRoot, sessionPID)
}

// ParseDestination parses a string as a URI and if that fails, as a SSH URI.
func ParseDestination(destination string) (*url.URL, error) {
	destURL, err := url.Parse(destination)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	if destURL.Scheme != "" && destURL.Host != "" {
		return destURL, nil
	}

	destURL, err = url.Parse("ssh://" + destination)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return destURL, nil
}

// rawForwarderACK is the acknowledgement sent by "graftd raw" once it has connected
// to the daemon's Unix socket and is ready to forward traffic.
const rawForwarderACK = "ACK"

// readRawForwarderACK reads the ACK from a raw forwarder's stdout.
// Returns nil if the ACK was received, or an error otherwise.
func readRawForwarderACK(stdout io.Reader) error {
	var buf [len(rawForwarderACK)]byte

	if _, err := io.ReadFull(stdout, buf[:]); err != nil {
		return errors.WrapPrefix(err, "error reading ACK from raw forwarder")
	}

	if string(buf[:]) != rawForwarderACK {
		return errors.Errorf("unexpected response from raw forwarder: %q", string(buf[:]))
	}

	return nil
}

// connIOPipe is a simple read/write forwarder (really a io.ReadWriteCloser) across two streams.
type connIOPipe struct {
	reader io.Reader
	writer io.WriteCloser
}

func (conn connIOPipe) Read(b []byte) (int, error) {
	n, err := conn.reader.Read(b)
	if err != nil {
		return n, errors.Wrap(err)
	}

	return n, nil
}

func (conn connIOPipe) Write(b []byte) (int, error) {
	n, err := conn.writer.Write(b)
	if err != nil {
		return n, errors.Wrap(err)
	}

	return n, nil
}

func (conn connIOPipe) Close() error {
	// we will let this close via the command stop. hopefully that's okay!
	return nil
}

func (conn connIOPipe) LocalAddr() net.Addr {
	return &net.IPAddr{}
}

func (conn connIOPipe) RemoteAddr() net.Addr {
	return &net.IPAddr{}
}

func (conn connIOPipe) SetDeadline(_ time.Time) error {
	return nil
}

func (conn connIOPipe) SetReadDeadline(_ time.Time) error {
	return nil
}

func (conn connIOPipe) SetWriteDeadline(_ time.Time) error {
	return nil
}

// ResolveConnectionName constructs a connection name from a possible set name and os name. If neither
// are set, a UUID is used.
//
// TODO(erd): Replace UUID with human-readable naming scheme (e.g. adjective-noun).
func ResolveConnectionName(name, os string) string {
	if name == "" {
		name = os
	}

	if name == "" {
		name = uuid.NewString()
	}

	return name
}

// flushingWriter wraps a writer and tracks how many visual terminal rows
// have been consumed, accounting for line wrapping based on terminal width.
// Flush moves the cursor back to the start of the tracked output and clears it.
type flushingWriter struct {
	io.WriteCloser
	termWidth int
	rows      int
	col       int
}

func newFlushingWriter(w io.WriteCloser) *flushingWriter {
	termWidth, _, _ := term.GetSize(int(os.Stderr.Fd()))

	return &flushingWriter{WriteCloser: w, termWidth: termWidth}
}

func (w *flushingWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		switch b {
		case '\n':
			w.rows++
			w.col = 0
		case '\r':
			w.col = 0
		default:
			w.col++
			if w.termWidth > 0 && w.col >= w.termWidth {
				w.rows++
				w.col = 0
			}
		}
	}

	return w.WriteCloser.Write(p)
}

// Flush moves the cursor up to the start of the previously written output and clears it.
func (w *flushingWriter) Flush() {
	if w.rows > 0 {
		fmt.Fprintf(w.WriteCloser, "\033[%dA\033[J", w.rows)
	}

	w.rows = 0
	w.col = 0
}
