package graft

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// CheckStatus represents the outcome of a doctor check.
type CheckStatus int

const (
	// CheckPass indicates the check passed.
	CheckPass CheckStatus = iota
	// CheckWarn indicates a non-critical issue.
	CheckWarn
	// CheckFail indicates a critical failure.
	CheckFail
)

// CheckResult holds the outcome of a single doctor check.
type CheckResult struct {
	Name    string
	Status  CheckStatus
	Message string
	Details []string
}

// CheckShellActivation checks whether the graft shell session is active.
func CheckShellActivation(lookupEnv func(string) (string, bool)) CheckResult {
	sessStr, ok := lookupEnv("GRAFT_SESSION")
	if !ok {
		return CheckResult{
			Name:    "Shell activation",
			Status:  CheckWarn,
			Message: "not active (run 'eval $(graft shell)' to activate)",
		}
	}

	if _, err := strconv.ParseUint(sessStr, 10, 64); err != nil {
		return CheckResult{
			Name:    "Shell activation",
			Status:  CheckWarn,
			Message: fmt.Sprintf("malformed GRAFT_SESSION=%q", sessStr),
		}
	}

	return CheckResult{
		Name:    "Shell activation",
		Status:  CheckPass,
		Message: fmt.Sprintf("active (session PID: %s)", sessStr),
	}
}

// DaemonStatusFunc queries the local daemon for its status.
type DaemonStatusFunc func(ctx context.Context) (*graftv1.StatusResponse, error)

// CheckLocalDaemon checks whether the local daemon is running and healthy.
func CheckLocalDaemon(statusFn DaemonStatusFunc) CheckResult {
	resp, err := statusFn(context.Background())
	if err != nil {
		msg := err.Error()
		if errors.Is(err, ErrDaemonNotRunning) {
			msg = "not running (start with 'graft daemon')"
		}

		return CheckResult{
			Name:    "Local daemon",
			Status:  CheckFail,
			Message: msg,
		}
	}

	verStr := versionString(resp.GetVersionInfo())

	details := []string{
		"Version: " + verStr,
	}

	if resp.GetUptime() != nil {
		details = append(details, fmt.Sprintf("Uptime:  %s", resp.GetUptime().AsDuration()))
	}

	return CheckResult{
		Name:    "Local daemon",
		Status:  CheckPass,
		Message: fmt.Sprintf("running (%s)", verStr),
		Details: details,
	}
}

// CheckUpdates checks whether a newer version of graft is available.
func CheckUpdates(ctx context.Context, client ReleaseClient, currentVersion string) CheckResult {
	result, err := CheckForUpdate(ctx, client, currentVersion)
	if err != nil {
		return CheckResult{
			Name:    "Updates",
			Status:  CheckWarn,
			Message: fmt.Sprintf("could not check for updates: %s", err),
		}
	}

	if result.IsDevBuild {
		return CheckResult{
			Name:    "Updates",
			Status:  CheckWarn,
			Message: fmt.Sprintf("dev build (%s), cannot compare versions", currentVersion),
		}
	}

	if result.UpdateAvailable {
		cr := CheckResult{
			Name:    "Updates",
			Status:  CheckWarn,
			Message: fmt.Sprintf("update available: %s -> %s", currentVersion, result.LatestVersion),
		}
		if result.ReleaseNotes != "" {
			cr.Details = ReleaseNotesLines(result.ReleaseNotes)
		}

		return cr
	}

	return CheckResult{
		Name:    "Updates",
		Status:  CheckPass,
		Message: fmt.Sprintf("up to date (%s)", currentVersion),
	}
}

// ResolveSSHDetails resolves SSH configuration and returns detail lines for display.
func ResolveSSHDetails(hostAlias, fallbackPort, fallbackUser string, resolver sshConfigResolver) []string {
	resolved := resolveSSHConfig(resolver, hostAlias, fallbackPort, fallbackUser)

	details := []string{
		"Hostname: " + resolved.Hostname,
		"Port:     " + resolved.Port,
	}

	if resolved.User != "" {
		details = append(details, "User:     "+resolved.User)
	}

	for _, f := range resolved.IdentityFiles {
		if _, err := os.Stat(f); err != nil {
			continue
		}

		details = append(details, "Identity: "+f)
	}

	if resolved.ProxyCommand != "" {
		details = append(details, "Proxy:    "+resolved.ProxyCommand)
	}

	return details
}

// UDSProbeFunc probes whether Unix domain socket forwarding is supported over SSH.
// It should attempt to dial a Unix socket and return the error (or nil on success).
type UDSProbeFunc func() error

// CheckTransportMode determines whether UDS or stdio transport will be used.
func CheckTransportMode(probe UDSProbeFunc) CheckResult {
	err := probe()
	if err == nil {
		return CheckResult{
			Name:    "Transport",
			Status:  CheckPass,
			Message: "UDS (Unix domain socket forwarding supported)",
		}
	}

	var chanErr *ssh.OpenChannelError
	if errors.As(err, &chanErr) {
		//exhaustive:enforce
		switch chanErr.Reason {
		case ssh.ConnectionFailed:
			// Socket doesn't exist but UDS is supported.
			return CheckResult{
				Name:    "Transport",
				Status:  CheckPass,
				Message: "UDS (Unix domain socket forwarding supported)",
			}
		case ssh.Prohibited, ssh.UnknownChannelType:
			return CheckResult{
				Name:    "Transport",
				Status:  CheckWarn,
				Message: "stdio (Unix domain socket forwarding not supported, will use stdio tunnel)",
			}
		case ssh.ResourceShortage:
			// Fall through to unknown error.
		}
	}

	return CheckResult{
		Name:    "Transport",
		Status:  CheckWarn,
		Message: fmt.Sprintf("unable to determine transport mode: %s", err),
	}
}

// DoctorRemoteConnector is the subset of RemoteConnector needed by doctor checks.
type DoctorRemoteConnector interface {
	Identity() string
	RunOneShotCommand(ctx context.Context, command string) (string, error)
}

// RemoteDaemonConnectFunc connects to a remote daemon and returns its version info.
// Returns (version, running, error).
type RemoteDaemonConnectFunc func(ctx context.Context, binPath, socketPath string) (*graftv1.VersionInfo, bool, error)

// RemoteEnvironmentInfo holds discovered remote environment details.
type RemoteEnvironmentInfo struct {
	OS               string
	Arch             string
	HomeDir          string
	RemoteSocketPath string
}

// CheckRemoteEnvironment runs remote discovery to determine OS, arch, and home directory.
func CheckRemoteEnvironment(ctx context.Context, connector DoctorRemoteConnector) (CheckResult, RemoteEnvironmentInfo) {
	info, err := discoverRemote(ctx, doctorConnectorAdapter{connector})
	if err != nil {
		return CheckResult{
			Name:    "Remote environment",
			Status:  CheckFail,
			Message: err.Error(),
		}, RemoteEnvironmentInfo{}
	}

	return CheckResult{
		Name:    "Remote environment",
		Status:  CheckPass,
		Message: fmt.Sprintf("%s/%s", info.OS, info.Arch),
		Details: []string{
			"OS:   " + info.OS,
			"Arch: " + info.Arch,
			"Home: " + info.HomeDir,
		},
	}, RemoteEnvironmentInfo(info)
}

// CheckRemoteDaemon checks whether the remote daemon binary exists and is running.
func CheckRemoteDaemon(
	ctx context.Context,
	connector DoctorRemoteConnector,
	info RemoteEnvironmentInfo,
	connectFn RemoteDaemonConnectFunc,
	localVersion *graftv1.VersionInfo,
) CheckResult {
	binPath := filepath.Join(info.HomeDir, BinaryName(info.OS, info.Arch))
	if connector.Identity() != "" {
		binPath = filepath.Join(filepath.Dir(info.RemoteSocketPath), BinaryName(info.OS, info.Arch))
	}

	// Check if binary exists.
	_, statErr := connector.RunOneShotCommand(ctx, "stat "+binPath)
	if statErr != nil {
		return CheckResult{
			Name:    "Remote daemon",
			Status:  CheckWarn,
			Message: fmt.Sprintf("not installed (binary not found at %s)", binPath),
			Details: []string{"Binary: " + binPath},
		}
	}

	// Try connecting to the daemon.
	remoteVersion, online, connectErr := connectFn(ctx, binPath, info.RemoteSocketPath)
	if connectErr != nil || !online {
		return CheckResult{
			Name:    "Remote daemon",
			Status:  CheckWarn,
			Message: "not running (will be started on next 'graft connect')",
			Details: []string{"Binary: " + binPath},
		}
	}

	remVerStr := versionString(remoteVersion)

	diff := BuildVersionsEqual(localVersion, remoteVersion)
	if diff != "" {
		return CheckResult{
			Name:    "Remote daemon",
			Status:  CheckWarn,
			Message: fmt.Sprintf("running (%s), version mismatch with local (%s): %s", remVerStr, versionString(localVersion), diff),
			Details: []string{
				"Remote version: " + remVerStr,
				"Local version:  " + versionString(localVersion),
			},
		}
	}

	return CheckResult{
		Name:    "Remote daemon",
		Status:  CheckPass,
		Message: fmt.Sprintf("running (%s, matches local)", remVerStr),
	}
}

// CheckRemoteDirectories reports the remote directory paths (informational, always passes).
func CheckRemoteDirectories(info RemoteEnvironmentInfo) CheckResult {
	// Derive paths from the socket path, which already accounts for identity namespacing.
	// Socket lives at: <stateHome>/remote[/<identity>]/graftd.sock
	daemonDir := filepath.Dir(info.RemoteSocketPath)
	stateDir := filepath.Join(info.HomeDir, ".local", "state", "graft")
	configDir := filepath.Join(info.HomeDir, ".config", "graft")
	logsDir := filepath.Join(daemonDir, "logs")

	binPath := filepath.Join(info.HomeDir, BinaryName(info.OS, info.Arch))
	if daemonDir != filepath.Join(stateDir, "remote") {
		// Identity-namespaced: binary lives next to socket.
		binPath = filepath.Join(daemonDir, BinaryName(info.OS, info.Arch))
	}

	return CheckResult{
		Name:    "Remote directories",
		Status:  CheckPass,
		Message: "paths",
		Details: []string{
			"State:  " + stateDir,
			"Config: " + configDir,
			"Logs:   " + logsDir,
			"Socket: " + info.RemoteSocketPath,
			"Binary: " + binPath,
		},
	}
}

// DialDaemonSocket dials a graft daemon socket and returns the gRPC client connection.
func DialDaemonSocket(sockPath string) (*grpc.ClientConn, error) {
	clientConn, err := grpc.NewClient(
		"unix://"+sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return clientConn, nil
}

// DefaultSSHConfigResolver returns the default SSH config resolver.
func DefaultSSHConfigResolver() sshConfigResolver {
	return defaultSSHConfigResolver{}
}

// NewSSHConnectorFactory returns a new SSH connector factory.
func NewSSHConnectorFactory() ConnectorFactory {
	return newSSHConnectorFactory()
}

// doctorConnectorAdapter wraps DoctorRemoteConnector to satisfy the full RemoteConnector
// interface needed by discoverRemote. Only Identity and RunOneShotCommand are used.
type doctorConnectorAdapter struct {
	DoctorRemoteConnector
}

func (a doctorConnectorAdapter) Destination() string                        { return "" }
func (a doctorConnectorAdapter) SafeDestination() string                    { return "" }
func (a doctorConnectorAdapter) DeinitializeRemote(_ context.Context) error { return nil }
func (a doctorConnectorAdapter) CopyFile(_ context.Context, _, _, _ string) error {
	return nil
}

func (a doctorConnectorAdapter) ConnectToRemoteDaemon(
	_ context.Context, _, _ string,
) (RemoteDaemonConnection, bool, error) {
	return nil, false, nil
}

func (a doctorConnectorAdapter) StateFields() []any { return nil }
func (a doctorConnectorAdapter) Close() error       { return nil }

func (a doctorConnectorAdapter) InitializeRemote(_ context.Context) (bool, error) {
	return false, nil
}
