package graft

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bramvdbogaerde/go-scp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"google.golang.org/grpc"

	"github.com/edaniels/graft/errors"
)

const sshSchemeName = "ssh"

type sshConnectorFactory struct {
	dialer          net.Dialer
	sshConfigLookup sshConfigResolver
	// staticSigners, if set, replaces the normal signer resolution (agent + home keys).
	staticSigners []ssh.Signer
}

func newSSHConnectorFactory() ConnectorFactory {
	return &sshConnectorFactory{
		sshConfigLookup: defaultSSHConfigResolver{},
	}
}

// CreateConnector sets up a connector for ssh.
func (s *sshConnectorFactory) CreateConnector(
	_ context.Context, destURL *url.URL, identity string,
) (RemoteConnector, error) {
	return s.newSSHConnector(destURL, identity), nil
}

type sshConnector struct {
	identity string
	scheme   *sshConnectorFactory
	destURL  *url.URL

	sshClientMu sync.Mutex
	sshClient   *ssh.Client
}

// newSSHConnector returns an uninitialized ssh based [RemoteConnector].
func (s *sshConnectorFactory) newSSHConnector(destURL *url.URL, identity string) RemoteConnector {
	return &sshConnector{
		identity: identity,
		destURL:  destURL,
		scheme:   s,
	}
}

func (conn *sshConnector) Destination() string {
	return conn.destURL.String()
}

func (conn *sshConnector) SafeDestination() string {
	host := conn.destURL.Hostname()
	port := conn.destURL.Port()

	var user string
	if conn.destURL.User != nil {
		user = conn.destURL.User.Username()
	}

	result := host
	if user != "" {
		result = user + "@" + result
	}

	if port != "" && port != "22" {
		result = result + ":" + port
	}

	return result
}

func (conn *sshConnector) Identity() string {
	return conn.identity
}

// ProbeUDS attempts a single Unix domain socket dial to determine if the SSH server
// supports streamlocal forwarding. It dials a non-existent path and classifies the error.
func (conn *sshConnector) ProbeUDS(ctx context.Context) error {
	conn.sshClientMu.Lock()
	sshClient := conn.sshClient
	conn.sshClientMu.Unlock()

	if sshClient == nil {
		return errors.New("not connected to remote")
	}

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	probeConn, err := sshClient.DialContext(probeCtx, "unix", "/tmp/.graft-uds-probe-nonexistent")
	if err != nil {
		return errors.Wrap(err)
	}

	probeConn.Close()

	return nil
}

// InitializeRemote connects to the remote destination over SSH.
//
// TODO(erd): utilize already init?
func (conn *sshConnector) InitializeRemote(initCtx context.Context) (bool, error) {
	hostAlias := conn.destURL.Hostname()
	explicitPort := conn.destURL.Port()

	var explicitUser string
	if conn.destURL.User != nil {
		explicitUser = conn.destURL.User.Username()
	}

	resolved := resolveSSHConfig(conn.scheme.sshConfigLookup, hostAlias, explicitPort, explicitUser)

	if resolved.User == "" {
		currentUser, err := user.Current()
		if err != nil {
			return false, errors.WrapPrefix(err, "error determining current user")
		}

		resolved.User = currentUser.Username
	}

	// Update destURL to the fully resolved destination so that Destination()
	// and daemon key matching produce consistent, canonical values.
	conn.destURL.Host = net.JoinHostPort(resolved.Hostname, resolved.Port)
	conn.destURL.User = url.User(resolved.User)

	resolvedDestination := net.JoinHostPort(resolved.Hostname, resolved.Port)

	logger := slog.With("destination", conn.destURL.String())

	signers := conn.scheme.getAllSSHSigners(initCtx)
	signers = append(signers, conn.scheme.getIdentityFileSigners(logger, resolved.IdentityFiles)...)

	clientConfig := &ssh.ClientConfig{
		User: resolved.User,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signers...),
		},
		// TODO(erd): use host keys
		//nolint:gosec
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	clientConfig.SetDefaults()

	var (
		sshConn net.Conn
		err     error
	)

	if resolved.ProxyCommand != "" {
		logger.DebugContext(initCtx, "using ProxyCommand for SSH connection", "command", resolved.ProxyCommand)
		// Use context.Background() so the ProxyCommand process outlives initCtx.
		// The process is cleaned up via sshConn.Close() -> proxyCommandConn.Close() -> SIGKILL process group,
		// which is triggered by ssh.Client.Close() -> sshClientConn.Close() -> sshConn.Close().
		sshConn, err = dialProxyCommand(context.Background(), logger, resolved.ProxyCommand)
	} else {
		sshConn, err = conn.scheme.dialer.DialContext(initCtx, "tcp", resolvedDestination)
	}

	if err != nil {
		slog.ErrorContext(initCtx, "error dialing destination", "dest", resolvedDestination, "error", err)

		return false, errors.Wrap(err)
	}

	sshClientConn, chans, reqs, err := ssh.NewClientConn(sshConn, resolvedDestination, clientConfig)
	if err != nil {
		sshConn.Close()
		slog.ErrorContext(initCtx, "error making new ssh client connection", "error", err)

		return false, errors.Wrap(err)
	}

	client := ssh.NewClient(sshClientConn, chans, reqs)

	conn.sshClientMu.Lock()
	conn.sshClient = client
	conn.sshClientMu.Unlock()

	return false, nil
}

// CopyFile copies over graft and starts it up as a daemon.
func (conn *sshConnector) CopyFile(
	ctx context.Context,
	localPath string,
	remotePath string,
	permissions string,
) error {
	conn.sshClientMu.Lock()
	sshClient := conn.sshClient
	conn.sshClientMu.Unlock()

	pathFile, err := os.Open(localPath)
	if err != nil {
		slog.ErrorContext(ctx, "error opening file", "error", err)

		return errors.Wrap(err)
	}

	scpClient, err := scp.NewClientBySSH(sshClient)
	if err != nil {
		slog.ErrorContext(ctx, "error making scp client", "error", err)

		return errors.Wrap(err)
	}
	defer scpClient.Close()

	slog.DebugContext(ctx, "copying file to remote via SCP", "local_path", localPath, "remote_path", remotePath)

	if copyErr := scpClient.CopyFile(ctx, pathFile, remotePath, "0"+permissions); copyErr != nil {
		slog.ErrorContext(ctx, "error copying file", "error", copyErr)

		return errors.Wrap(copyErr)
	}

	return nil
}

// ConnectToRemoteDaemon attempts to establish a unix domain socket based gRPC connection to the daemon in the container.
func (conn *sshConnector) ConnectToRemoteDaemon(
	ctx context.Context,
	remoteBinPath string,
	remoteSocketPath string,
) (RemoteDaemonConnection, bool, error) {
	conn.sshClientMu.Lock()
	sshClient := conn.sshClient
	conn.sshClientMu.Unlock()

	if sshClient == nil {
		return nil, false, errors.New("not connected to remote")
	}

	const tempDialTimeout = 3 * time.Second

	tempDialCtx, cancelTempDial := context.WithTimeout(ctx, tempDialTimeout)
	defer cancelTempDial()

	const maxInitRemoteSocketAttempts = 5

	var err error

	for i := range maxInitRemoteSocketAttempts {
		remoteConn, dialErr := sshClient.DialContext(tempDialCtx, "unix", remoteSocketPath)
		if dialErr == nil {
			clientConn, grpcErr := remoteConnToGRPCClientConn(remoteConn)
			if grpcErr != nil {
				return nil, false, errors.WrapPrefix(grpcErr, "unlikely: error turning ssh unix conn into gRPC Client Connection")
			}

			return sshRemoteDaemonConnection{clientConn, nil}, true, nil
		}

		err = dialErr

		if i+1 == maxInitRemoteSocketAttempts {
			break
		}

		var chanErr *ssh.OpenChannelError
		if errors.As(err, &chanErr) {
			//exhaustive:enforce
			switch chanErr.Reason {
			case ssh.ConnectionFailed:
				// TODO(erd): need to check for "(open failed)" substring or is this enough?
				// daemon is unavailable; this could trigger a reinstall
				return nil, false, nil
			case ssh.Prohibited, ssh.UnknownChannelType:
				// Server doesn't support Unix socket forwarding.
				// Fall back to connecting via an SSH exec session.
				slog.DebugContext(ctx, "unix forwarding not supported; falling back to stdio tunnel",
					"reason", chanErr.Reason)

				return conn.connectViaStdioTunnel(tempDialCtx, sshClient, remoteBinPath)
			case ssh.ResourceShortage:
			}
		}

		if !IsCanceledError(err) {
			slog.DebugContext(ctx, "error dialing remote daemon; trying again", "error", err)
		}

		timer := time.NewTimer(time.Second)
		select {
		case <-timer.C:
		case <-tempDialCtx.Done():
			timer.Stop()

			return nil, false, errors.Wrap(context.Cause(tempDialCtx))
		}
	}

	if err != nil {
		return nil, false, err
	}

	return nil, false, errors.New("too many attempts dialing")
}

// connectViaStdioTunnel runs "graft raw" over an SSH exec session, which connects to the
// daemon's Unix socket and pipes it over stdin/stdout. This is the same approach used by the
// Docker connector and works when the SSH server doesn't support Unix socket forwarding.
func (conn *sshConnector) connectViaStdioTunnel(
	ctx context.Context,
	sshClient *ssh.Client,
	remoteBinPath string,
) (RemoteDaemonConnection, bool, error) {
	sess, err := sshClient.NewSession()
	if err != nil {
		return nil, false, errors.WrapPrefix(err, "error creating SSH session for stdio tunnel")
	}

	stdinPipe, err := sess.StdinPipe()
	if err != nil {
		sess.Close()

		return nil, false, errors.WrapPrefix(err, "error getting stdin pipe for stdio tunnel")
	}

	stdoutPipe, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()

		return nil, false, errors.WrapPrefix(err, "error getting stdout pipe for stdio tunnel")
	}

	rawCmd := remoteBinPath + " raw"
	if conn.identity != "" {
		rawCmd += " " + conn.identity
	}

	if err := sess.Start(rawCmd); err != nil {
		sess.Close()

		return nil, false, errors.WrapPrefix(err, "error starting raw tunnel")
	}

	if err := readRawForwarderACK(stdoutPipe); err != nil {
		sess.Close()

		return nil, false, err
	}

	slog.DebugContext(ctx, "established stdio tunnel to remote daemon via raw forwarder")

	tunnelConn := &stdioTunnelConn{
		connIOPipe: connIOPipe{
			reader: stdoutPipe,
			writer: stdinPipe,
		},
		sess: sess,
	}

	clientConn, grpcErr := remoteConnToGRPCClientConn(tunnelConn)
	if grpcErr != nil {
		tunnelConn.Close()

		return nil, false, errors.WrapPrefix(grpcErr, "error creating gRPC client over stdio tunnel")
	}

	return sshRemoteDaemonConnection{clientConn, tunnelConn}, true, nil
}

// stdioTunnelConn wraps an SSH session's stdin/stdout as a net.Conn.
type stdioTunnelConn struct {
	connIOPipe

	sess *ssh.Session
}

func (s *stdioTunnelConn) Close() error {
	var errs []error

	if s.writer != nil {
		if err := s.writer.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if s.sess != nil {
		if err := s.sess.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

type sshRemoteDaemonConnection struct {
	clientConn *grpc.ClientConn
	tunnelConn *stdioTunnelConn // non-nil when using stdio tunnel fallback
}

func (conn sshRemoteDaemonConnection) ClientConn() *grpc.ClientConn {
	return conn.clientConn
}

func (conn sshRemoteDaemonConnection) Close() error {
	grpcErr := conn.clientConn.Close()

	var tunnelErr error
	if conn.tunnelConn != nil {
		tunnelErr = conn.tunnelConn.Close()
	}

	return errors.Join(grpcErr, tunnelErr)
}

func (conn *sshConnector) StateFields() []any {
	conn.sshClientMu.Lock()
	sshClient := conn.sshClient
	conn.sshClientMu.Unlock()

	if sshClient == nil {
		return nil
	}

	return []any{
		"addr", sshClient.RemoteAddr(),
		"user", sshClient.User(),
	}
}

// Close ends the underlying gRPC and client connection.
func (conn *sshConnector) Close() error {
	conn.sshClientMu.Lock()
	sshClient := conn.sshClient
	conn.sshClient = nil
	conn.sshClientMu.Unlock()

	if sshClient == nil {
		return nil
	}

	if err := sshClient.Close(); err != nil {
		return errors.WrapPrefix(err, "error closing SSH client")
	}

	return nil
}

// DeinitializeRemote just closes the connection for now, but does not destroy the daemon.
func (conn *sshConnector) DeinitializeRemote(_ context.Context) error {
	// TODO(erd): Determine if remote daemon should also be destroyed on disconnect.
	return conn.Close()
}

// RunOneShotCommand runs a command via the ssh client.
func (conn *sshConnector) RunOneShotCommand(
	_ context.Context,
	command string,
) (string, error) {
	conn.sshClientMu.Lock()
	sshClient := conn.sshClient
	conn.sshClientMu.Unlock()

	if sshClient == nil {
		return "", errors.New("not connected to remote")
	}

	sess, err := sshClient.NewSession()
	if err != nil {
		return "", errors.Wrap(err)
	}
	defer sess.Close()

	ret, err := sess.CombinedOutput(command)
	if err != nil {
		if len(ret) == 0 {
			return "", errors.Wrap(err)
		}

		return "", errors.WrapSuffix(err, string(ret))
	}

	return string(ret), nil
}

// getAgentSSHSigners returns signers that an ssh-agent can provide.
func (s *sshConnectorFactory) getAgentSSHSigners(ctx context.Context) []ssh.Signer {
	// TODO(erd): Verify if sock connection requires explicit close.
	sock, err := s.dialer.DialContext(ctx, "unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		slog.ErrorContext(ctx, "error dialing ssh auth sock", "error", err)

		return nil
	}

	agent := agent.NewClient(sock)

	agentSigners, err := agent.Signers()
	if err != nil {
		slog.ErrorContext(ctx, "error getting ssh agent signers", "error", err)

		return nil
	}

	return agentSigners
}

// getHomeSSHSigners returns signers that are in the .ssh path.
//
// TODO(erd): there's probably a better way to determine this.
func (s *sshConnectorFactory) getHomeSSHSigners() []ssh.Signer {
	sshPath, err := SSHPath()
	if err != nil {
		return nil
	}

	entries, err := os.ReadDir(sshPath)
	if err != nil {
		return nil
	}

	paths := make([]string, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		paths = append(paths, filepath.Join(sshPath, entry.Name()))
	}

	return signersFromKeyFiles(slog.Default(), paths)
}

// getAllSSHSigners returns the SSH signers that can be used for the current user. This includes
// what's in the home directory and provided by the ssh-agent.
func (s *sshConnectorFactory) getAllSSHSigners(ctx context.Context) []ssh.Signer {
	if len(s.staticSigners) > 0 {
		return s.staticSigners
	}

	allSigners := s.getHomeSSHSigners()
	allSigners = append(allSigners, s.getAgentSSHSigners(ctx)...)

	return allSigners
}

// getIdentityFileSigners returns signers from the given identity file paths.
func (s *sshConnectorFactory) getIdentityFileSigners(logger *slog.Logger, identityFiles []string) []ssh.Signer {
	return signersFromKeyFiles(logger, identityFiles)
}

// signersFromKeyFiles reads SSH private keys from the given file paths and returns signers.
func signersFromKeyFiles(logger *slog.Logger, paths []string) []ssh.Signer {
	signers := make([]ssh.Signer, 0, len(paths))

	for _, f := range paths {
		rd, err := os.ReadFile(f)
		if err != nil {
			logger.Debug("skipping identity file", "path", f, "error", err)

			continue
		}

		privKey, err := ssh.ParseRawPrivateKey(rd)
		if err != nil {
			logger.Debug("skipping identity file; failed to parse key", "path", f, "error", err)

			continue
		}

		fileSigner, err := ssh.NewSignerFromKey(privKey)
		if err != nil {
			logger.Debug("skipping identity file; failed to create signer", "path", f, "error", err)

			continue
		}

		signers = append(signers, fileSigner)
	}

	return signers
}

// proxyCommandConn wraps a connIOPipe and an exec.Cmd to provide a net.Conn
// backed by a ProxyCommand subprocess.
type proxyCommandConn struct {
	connIOPipe

	cmd       *exec.Cmd
	closeOnce sync.Once
	closeErr  error
}

func (p *proxyCommandConn) Close() error {
	p.closeOnce.Do(func() {
		var errs []error

		if p.writer != nil {
			if err := p.writer.Close(); err != nil {
				errs = append(errs, err)
			}
		}

		if p.cmd != nil && p.cmd.Process != nil {
			// Kill the entire process group so child processes are also terminated.
			//nolint:errcheck
			syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)

			// Reap the process in a goroutine to avoid blocking if child processes
			// inherited the pipes (common with ProxyCommand that spawns subprocesses).
			go func() {
				//nolint:errcheck
				p.cmd.Wait()
			}()
		}

		p.closeErr = errors.Join(errs...)
	})

	return p.closeErr
}

// dialProxyCommand spawns a ProxyCommand and returns a net.Conn using its stdin/stdout.
func dialProxyCommand(ctx context.Context, logger *slog.Logger, command string) (net.Conn, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.WrapPrefix(err, "error getting stdin pipe for ProxyCommand")
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.WrapPrefix(err, "error getting stdout pipe for ProxyCommand")
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, errors.WrapPrefix(err, "error getting stderr pipe for ProxyCommand")
	}

	if err := cmd.Start(); err != nil {
		return nil, errors.WrapPrefix(err, "error starting ProxyCommand")
	}

	// Log ProxyCommand stderr in the background for diagnostics.
	go func() {
		data, err := io.ReadAll(stderrPipe)
		if err != nil {
			logger.Debug("error reading ProxyCommand stderr", "error", err)

			return
		}

		if len(data) > 0 {
			logger.Debug("ProxyCommand stderr output", "stderr", string(data))
		}
	}()

	return &proxyCommandConn{
		connIOPipe: connIOPipe{
			reader: stdout,
			writer: stdin,
		},
		cmd: cmd,
	}, nil
}
