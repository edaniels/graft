package graft

import (
	"context"
	"net/url"

	"google.golang.org/grpc"
)

// A ConnectorFactory creates transport-level connectors for a given destination.
// Connectors are shared across all connections to the same remote host via remoteDaemon.
type ConnectorFactory interface {
	CreateConnector(
		ctx context.Context,
		destURL *url.URL,
		identity string,
	) (RemoteConnector, error)
}

type RemoteDaemonConnection interface {
	ClientConn() *grpc.ClientConn
	Close() error
}

// RemoteConnector is a transport-level abstraction for communicating with a remote host.
// It handles SSH/Docker connection setup, file copying, and command execution.
// A single RemoteConnector is shared by all connections to the same host via remoteDaemon.
type RemoteConnector interface {
	// Destination returns the URI for this destination.
	Destination() string

	// SafeDestination returns a display-safe destination string (no passwords/secrets).
	SafeDestination() string

	// Identity returns the local daemon identity used to namespace the remote daemon.
	Identity() string

	// InitializeRemote does connector specific initialization prior to daemon installation. If the
	// remote is already initialized, true is returned.
	InitializeRemote(initCtx context.Context) (bool, error)
	DeinitializeRemote(ctx context.Context) error

	// RunOneShotCommand starts a [RunningCommand] on the remote, waits for it to stop, and returns its output.
	// If the exit status is not 0, an error is returned.
	RunOneShotCommand(ctx context.Context, command string) (string, error)

	// ConnectToRemoteDaemon attempts to establish a gRPC connection to the daemon at the remote destination.
	// If the daemon is able to be connected to, the return boolean will be true and the connection
	// set; otherwise, the daemon may need reinstallation.
	ConnectToRemoteDaemon(
		ctx context.Context,
		remoteBinPath string,
		remoteSocketPath string,
	) (RemoteDaemonConnection, bool, error)

	// CopyFile copies the file at the local path to the remote path.
	CopyFile(
		ctx context.Context,
		localPath string,
		remotePath string,
		permissions string,
	) error

	// StateFields returns any fields that should be printed out in status update messages for this connector.
	StateFields() []any

	Close() error
}
