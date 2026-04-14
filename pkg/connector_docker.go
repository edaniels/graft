package graft

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	"github.com/moby/go-archive"
	"github.com/moby/go-archive/compression"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/jsonmessage"
	"google.golang.org/grpc"

	"github.com/edaniels/graft/errors"
)

const dockerSchemeName = "docker"

type dockerConnectorFactory struct {
	initOnce     sync.Once
	dockerClient *client.Client
	initErr      error
}

var errUnsupportedDockerPlatform = errors.NewBare("do not know how to start docker on platform")

// newDockerConnectorFactory returns a ConnectorFactory for docker connections. The factory is lazy -
// it does not attempt to connect to or start Docker until the first CreateConnector call.
func newDockerConnectorFactory() ConnectorFactory {
	return &dockerConnectorFactory{}
}

// ensureDockerRunning initializes the docker client and ensures the docker engine is running,
// starting it if necessary. It is called lazily on the first CreateConnector call.
func (s *dockerConnectorFactory) ensureDockerRunning(ctx context.Context) error {
	s.initOnce.Do(func() {
		s.dockerClient, s.initErr = initDockerClient(ctx)
	})

	return s.initErr
}

// initDockerClient creates a docker client and ensures the engine is reachable,
// starting it if necessary on supported platforms.
func initDockerClient(ctx context.Context) (*client.Client, error) {
	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		slog.ErrorContext(ctx, "error creating new client", "error", err)

		return nil, errors.Wrap(err)
	}

	_, infoErr := dockerClient.Info(ctx, client.InfoOptions{})
	if infoErr == nil {
		return dockerClient, nil
	}

	if !client.IsErrConnectionFailed(infoErr) {
		return nil, errors.Wrap(infoErr)
	}

	if err := startDockerEngine(ctx, dockerClient); err != nil {
		return nil, err
	}

	return dockerClient, nil
}

// startDockerEngine attempts to start the docker engine and waits for it to become reachable.
func startDockerEngine(ctx context.Context, dockerClient *client.Client) error {
	slog.InfoContext(ctx, "trying to start docker")

	switch runtime.GOOS {
	case osDarwin:
		return startDockerDarwin(ctx, dockerClient)
	default:
		return errors.WrapSuffix(errUnsupportedDockerPlatform, runtime.GOOS)
	}
}

func startDockerDarwin(ctx context.Context, dockerClient *client.Client) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if context.Cause(ctx) != nil {
			return errors.Wrap(context.Cause(ctx))
		}

		output, err := exec.CommandContext(ctx, "open", "-j", "-g", "-a", "Docker").CombinedOutput()
		if err != nil {
			return errors.Wrap(err)
		}

		if len(output) != 0 {
			slog.DebugContext(ctx, "open Docker output", "output", string(output))
		}

		select {
		case <-ctx.Done():
			return errors.Wrap(context.Cause(ctx))
		case <-ticker.C:
		}

		if _, infoErr := dockerClient.Info(ctx, client.InfoOptions{}); infoErr == nil {
			return nil
		}

		slog.DebugContext(ctx, "cannot connect to docker yet")
	}
}

// CreateConnector sets up an uninitialized connector for docker. The intended image is not validated
// for existence at this stage. The container name is extracted from the destURL "containerName"
// query parameter. On the first call, this ensures the docker engine is running.
func (s *dockerConnectorFactory) CreateConnector(
	ctx context.Context, destURL *url.URL, identity string,
) (RemoteConnector, error) {
	if err := s.ensureDockerRunning(ctx); err != nil {
		return nil, err
	}

	return newDockerConnector(destURL, identity, s.dockerClient)
}

type dockerConnector struct {
	identity      string
	dockerClient  *client.Client
	containerName string
	imageTag      string

	mu          sync.Mutex
	containerID string
	destURL     *url.URL
}

// newDockerConnector returns an uninitialized docker based [Connection]. The destURL must have
// an "imageTag" query param and optionally a "containerName" query param.
func newDockerConnector(
	destURL *url.URL,
	identity string,
	dockerClient *client.Client,
) (RemoteConnector, error) {
	imageTag := destURL.Query().Get("imageTag")
	if imageTag == "" {
		return nil, errors.New("destination URL missing imageTag query param")
	}

	containerName := destURL.Query().Get("containerName")
	if containerName == "" {
		containerName = imageTag
	}

	return &dockerConnector{
		identity:      identity,
		dockerClient:  dockerClient,
		containerName: containerName,
		imageTag:      imageTag,
		containerID:   destURL.Host,
		destURL:       destURL,
	}, nil
}

func (conn *dockerConnector) Destination() string {
	return conn.destURL.String()
}

func (conn *dockerConnector) SafeDestination() string {
	return conn.Destination()
}

func (conn *dockerConnector) Identity() string {
	return conn.identity
}

// InitializeRemote pulls the image identified in the destination URL and creates the container,
// If any error happens along the way, the container is deleted.
func (conn *dockerConnector) InitializeRemote(initCtx context.Context) (bool, error) {
	conn.mu.Lock()
	containerID := conn.containerID
	conn.mu.Unlock()

	if containerID != "" {
		// check container exists
		inspectResp, err := conn.dockerClient.ContainerInspect(initCtx, containerID, client.ContainerInspectOptions{})
		if err != nil {
			slog.ErrorContext(initCtx, "error inspecting container", "error", err)

			return false, errors.Wrap(err)
		}

		if !inspectResp.Container.State.Running {
			if _, err := conn.dockerClient.ContainerStart(initCtx, containerID, client.ContainerStartOptions{}); err != nil {
				return false, errors.Wrap(err)
			}
		}

		conn.mu.Lock()
		// TODO(erd): Verify that mutating destURL.Host under lock is safe w.r.t. concurrent existence checks.
		conn.destURL.Host = containerID
		conn.mu.Unlock()

		return true, nil
	}

	ref, err := reference.ParseNormalizedNamed(conn.imageTag)
	if err != nil {
		return false, errors.Wrap(err)
	}

	fullName := reference.FamiliarString(reference.TagNameOnly(ref))

	summaries, err := conn.dockerClient.ImageList(initCtx, client.ImageListOptions{
		All: true,
	})
	if err != nil {
		slog.ErrorContext(initCtx, "error listing images", "error", err)

		return false, errors.Wrap(err)
	}

	var foundImage bool

	for _, sum := range summaries.Items {
		if slices.Contains(sum.RepoTags, fullName) {
			foundImage = true

			break
		}
	}

	if !foundImage {
		slog.DebugContext(initCtx, "need to pull image", "image", conn.imageTag)

		out, pullErr := conn.dockerClient.ImagePull(initCtx, conn.imageTag, client.ImagePullOptions{})
		if pullErr != nil {
			slog.ErrorContext(initCtx, "error pulling image", "image", conn.imageTag, "error", pullErr)

			return false, errors.Wrap(pullErr)
		}

		//nolint:errcheck
		jsonmessage.DisplayJSONMessagesStream(out, OOBWriterFromContext(initCtx), 0, true, nil)
	} else {
		slog.DebugContext(initCtx, "already have image", "image", conn.imageTag)
	}

	createResp, err := conn.dockerClient.ContainerCreate(initCtx, client.ContainerCreateOptions{
		Name: conn.containerName,
		Config: &container.Config{
			Image: conn.imageTag,
			Tty:   true,
			// TODO(erd): Make default shell command configurable.
			Cmd: []string{"bash"},
		},
	})
	if err != nil {
		slog.ErrorContext(initCtx, "error creating container", "error", err)

		return false, errors.Wrap(err)
	}

	slog.DebugContext(initCtx, "created container", "container_id", createResp.ID, "response", createResp)
	containerID = createResp.ID

	// Record the containerID immediately so DeinitializeRemote can clean it up
	// if a subsequent step (e.g. ContainerStart) fails.
	conn.mu.Lock()
	conn.containerID = containerID
	conn.destURL.Host = containerID
	conn.mu.Unlock()

	if _, err := conn.dockerClient.ContainerStart(initCtx, containerID, client.ContainerStartOptions{}); err != nil {
		return false, errors.Wrap(err)
	}

	return false, nil
}

func (conn *dockerConnector) Close() error {
	return nil
}

// ConnectToRemoteDaemon attempts to establish a unix domain socket based gRPC connection to the daemon in the container.
// The way we do this is kind of odd: the daemon itself has a `raw` mode where it itself is dialing the UDS and forwarding
// the data for us. We could use something like socat for this (or something better?) but it's kind of convenient that it's
// all encapsulated within graft.
//

func (conn *dockerConnector) ConnectToRemoteDaemon(
	ctx context.Context,
	remoteBinPath string,
	_ string,
) (RemoteDaemonConnection, bool, error) {
	// raw gives us a UDS connection to the daemon
	cmd := []string{remoteBinPath, "raw"}
	if conn.identity != "" {
		cmd = append(cmd, conn.identity)
	}

	slog.DebugContext(ctx, "running docker command", "command", cmd)

	runningCmd, err := conn.runContainerExec(ctx, cmd, false)
	if err != nil {
		return nil, false, err
	}

	var success bool

	defer func() {
		if !success {
			if _, stopErr := StopCommand(runningCmd); stopErr != nil {
				slog.DebugContext(ctx, "connectToRemoteDaemon: error stopping command", "error", stopErr)
			}

			return
		}
	}()

	// racy when current value being read but probably okay
	var (
		stderrMu  sync.Mutex
		stderr    []byte
		stderrErr error
	)

	go func() {
		var buf [256]byte
		for {
			n, stderrReadErr := runningCmd.Stderr().Read(buf[:])
			if n != 0 {
				stderrMu.Lock()

				stderr = append(stderr, buf[:n]...)

				stderrMu.Unlock()
			}

			if stderrReadErr != nil {
				stderrMu.Lock()

				stderrErr = stderrReadErr

				stderrMu.Unlock()

				return
			}
		}
	}()

	if ackErr := readRawForwarderACK(runningCmd.Stdout()); ackErr != nil {
		stderrMu.Lock()

		currStderr := stderr
		currStderrErr := stderrErr

		stderrMu.Unlock()

		if currStderrErr != nil {
			slog.ErrorContext(ctx, "connectToRemoteDaemon: failed to read ACK and stderr", "error", ackErr)

			return nil, false, errors.New("failed to connect to remote daemon")
		}

		if len(currStderr) > 0 {
			// TODO(erd): validate the error here to determine if it's really okay to continue
			currStderrErr = errors.New(string(currStderr))
			slog.ErrorContext(ctx, "connectToRemoteDaemon: error connecting", "error", currStderrErr)

			return nil, true, currStderrErr
		}

		slog.ErrorContext(ctx, "connectToRemoteDaemon: error reading ACK", "error", ackErr)

		return nil, false, ackErr
	}

	clientConn, grpcErr := remoteConnToGRPCClientConn(&connIOPipe{
		reader: runningCmd.Stdout(),
		writer: runningCmd.Stdin(),
	})
	if grpcErr != nil {
		return nil, false, errors.WrapPrefix(grpcErr, "unlikely: error turning UDS pipe into gRPC Client Connection")
	}

	success = true

	return dockerRemoteDaemonConnection{clientConn, runningCmd}, true, nil
}

type dockerRemoteDaemonConnection struct {
	clientConn *grpc.ClientConn
	udsCmd     RunningCommand
}

func (conn dockerRemoteDaemonConnection) ClientConn() *grpc.ClientConn {
	return conn.clientConn
}

func (conn dockerRemoteDaemonConnection) Close() error {
	_, err := StopCommand(conn.udsCmd)

	return err
}

// CopyFile copies the contents of local file to the docker container at the remote path.
func (conn *dockerConnector) CopyFile(
	ctx context.Context,
	localPath string,
	remotePath string,
	permissions string,
) error {
	conn.mu.Lock()
	containerID := conn.containerID
	conn.mu.Unlock()

	tarredContents, err := archive.TarWithOptions(filepath.Dir(localPath), &archive.TarOptions{
		IncludeFiles:     []string{filepath.Base(localPath)},
		Compression:      compression.Gzip,
		RebaseNames:      map[string]string{filepath.Base(localPath): filepath.Base(remotePath)},
		IncludeSourceDir: true,
	})
	if err != nil {
		slog.ErrorContext(ctx, "error tarring file", "local_path", localPath, "error", err)

		return errors.Wrap(err)
	}

	slog.DebugContext(ctx, "copying file in tar to container", "remote_path", remotePath)

	if _, copyErr := conn.dockerClient.CopyToContainer(
		ctx,
		containerID,
		client.CopyToContainerOptions{
			DestinationPath: filepath.Dir(remotePath),
			Content:         tarredContents,
		},
	); copyErr != nil {
		slog.ErrorContext(ctx, "error copying file", "error", copyErr)

		return errors.Wrap(copyErr)
	}

	cmd := fmt.Sprintf("chmod %s %s", permissions, remotePath)
	slog.DebugContext(ctx, "running", "cmd", cmd)

	if _, runErr := conn.RunOneShotCommand(ctx, cmd); runErr != nil {
		slog.ErrorContext(ctx, "error changing permissions on file", "error", runErr)

		return runErr
	}

	return nil
}

// RunOneShotCommand runs a command via the docker client.
func (conn *dockerConnector) RunOneShotCommand(
	ctx context.Context,
	command string,
) (string, error) {
	return conn.runOneShotCommand(ctx, command, true)
}

// runOneShotCommand starts a [RunningCommand] for docker, waits for it to stop, and returns its output.
//
// TODO(erd): Redesign to avoid RunningCommand interface; current approach creates unnecessary complexity
// in signal handling and output extraction.
func (conn *dockerConnector) runOneShotCommand(
	ctx context.Context,
	command string,
	// TODO(erd): The shouldStop parameter is confusing; if we only ever set it to false for a kill itself,
	// this should be a separate command.
	shouldStop bool,
) (string, error) {
	runningCmd, err := conn.runContainerExec(
		ctx,
		[]string{defaultShellPath, "-c", fmt.Sprintf(`"%s"`, command)}, // quoted becauase it gets wrapped below
		true,
	)
	if err != nil {
		return "", err
	}

	// Note(erd): this pid is the wrapped command pid, not the command itself
	slog.DebugContext(ctx, "command running", "command", command)

	if shouldStop {
		defer func() {
			if _, stopErr := StopCommand(runningCmd); stopErr != nil {
				slog.DebugContext(ctx, "error stopping command", "error", stopErr)
			}
		}()
	}

	if closeErr := runningCmd.Stdin().Close(); closeErr != nil {
		return "", errors.Wrap(closeErr)
	}

	var (
		stderr    []byte
		stderrErr error
	)

	stderrReadDone := make(chan struct{})

	go func() {
		stderr, stderrErr = io.ReadAll(runningCmd.Stderr())

		close(stderrReadDone)
	}()

	var (
		stdout    []byte
		stdoutErr error
	)

	stdoutReadDone := make(chan struct{})

	go func() {
		stdout, stdoutErr = io.ReadAll(runningCmd.Stdout())

		close(stdoutReadDone)
	}()

	<-stdoutReadDone
	<-stderrReadDone

	exitStatus, err := runningCmd.Wait()
	if err != nil {
		return "", err
	}

	// TODO(erd): Consider separating stdout and stderr instead of concatenating.
	finalOutput := string(stdout) + string(stderr)

	finalErr := errors.Join(stdoutErr, stderrErr)
	if exitStatus != 0 {
		return "", errors.Join(errors.Errorf("exit-status %d: %s", exitStatus, finalOutput), finalErr)
	}

	return finalOutput, finalErr
}

// dockerWrapper is used to run a command while grabing the docker native PID (so it can be later killed).
//
// Note(erd): I wrote this and barely understand the redirection. It may be better done through a small C
// program. AFAIK, we need to do the subcommand to grab the PID and then because we do the subcommand, we
// need to do this crazy fifo pipe thing to extract stdout/err separately. I think this is very much worth
// revisiting in the future and I wouldn't be surprised if it causes some bugs.
//
// Another motivator for something in C/go is that this relies on some shell-fu and we may not have shell.
var dockerWrapper = `mkdir -p /tmp
stdout=$(mktemp -u)
mkfifo -m 600 "$stdout"
stderr=$(mktemp -u)
mkfifo -m 600 "$stderr"
{ %s <&3 3<&- 1>$stdout 2>$stderr & } 3<&0
PID=$!
echo $!
cat $stdout &
cat $stderr 1>&2 &
wait $PID
`

// runContainerExec takes a command and actually executes it on the docker container via ExecCreate (docker container exec).
// A [RunningCommand] is returned that can be used to handle the input/output.
func (conn *dockerConnector) runContainerExec(
	ctx context.Context,
	cmd []string,
	oneShot bool,
) (*DockerRunningCommand, error) {
	conn.mu.Lock()
	dockerClient := conn.dockerClient
	conn.mu.Unlock()

	wrappedCmd := []string{defaultShellPath, "-c", fmt.Sprintf(dockerWrapper, strings.Join(cmd, " "))}

	cmdExec, err := dockerClient.ExecCreate(ctx, conn.containerID, client.ExecCreateOptions{
		AttachStdin:  !oneShot,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          wrappedCmd,
		// TODO(erd): Determine whether to preserve parent environment variables.
		Env: []string{"TERM=xterm-256color"},
	})
	if err != nil {
		return nil, errors.Wrap(err)
	}

	hijacked, err := dockerClient.ExecAttach(ctx, cmdExec.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, errors.Wrap(err)
	}

	var stdin io.WriteCloser
	if oneShot {
		stdin = noopWriteCloser{io.Discard}
	} else {
		stdin = hijacked.Conn
	}

	// TODO(erd): Passing function callbacks suggests RunningCommand is the wrong interface for docker exec.
	runningCmd, err := NewDockerRunningCommand(
		hijacked.HijackedResponse,
		//nolint:contextcheck // parent ctx will die
		func() (client.ExecInspectResult, error) {
			return dockerClient.ExecInspect(context.Background(), cmdExec.ID, client.ExecInspectOptions{})
		},
		//nolint:contextcheck // parent ctx will die
		func(opts client.ContainerResizeOptions) error {
			if _, resizeErr := dockerClient.ExecResize(context.Background(), cmdExec.ID, client.ExecResizeOptions(opts)); resizeErr != nil {
				return errors.Wrap(resizeErr)
			}

			return nil
		},
		//nolint:contextcheck // parent ctx will die
		func(sig, pid string) (string, error) {
			// TODO(erd): Use Docker API directly for signal handling instead of runOneShotCommand.
			return conn.runOneShotCommand(context.Background(), fmt.Sprintf("kill -s %s %s", sig, pid), false)
		},
		stdin,
		false,
	)
	if err != nil {
		return nil, err
	}

	return runningCmd, nil
}

type noopWriteCloser struct {
	io.Writer
}

func (w noopWriteCloser) Close() error {
	return nil
}

func (conn *dockerConnector) StateFields() []any {
	return nil
}

// DeinitializeRemote closes the connection and removes the underlying container.
// Removes by containerID if known, otherwise falls back to removing by
// containerName so partially initialized containers aren't leaked.
func (conn *dockerConnector) DeinitializeRemote(ctx context.Context) error {
	conn.mu.Lock()
	containerID := conn.containerID
	conn.containerID = ""
	containerName := conn.containerName
	conn.mu.Unlock()

	target := containerID
	if target == "" {
		target = containerName
	}

	if target == "" {
		return nil
	}

	if _, err := conn.dockerClient.ContainerRemove(ctx, target, client.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}); err != nil {
		if strings.Contains(err.Error(), "No such container") {
			return nil
		}

		return errors.WrapPrefix(err, "error removing container")
	}

	return nil
}
