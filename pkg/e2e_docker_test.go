package graft

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.viam.com/test"
	"golang.org/x/crypto/ssh"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

const (
	e2eImageName     = "graft-e2e-test"
	e2eContainerUser = "testuser"
)

// TestConnectorE2E exercises connectors against real Docker/SSH targets.
func TestConnectorE2E(t *testing.T) {
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	variants := map[string]func(t *testing.T) e2eConnectorVariant{
		"Docker": env.newDockerVariant,
		"SSH":    env.newSSHVariant,
	}

	for name, newVariant := range variants {
		t.Run(name, func(t *testing.T) {
			v := newVariant(t)
			t.Cleanup(v.cleanup)

			ctx := t.Context()

			t.Run("InitializeRemote", func(t *testing.T) {
				alreadyInit, err := v.connector.InitializeRemote(ctx)
				test.That(t, err, test.ShouldBeNil)
				test.That(t, alreadyInit, test.ShouldBeFalse)
			})

			t.Run("RunOneShotCommand", func(t *testing.T) {
				output, err := v.connector.RunOneShotCommand(ctx, "echo hello")
				test.That(t, err, test.ShouldBeNil)
				test.That(t, strings.TrimSpace(output), test.ShouldEqual, "hello")
			})

			t.Run("RunOneShotCommandExitCode", func(t *testing.T) {
				_, err := v.connector.RunOneShotCommand(ctx, "exit 42")
				test.That(t, err, test.ShouldNotBeNil)
			})

			t.Run("CopyFile", func(t *testing.T) {
				remotePath := "/tmp/graft-test-copy"
				err := v.connector.CopyFile(ctx, env.graftBinPath, remotePath, "755")
				test.That(t, err, test.ShouldBeNil)

				output, err := v.connector.RunOneShotCommand(ctx, "stat "+remotePath)
				test.That(t, err, test.ShouldBeNil)
				test.That(t, output, test.ShouldNotBeEmpty)
			})
		})
	}

	// Docker-specific: reinitialize returns true for an already-running container.
	t.Run("Docker/ReinitializeAlreadyRunning", func(t *testing.T) {
		v := env.newDockerVariant(t)
		t.Cleanup(v.cleanup)

		_, err := v.connector.InitializeRemote(t.Context())
		test.That(t, err, test.ShouldBeNil)

		alreadyInit, err := v.connector.InitializeRemote(t.Context())
		test.That(t, err, test.ShouldBeNil)
		test.That(t, alreadyInit, test.ShouldBeTrue)
	})
}

// TestConnectionManagerE2E exercises the full ConnectionManager path including
// daemon installation and gRPC command execution.
func TestConnectionManagerE2E(t *testing.T) {
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	type variant struct {
		schemeName string
		factory    ConnectorFactory
		destURL    *url.URL
	}

	newDockerVariant := func(t *testing.T, containerName string) variant {
		t.Helper()

		return variant{
			schemeName: dockerSchemeName,
			factory:    env.dockerConnectorFactory(t),
			destURL:    env.dockerDestURL(t, containerName),
		}
	}

	newSSHVariant := func(t *testing.T, _ string) variant {
		t.Helper()

		sshPort := env.startSSHContainer(t)

		return variant{
			schemeName: sshSchemeName,
			factory:    env.sshConnectorFactory(t),
			destURL:    env.sshDestURL(t, sshPort),
		}
	}

	variants := map[string]func(t *testing.T, containerName string) variant{
		"Docker": newDockerVariant,
		"SSH":    newSSHVariant,
	}

	for name, newVariant := range variants {
		t.Run(name, func(t *testing.T) {
			connName := sanitizeContainerName("graft-e2e-connmgr-" + t.Name())
			v := newVariant(t, connName)

			mgr := NewConnectionManager(slog.LevelDebug)
			mgr.RegisterConnectorFactory(v.schemeName, v.factory)
			t.Cleanup(mgr.Close)

			localRoot := t.TempDir()

			conn, err := mgr.Initialize(t.Context(), connName, v.destURL, localRoot, "", "", false, false)
			test.That(t, err, test.ShouldBeNil)

			t.Cleanup(func() {
				test.That(t, mgr.Remove(context.Background(), connName), test.ShouldBeNil)
				// Defensive fallback in case mgr.Remove didn't reach the container
				// (applies to Docker; harmless no-op for SSH-named connections).
				forceRemoveDockerContainerByName(t, connName)
			})

			state, _ := conn.State()
			test.That(t, state, test.ShouldEqual, ConnectionStateConnected)

			output := runCommandViaConnection(t, conn, "echo", "hello")
			test.That(t, output, test.ShouldEqual, "hello")

			output = runCommandViaConnection(t, conn, "cat", "/etc/os-release")
			test.That(t, output, test.ShouldContainSubstring, "Ubuntu")
		})
	}
}

// TestMultipleConnectionsSameIdentityE2E verifies that two connections with the
// same identity to the same SSH host share the remote daemon without reinstalling.
func TestMultipleConnectionsSameIdentityE2E(t *testing.T) {
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	sshPort := env.startSSHContainer(t)

	mgr := NewConnectionManager(slog.LevelDebug)
	mgr.RegisterConnectorFactory(sshSchemeName, env.sshConnectorFactory(t))
	t.Cleanup(mgr.Close)

	destURL := env.sshDestURL(t, sshPort)
	identity := "test-shared-aa11"

	connName1 := sanitizeContainerName("graft-e2e-sameid1-" + t.Name())
	connName2 := sanitizeContainerName("graft-e2e-sameid2-" + t.Name())

	// First connection installs and starts the remote daemon.
	conn1, err := mgr.Initialize(t.Context(), connName1, destURL, t.TempDir(), "/tmp/proj1", identity, false, false)
	test.That(t, err, test.ShouldBeNil)

	t.Cleanup(func() {
		test.That(t, mgr.Remove(context.Background(), connName1), test.ShouldBeNil)
	})

	state1, _ := conn1.State()
	test.That(t, state1, test.ShouldEqual, ConnectionStateConnected)

	// Override ourVersion to match the remote daemon so the second connection
	// doesn't trigger a reinstall. In production, both connections originate
	// from the same local daemon, so versions always match.
	remClient := graftv1.NewGraftServiceClient(conn1.daemon.RemoteClientConn())
	resp, err := remClient.Status(t.Context(), &graftv1.StatusRequest{})
	test.That(t, err, test.ShouldBeNil)

	savedVersion := ourVersion
	ourVersion = resp.GetVersionInfo()

	t.Cleanup(func() { ourVersion = savedVersion })

	// Second connection with the same identity reuses the existing remote daemon.
	conn2, err := mgr.Initialize(t.Context(), connName2, destURL, t.TempDir(), "/tmp/proj2", identity, false, false)
	test.That(t, err, test.ShouldBeNil)

	t.Cleanup(func() {
		test.That(t, mgr.Remove(context.Background(), connName2), test.ShouldBeNil)
	})

	state2, _ := conn2.State()
	test.That(t, state2, test.ShouldEqual, ConnectionStateConnected)

	// Both connections can run commands via the shared daemon.
	output1 := runCommandViaConnection(t, conn1, "echo", "from-conn1")
	test.That(t, output1, test.ShouldEqual, "from-conn1")

	output2 := runCommandViaConnection(t, conn2, "echo", "from-conn2")
	test.That(t, output2, test.ShouldEqual, "from-conn2")

	// Verify different remote roots.
	test.That(t, conn1.RemoteRoot(), test.ShouldEqual, "/tmp/proj1")
	test.That(t, conn2.RemoteRoot(), test.ShouldEqual, "/tmp/proj2")

	// Kill the shared remote daemon to trigger reconnect on both connections.
	// The Shutdown RPC is fire-and-forget: the remote daemon sends SIGINT to
	// itself, which may tear down the gRPC server before the response is
	// flushed, causing an EOF or Unavailable error on the client side.
	remClient = graftv1.NewGraftServiceClient(conn1.daemon.RemoteClientConn())
	remClient.Shutdown(t.Context(), &graftv1.ShutdownRequest{}) //nolint:errcheck

	// Trigger reconnect. Both connections share a daemon, so only one
	// reconnect attempt is needed. The daemon's reconnect guard prevents
	// concurrent attempts, so the second call returns false immediately.
	reconnectCtx := mgr.runCtx

	result := conn1.daemon.Reconnect(reconnectCtx)
	test.That(t, result, test.ShouldBeTrue)

	// Both connections should be connected again and able to run commands.
	state1, _ = conn1.State()
	test.That(t, state1, test.ShouldEqual, ConnectionStateConnected)

	state2, _ = conn2.State()
	test.That(t, state2, test.ShouldEqual, ConnectionStateConnected)

	output1 = runCommandViaConnection(t, conn1, "echo", "after-reconnect-1")
	test.That(t, output1, test.ShouldEqual, "after-reconnect-1")

	output2 = runCommandViaConnection(t, conn2, "echo", "after-reconnect-2")
	test.That(t, output2, test.ShouldEqual, "after-reconnect-2")
}

// TestMultipleConnectionsDifferentIdentitiesE2E verifies that two connections
// with different identities to the same SSH host each get their own remote
// daemon instance (simulating two local machines connecting to the same remote).
func TestMultipleConnectionsDifferentIdentitiesE2E(t *testing.T) {
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	sshPort := env.startSSHContainer(t)

	mgr := NewConnectionManager(slog.LevelDebug)
	mgr.RegisterConnectorFactory(sshSchemeName, env.sshConnectorFactory(t))
	t.Cleanup(mgr.Close)

	destURL := env.sshDestURL(t, sshPort)

	connName1 := sanitizeContainerName("graft-e2e-diffid1-" + t.Name())
	connName2 := sanitizeContainerName("graft-e2e-diffid2-" + t.Name())

	// First connection with identity "alpha".
	conn1, err := mgr.Initialize(t.Context(), connName1, destURL, t.TempDir(), "/tmp/proj1", "test-alpha-aa11", false, false)
	test.That(t, err, test.ShouldBeNil)

	t.Cleanup(func() {
		test.That(t, mgr.Remove(context.Background(), connName1), test.ShouldBeNil)
	})

	state1, _ := conn1.State()
	test.That(t, state1, test.ShouldEqual, ConnectionStateConnected)

	// Second connection with identity "bravo" which has a separate remote daemon instance.
	conn2, err := mgr.Initialize(t.Context(), connName2, destURL, t.TempDir(), "/tmp/proj2", "test-bravo-bb22", false, false)
	test.That(t, err, test.ShouldBeNil)

	t.Cleanup(func() {
		test.That(t, mgr.Remove(context.Background(), connName2), test.ShouldBeNil)
	})

	state2, _ := conn2.State()
	test.That(t, state2, test.ShouldEqual, ConnectionStateConnected)

	// Both connections can run commands independently via their own daemons.
	output1 := runCommandViaConnection(t, conn1, "echo", "from-alpha")
	test.That(t, output1, test.ShouldEqual, "from-alpha")

	output2 := runCommandViaConnection(t, conn2, "echo", "from-bravo")
	test.That(t, output2, test.ShouldEqual, "from-bravo")

	// Verify different remote roots.
	test.That(t, conn1.RemoteRoot(), test.ShouldEqual, "/tmp/proj1")
	test.That(t, conn2.RemoteRoot(), test.ShouldEqual, "/tmp/proj2")
}

// e2eDockerEnv holds shared state for the E2E Docker tests.
type e2eDockerEnv struct {
	imageID       string
	graftBinPath  string
	sshPrivateKey ssh.Signer
	sshKeyDir     string
}

var (
	sharedE2EEnv     *e2eDockerEnv
	sharedE2EEnvOnce sync.Once
	errSharedE2EEnv  error
)

// getOrSetupE2EEnv builds the test Docker image and graft binary once, sharing across tests.
func getOrSetupE2EEnv(t *testing.T) *e2eDockerEnv {
	t.Helper()

	sharedE2EEnvOnce.Do(func() {
		sharedE2EEnv, errSharedE2EEnv = buildE2EEnv(t.Context())
	})

	test.That(t, errSharedE2EEnv, test.ShouldBeNil)

	sshKeyDir := t.TempDir()
	signer := generateSSHKey(t, sshKeyDir)

	return &e2eDockerEnv{
		imageID:       sharedE2EEnv.imageID,
		graftBinPath:  sharedE2EEnv.graftBinPath,
		sshPrivateKey: signer,
		sshKeyDir:     sshKeyDir,
	}
}

func buildE2EEnv(ctx context.Context) (*e2eDockerEnv, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, errors.New("could not determine source file path")
	}

	repoRoot := filepath.Join(filepath.Dir(thisFile), "..")

	graftBinPath, err := buildE2EBinary(ctx, repoRoot, "linux", runtime.GOARCH)
	if err != nil {
		return nil, err
	}

	dockerfilePath := filepath.Join(repoRoot, "testdata", "docker-e2e")

	buildCmd := exec.CommandContext(ctx, "docker", "build", "-t", e2eImageName, dockerfilePath)
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr

	if err := buildCmd.Run(); err != nil {
		return nil, fmt.Errorf("building docker image: %w", err)
	}

	return &e2eDockerEnv{
		imageID:      e2eImageName,
		graftBinPath: graftBinPath,
	}, nil
}

func buildE2EBinary(ctx context.Context, repoRoot, osName, archName string) (string, error) {
	binName := daemonBinName(osName, archName)
	binPath := filepath.Join(repoRoot, "bin", binName)

	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./cmd/graft")
	cmd.Dir = repoRoot
	cmd.Env = slices.DeleteFunc(os.Environ(), func(v string) bool {
		return strings.HasPrefix(v, "CGO_ENABLED=") || strings.HasPrefix(v, "GOOS=") || strings.HasPrefix(v, "GOARCH=")
	})
	cmd.Env = append(cmd.Env,
		"CGO_ENABLED=0",
		"GOOS="+osName,
		"GOARCH="+archName,
	)
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("building graft binary: %w", err)
	}

	return binPath, nil
}

func generateSSHKey(t *testing.T, sshKeyDir string) ssh.Signer {
	t.Helper()

	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	test.That(t, err, test.ShouldBeNil)

	signer, err := ssh.NewSignerFromKey(privKey)
	test.That(t, err, test.ShouldBeNil)

	sshPubKey, err := ssh.NewPublicKey(pubKey)
	test.That(t, err, test.ShouldBeNil)

	authorizedKey := ssh.MarshalAuthorizedKey(sshPubKey)
	err = os.WriteFile(filepath.Join(sshKeyDir, "authorized_keys"), authorizedKey, 0o600)
	test.That(t, err, test.ShouldBeNil)

	return signer
}

func sanitizeContainerName(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, " ", "-")
	name = fmt.Sprintf("%s-%d", name, time.Now().UnixNano())

	return name
}

func (env *e2eDockerEnv) startSSHContainer(t *testing.T) string {
	t.Helper()

	return env.startSSHContainerInfo(t).port
}

func waitForSSH(t *testing.T, containerID string) {
	t.Helper()

	waitCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	ready := make(chan struct{})

	go func() {
		for {
			checkCmd := exec.CommandContext(waitCtx, "docker", "exec", containerID, "bash", "-c",
				"ss -tlnp | grep -q ':22'")
			if checkCmd.Run() == nil {
				close(ready)

				return
			}

			select {
			case <-waitCtx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()

	select {
	case <-ready:
	case <-waitCtx.Done():
		test.That(t, "SSH did not become ready in time", test.ShouldBeEmpty)
	}
}

func requireDocker(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required for E2E tests but was not found in PATH")
	}
}

func (env *e2eDockerEnv) dockerConnectorFactory(t *testing.T) ConnectorFactory {
	t.Helper()

	return newDockerConnectorFactory()
}

func (env *e2eDockerEnv) sshConnectorFactory(t *testing.T) ConnectorFactory {
	t.Helper()

	return &sshConnectorFactory{
		sshConfigLookup: &fakeSSHConfigResolver{
			values:    map[string]map[string]string{},
			allValues: map[string]map[string][]string{},
		},
		staticSigners: []ssh.Signer{env.sshPrivateKey},
	}
}

func (env *e2eDockerEnv) dockerDestURL(t *testing.T, containerNames ...string) *url.URL {
	t.Helper()

	raw := "docker://?imageTag=" + env.imageID
	if len(containerNames) > 0 && containerNames[0] != "" {
		raw += "&containerName=" + containerNames[0]
	}

	destURL, err := url.Parse(raw)
	test.That(t, err, test.ShouldBeNil)

	return destURL
}

func (env *e2eDockerEnv) sshDestURL(t *testing.T, port string) *url.URL {
	t.Helper()

	destURL, err := url.Parse(fmt.Sprintf("ssh://%s@127.0.0.1:%s", e2eContainerUser, port))
	test.That(t, err, test.ShouldBeNil)

	return destURL
}

// e2eConnectorVariant bundles everything needed to run connector subtests against
// a particular transport (Docker or SSH).
type e2eConnectorVariant struct {
	factory   ConnectorFactory
	destURL   *url.URL
	connector RemoteConnector
	cleanup   func()
}

func (env *e2eDockerEnv) newDockerVariant(t *testing.T) e2eConnectorVariant {
	t.Helper()

	// The default containerName equals the image tag and is static across runs.
	// Force-remove any stale container so a prior run's leaked state can't
	// poison this one.
	forceRemoveDockerContainerByName(t, env.imageID)

	factory := env.dockerConnectorFactory(t)
	destURL := env.dockerDestURL(t)

	connector, err := factory.CreateConnector(t.Context(), destURL, "")
	test.That(t, err, test.ShouldBeNil)

	return e2eConnectorVariant{
		factory:   factory,
		destURL:   destURL,
		connector: connector,
		cleanup: func() {
			test.That(t, connector.DeinitializeRemote(context.Background()), test.ShouldBeNil)
			// Belt and suspenders: forcibly remove by name in case Deinitialize
			// no-oped because containerID wasn't tracked for some reason.
			forceRemoveDockerContainerByName(t, env.imageID)
		},
	}
}

// forceRemoveDockerContainerByName does a best-effort removal of a docker
// container by name, swallowing "not found" errors. Helps keep test runs
// hermetic against state leaked from prior runs.
func forceRemoveDockerContainerByName(t *testing.T, name string) {
	t.Helper()

	if name == "" {
		return
	}

	rmCmd := exec.Command("docker", "rm", "-f", name) //nolint:noctx

	out, err := rmCmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		t.Logf("forceRemoveDockerContainerByName(%q): %v: %s", name, err, string(out))
	}
}

func (env *e2eDockerEnv) newSSHVariant(t *testing.T) e2eConnectorVariant {
	t.Helper()

	sshPort := env.startSSHContainer(t)
	factory := env.sshConnectorFactory(t)
	destURL := env.sshDestURL(t, sshPort)

	connector, err := factory.CreateConnector(t.Context(), destURL, "")
	test.That(t, err, test.ShouldBeNil)

	return e2eConnectorVariant{
		factory:   factory,
		destURL:   destURL,
		connector: connector,
		cleanup: func() {
			test.That(t, connector.Close(), test.ShouldBeNil)
		},
	}
}

// sshContainerInfo holds the SSH container details needed for reconnect tests.
type sshContainerInfo struct {
	port        string
	containerID string
}

// startSSHContainerInfo is like startSSHContainer but also returns the container ID.
func (env *e2eDockerEnv) startSSHContainerInfo(t *testing.T) sshContainerInfo {
	t.Helper()

	ctx := t.Context()
	safeName := sanitizeContainerName("graft-e2e-ssh-" + t.Name())

	runCmd := exec.CommandContext(ctx, "docker", "run", "-d",
		"-p", "0:22",
		"--name", safeName,
		env.imageID,
	)

	out, err := runCmd.Output()
	test.That(t, err, test.ShouldBeNil)

	containerID := strings.TrimSpace(string(out))

	t.Cleanup(func() {
		//nolint:errcheck
		exec.CommandContext(context.Background(), "docker", "rm", "-f", containerID).Run()
	})

	portCmd := exec.CommandContext(ctx, "docker", "port", containerID, "22")
	portOut, err := portCmd.Output()
	test.That(t, err, test.ShouldBeNil)

	hostPort := strings.TrimSpace(string(portOut))
	parts := strings.Split(hostPort, ":")
	test.That(t, len(parts) >= 2, test.ShouldBeTrue)

	sshPort := parts[len(parts)-1]

	authKeysPath := filepath.Join(env.sshKeyDir, "authorized_keys")
	cpCmd := exec.CommandContext(ctx, "docker", "cp", authKeysPath,
		containerID+":/home/"+e2eContainerUser+"/.ssh/authorized_keys")
	err = cpCmd.Run()
	test.That(t, err, test.ShouldBeNil)

	fixCmd := exec.CommandContext(ctx, "docker", "exec", containerID,
		"chown", e2eContainerUser+":"+e2eContainerUser,
		"/home/"+e2eContainerUser+"/.ssh/authorized_keys")
	err = fixCmd.Run()
	test.That(t, err, test.ShouldBeNil)

	waitForSSH(t, containerID)

	return sshContainerInfo{port: sshPort, containerID: containerID}
}

// startSSHContainerOnPort starts a new SSH container bound to a specific host port.
// Returns the new container ID. Registers cleanup to remove the container.
func (env *e2eDockerEnv) startSSHContainerOnPort(t *testing.T, hostPort string) string {
	t.Helper()

	ctx := t.Context()
	safeName := sanitizeContainerName("graft-e2e-ssh-reconn-" + t.Name())

	runCmd := exec.CommandContext(ctx, "docker", "run", "-d", //nolint:gosec // test helper
		"-p", hostPort+":22",
		"--name", safeName,
		env.imageID,
	)

	out, err := runCmd.Output()
	test.That(t, err, test.ShouldBeNil)

	containerID := strings.TrimSpace(string(out))

	t.Cleanup(func() {
		//nolint:errcheck
		exec.CommandContext(context.Background(), "docker", "rm", "-f", containerID).Run()
	})

	authKeysPath := filepath.Join(env.sshKeyDir, "authorized_keys")
	cpCmd := exec.CommandContext(ctx, "docker", "cp", authKeysPath,
		containerID+":/home/"+e2eContainerUser+"/.ssh/authorized_keys")
	test.That(t, cpCmd.Run(), test.ShouldBeNil)

	fixCmd := exec.CommandContext(ctx, "docker", "exec", containerID,
		"chown", e2eContainerUser+":"+e2eContainerUser,
		"/home/"+e2eContainerUser+"/.ssh/authorized_keys")
	test.That(t, fixCmd.Run(), test.ShouldBeNil)

	waitForSSH(t, containerID)

	return containerID
}

// TestConnectionReconnectE2E exercises the full reconnect path: transport dies,
// health check detects the failure, reconnect re-establishes transport + daemon.
func TestConnectionReconnectE2E(t *testing.T) {
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	type reconnectVariant struct {
		schemeName string
		factory    ConnectorFactory
		destURL    *url.URL
		// breakTransport kills the container's transport so the health check
		// detects a failure. restoreTransport brings it back so reconnect can
		// succeed. Separated so the caller can start the health-check loop
		// between the two.
		breakTransport   func(t *testing.T)
		restoreTransport func(t *testing.T)
	}

	newDockerVariant := func(t *testing.T, containerName string) reconnectVariant {
		t.Helper()

		return reconnectVariant{
			schemeName: dockerSchemeName,
			factory:    env.dockerConnectorFactory(t),
			destURL:    env.dockerDestURL(t, containerName),
			// containerID discovered after Initialize via docker ps.
		}
	}

	newSSHVariant := func(t *testing.T, _ string) reconnectVariant {
		t.Helper()

		info := env.startSSHContainerInfo(t)

		return reconnectVariant{
			schemeName: sshSchemeName,
			factory:    env.sshConnectorFactory(t),
			destURL:    env.sshDestURL(t, info.port),
			breakTransport: func(t *testing.T) {
				t.Helper()

				rmCmd := exec.CommandContext(t.Context(), "docker", "rm", "-f", info.containerID) //nolint:gosec
				test.That(t, rmCmd.Run(), test.ShouldBeNil)
			},
			restoreTransport: func(t *testing.T) {
				t.Helper()

				// Recreate the container on the same host port so the SSH connector
				// can reconnect to the same address.
				newContainerID := env.startSSHContainerOnPort(t, info.port)
				info.containerID = newContainerID
			},
		}
	}

	variants := map[string]func(t *testing.T, containerName string) reconnectVariant{
		"Docker": newDockerVariant,
		"SSH":    newSSHVariant,
	}

	for name, newVariant := range variants {
		t.Run(name, func(t *testing.T) {
			connName := sanitizeContainerName("graft-e2e-reconnect-" + t.Name())
			v := newVariant(t, connName)

			mgr := NewConnectionManager(slog.LevelDebug)
			mgr.RegisterConnectorFactory(v.schemeName, v.factory)
			t.Cleanup(mgr.Close)

			localRoot := t.TempDir()

			conn, err := mgr.Initialize(t.Context(), connName, v.destURL, localRoot, "", "", false, false)
			test.That(t, err, test.ShouldBeNil)

			t.Cleanup(func() {
				test.That(t, mgr.Remove(context.Background(), connName), test.ShouldBeNil)
				// Defensive fallback in case mgr.Remove didn't reach the container.
				forceRemoveDockerContainerByName(t, connName)
			})

			state, _ := conn.State()
			test.That(t, state, test.ShouldEqual, ConnectionStateConnected)

			// Verify the connection works.
			output := runCommandViaConnection(t, conn, "echo", "pre-reconnect")
			test.That(t, output, test.ShouldEqual, "pre-reconnect")

			if v.breakTransport != nil {
				// Variant provides custom break/restore (e.g. SSH removes and recreates the container).
				v.breakTransport(t)
			} else {
				// Docker variant: discover container ID and stop it.
				filter := "name=" + connName

				psCmd := exec.CommandContext(t.Context(), "docker", "ps", "-q", "--filter", filter)
				psOut, psErr := psCmd.Output()
				test.That(t, psErr, test.ShouldBeNil)

				containerID := strings.TrimSpace(string(psOut))
				test.That(t, containerID, test.ShouldNotBeEmpty)

				stopCmd := exec.CommandContext(t.Context(), "docker", "stop", "-t", "1", containerID)
				test.That(t, stopCmd.Run(), test.ShouldBeNil)

				v.restoreTransport = func(t *testing.T) {
					t.Helper()

					startCmd := exec.CommandContext(t.Context(), "docker", "start", containerID)
					test.That(t, startCmd.Run(), test.ShouldBeNil)
				}
			}

			// Start the health check loop so it detects the failure.
			runCtx, runCancel := context.WithCancel(t.Context())
			t.Cleanup(runCancel)

			go mgr.Run(runCtx)

			// Restore the transport so the reconnect loop can succeed.
			v.restoreTransport(t)

			// Wait for reconnection: poll state with a timeout.
			reconnected := make(chan struct{})

			go func() {
				for {
					st, _ := conn.State()
					if st == ConnectionStateConnected {
						// Only consider it reconnected if we first saw a non-connected state.
						// Check by trying to actually run a command.
						_, cmdErr := conn.RunCommand(
							context.Background(),
							"", false, "echo", []string{"health"}, nil, false, false, true, true,
						)
						if cmdErr == nil {
							close(reconnected)

							return
						}
					}

					select {
					case <-runCtx.Done():
						return
					default:
					}
				}
			}()

			// Use a timeout context for waiting.
			waitCtx, waitCancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer waitCancel()

			select {
			case <-reconnected:
				// Success
			case <-waitCtx.Done():
				state, reason := conn.State()
				t.Fatalf("timed out waiting for reconnect; state=%s reason=%s", state, reason)
			}

			// Verify the connection works after reconnect.
			output = runCommandViaConnection(t, conn, "echo", "post-reconnect")
			test.That(t, output, test.ShouldEqual, "post-reconnect")
		})
	}
}

// runCommandViaConnection runs a command via the full gRPC connection path and returns its stdout.
func runCommandViaConnection(t *testing.T, conn *Connection, command string, args ...string) string {
	t.Helper()

	return runCommandViaConnectionInDir(t, conn, "", command, args...)
}

// runCommandViaConnectionInDir is like runCommandViaConnection but sets the working directory.
func runCommandViaConnectionInDir(t *testing.T, conn *Connection, cwd, command string, args ...string) string {
	t.Helper()

	runningCmd, err := conn.RunCommand(
		t.Context(),
		cwd,
		false, // shell
		command,
		args,
		nil,   // extraEnv
		false, // sudo
		false, // allocatePty
		true,  // redirectStdout
		true,  // redirectStderr
	)
	test.That(t, err, test.ShouldBeNil)

	test.That(t, runningCmd.Stdin().Close(), test.ShouldBeNil)

	var (
		stdout    []byte
		stdoutErr error
	)

	stdoutDone := make(chan struct{})

	go func() {
		stdout, stdoutErr = io.ReadAll(runningCmd.Stdout())

		close(stdoutDone)
	}()

	exitCode, err := runningCmd.Wait()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, exitCode, test.ShouldEqual, 0)

	<-stdoutDone
	test.That(t, stdoutErr, test.ShouldBeNil)

	return strings.TrimSpace(string(stdout))
}

// dockerExec runs a command inside a container.
func dockerExec(t *testing.T, containerID string, cmd string) {
	t.Helper()

	execCmd := exec.CommandContext(t.Context(), "docker", "exec", containerID, "bash", "-c", cmd)
	err := execCmd.Run()
	test.That(t, err, test.ShouldBeNil)
}

// TestEnvProviderDiscoveryE2E verifies that the env provider system discovers
// binaries from mise-managed PATH entries on a real remote via SSH, and that
// newly added directories are picked up over time.
func TestEnvProviderDiscoveryE2E(t *testing.T) { //nolint:gocognit
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	sc := env.startSSHContainerInfo(t)

	// Install a fake mise binary that outputs env for /opt/tools/bin.
	dockerExec(t, sc.containerID, `cat > /usr/local/bin/mise << 'SCRIPT'
#!/bin/bash
if [[ "$1" == "env" ]]; then
    echo 'export PATH=/opt/tools/bin:'"$PATH"
elif [[ "$1" == "hook-env" ]]; then
    # Check if we're in a directory with a .mise.toml
    dir="$PWD"
    while [ "$dir" != "/" ]; do
        if [ -f "$dir/.mise.toml" ]; then
            echo 'export PATH=/opt/tools/bin:'"$PATH"
            exit 0
        fi
        dir=$(dirname "$dir")
    done
fi
SCRIPT
chmod +x /usr/local/bin/mise`)

	// Create a test binary in /opt/tools/bin.
	dockerExec(t, sc.containerID, `mkdir -p /opt/tools/bin && cat > /opt/tools/bin/graft-test-tool << 'SCRIPT'
#!/bin/bash
echo "hello-from-env-provider"
SCRIPT
chmod +x /opt/tools/bin/graft-test-tool`)

	// Create a .mise.toml in a project directory so the provider detects config.
	dockerExec(t, sc.containerID, `mkdir -p /home/testuser/project && cat > /home/testuser/project/.mise.toml << 'TOML'
[tools]
go = "1.22"
TOML
chown -R testuser:testuser /home/testuser/project`)

	// Connect via SSH. The remote daemon starts, detects fake mise,
	// and begins periodic Refresh for /home/testuser/project.
	mgr := NewConnectionManager(slog.LevelDebug)
	mgr.RegisterConnectorFactory(sshSchemeName, env.sshConnectorFactory(t))
	t.Cleanup(mgr.Close)

	connName := sanitizeContainerName("graft-e2e-envprov-" + t.Name())
	destURL := env.sshDestURL(t, sc.port)

	conn, err := mgr.Initialize(t.Context(), connName, destURL, t.TempDir(), "/home/testuser/project", "", false, false)
	test.That(t, err, test.ShouldBeNil)

	t.Cleanup(func() {
		test.That(t, mgr.Remove(context.Background(), connName), test.ShouldBeNil)
	})

	state, _ := conn.State()
	test.That(t, state, test.ShouldEqual, ConnectionStateConnected)

	// Wait for graft-test-tool to appear in discovered commands.
	// The DiscoverCommands loop runs every 1s and includes ExtraPATHDirs.
	waitCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	found := make(chan struct{})

	go func() {
		for {
			_, byDir := conn.daemon.AvailableCommands()
			for dir, cmds := range byDir {
				for _, cmd := range cmds {
					if dir == "/home/testuser/project" && filepath.Base(cmd) == "graft-test-tool" {
						close(found)

						return
					}
				}
			}

			select {
			case <-waitCtx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()

	select {
	case <-found:
	case <-waitCtx.Done():
		test.That(t, "graft-test-tool was not discovered via env provider within timeout", test.ShouldBeEmpty)
	}

	// Run the tool via the connection - it should be found and execute.
	output := runCommandViaConnectionInDir(t, conn, "/home/testuser/project", "graft-test-tool")
	test.That(t, output, test.ShouldEqual, "hello-from-env-provider")

	// Phase 2: add a second tool directory. Update fake mise to include it.
	dockerExec(t, sc.containerID, `mkdir -p /opt/extra/bin && cat > /opt/extra/bin/graft-extra-tool << 'SCRIPT'
#!/bin/bash
echo "hello-from-extra"
SCRIPT
chmod +x /opt/extra/bin/graft-extra-tool`)

	dockerExec(t, sc.containerID, `cat > /usr/local/bin/mise << 'SCRIPT'
#!/bin/bash
if [[ "$1" == "env" ]]; then
    echo 'export PATH=/opt/tools/bin:/opt/extra/bin:'"$PATH"
elif [[ "$1" == "hook-env" ]]; then
    dir="$PWD"
    while [ "$dir" != "/" ]; do
        if [ -f "$dir/.mise.toml" ]; then
            echo 'export PATH=/opt/tools/bin:/opt/extra/bin:'"$PATH"
            exit 0
        fi
        dir=$(dirname "$dir")
    done
fi
SCRIPT
chmod +x /usr/local/bin/mise`)

	// Touch the .mise.toml to trigger mtime-based cache invalidation.
	dockerExec(t, sc.containerID, `touch /home/testuser/project/.mise.toml`)

	// Wait for the extra tool to be discovered.
	waitCtx2, cancel2 := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel2()

	found2 := make(chan struct{})

	go func() {
		for {
			// TODO(erd): test me
			_, byDir := conn.daemon.AvailableCommands()
			for dir, cmds := range byDir {
				for _, cmd := range cmds {
					if dir == "/home/testuser/project" && filepath.Base(cmd) == "graft-extra-tool" {
						close(found2)

						return
					}
				}
			}

			select {
			case <-waitCtx2.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()

	select {
	case <-found2:
	case <-waitCtx2.Done():
		test.That(t, "graft-extra-tool was not discovered after mise update within timeout", test.ShouldBeEmpty)
	}

	output = runCommandViaConnectionInDir(t, conn, "/home/testuser/project", "graft-extra-tool")
	test.That(t, output, test.ShouldEqual, "hello-from-extra")
}
