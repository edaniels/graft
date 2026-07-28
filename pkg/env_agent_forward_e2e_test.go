package graft

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"go.viam.com/test"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// agentForwardReadyMarker is echoed by the remote once SSH_AUTH_SOCK is set
// and pointing at a live socket; used to poll for agent forwarding readiness.
const agentForwardReadyMarker = "ready"

// TestEnvForwardE2E proves the full envForward path against a real remote
// daemon: a config-declared forward name/glob is picked up by the reconcile
// loop with no manual step, resolved from the live local environment, and
// actually arrives in a command's environment on the other end of a real SSH
// connection.
func TestEnvForwardE2E(t *testing.T) {
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)
	t.Setenv("GRAFT_E2E_ENV_VAR", "e2e-value-123")

	localDir := mkShortTempDir(t, "src-")

	sc := env.startSSHContainerInfo(t)
	remoteDir := "/home/" + e2eContainerUser + "/envforward"

	connName := sanitizeContainerName("graft-e2e-envforward-" + t.Name())
	destination := fmt.Sprintf("ssh://%s@127.0.0.1:%s", e2eContainerUser, sc.port)

	config := &RootConfig{
		Connections: []ConnectionConfig{{
			Name:        connName,
			Destination: destination,
			LocalRoot:   localDir,
			RemoteRoot:  remoteDir,
			// A glob, not the exact name, so the test also proves pattern
			// matching against the live environment (see resolveEnvForwardNames).
			EnvForward: []string{"GRAFT_E2E_ENV_*"},
		}},
	}

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		conn := waitForConnectedConn(t, srv, connName)

		// No manual step: the 1s reconcile tick is what's expected to apply
		// EnvForward from config onto the live connection.
		test.That(t, waitFor(t, func() bool {
			return len(conn.EnvForwardNames()) > 0
		}), test.ShouldBeNil)

		extraEnv := resolveEnvForwardNames(conn.EnvForwardNames(), os.Environ())
		test.That(t, extraEnv, test.ShouldContain, "GRAFT_E2E_ENV_VAR=e2e-value-123")

		out, ok := runOneShotViaConn(t, conn, extraEnv, "sh", []string{"-c", "echo $GRAFT_E2E_ENV_VAR"})
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, strings.TrimSpace(out), test.ShouldEqual, "e2e-value-123")
	})
}

// TestSSHAgentForwardE2E proves the full agent-forward path against a real
// remote daemon: enabling ForwardAgent in config is enough (no manual RPC
// invocation) for a real remote command to see a live SSH_AUTH_SOCK and
// successfully relay the SSH agent protocol back to a real local agent.
func TestSSHAgentForwardE2E(t *testing.T) {
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	sockPath, fingerprint := startFakeSSHAgent(t)
	t.Setenv("SSH_AUTH_SOCK", sockPath)

	localDir := mkShortTempDir(t, "src-")

	sc := env.startSSHContainerInfo(t)
	remoteDir := "/home/" + e2eContainerUser + "/agentforward"

	connName := sanitizeContainerName("graft-e2e-agentforward-" + t.Name())
	destination := fmt.Sprintf("ssh://%s@127.0.0.1:%s", e2eContainerUser, sc.port)

	config := &RootConfig{
		Connections: []ConnectionConfig{{
			Name:         connName,
			Destination:  destination,
			LocalRoot:    localDir,
			RemoteRoot:   remoteDir,
			ForwardAgent: true,
		}},
	}

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		conn := waitForConnectedConn(t, srv, connName)

		// No manual step: the 1s reconcile tick is what's expected to start
		// the forward from config. Poll via a real remote command (not
		// internal state) so this proves the socket is actually live.
		waitForAgentForwardReady(t, conn)

		// Prove the relay actually works end-to-end: ssh-add on the remote,
		// talking through the forwarded socket, must see our local key.
		out, ok := runOneShotViaConn(t, conn, nil, "ssh-add", []string{"-l"})
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, out, test.ShouldContainSubstring, fingerprint)
	})
}

// TestSSHAgentForwardIsolationE2E verifies agent forwarding is scoped per
// connection name, not per remote daemon: two local Connections to the same
// host+identity (sharing one remoteDaemon/remote process) with different
// ForwardAgent settings must not fight over shared state. The forwarding
// connection gets a live, working socket; the non-forwarding one never sees
// SSH_AUTH_SOCK, even though every command runs on the very same remote
// daemon process.
func TestSSHAgentForwardIsolationE2E(t *testing.T) {
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	sockPath, fingerprint := startFakeSSHAgent(t)
	t.Setenv("SSH_AUTH_SOCK", sockPath)

	localDirOn := mkShortTempDir(t, "src-on-")
	localDirOff := mkShortTempDir(t, "src-off-")

	sc := env.startSSHContainerInfo(t)
	destination := fmt.Sprintf("ssh://%s@127.0.0.1:%s", e2eContainerUser, sc.port)

	connNameOn := sanitizeContainerName("graft-e2e-agentiso-on-" + t.Name())
	connNameOff := sanitizeContainerName("graft-e2e-agentiso-off-" + t.Name())

	config := &RootConfig{
		Connections: []ConnectionConfig{
			{
				Name:         connNameOn,
				Destination:  destination,
				LocalRoot:    localDirOn,
				RemoteRoot:   "/home/" + e2eContainerUser + "/agentiso-on",
				ForwardAgent: true,
			},
			{
				Name:        connNameOff,
				Destination: destination,
				LocalRoot:   localDirOff,
				RemoteRoot:  "/home/" + e2eContainerUser + "/agentiso-off",
				// ForwardAgent intentionally left false.
			},
		},
	}

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		connOn := waitForConnectedConn(t, srv, connNameOn)
		connOff := waitForConnectedConn(t, srv, connNameOff)

		// Sanity check the premise: both connections really do share one
		// remote daemon (same destination+identity), so this is a genuine
		// test of per-connection isolation, not just two unrelated daemons.
		test.That(t, connOn.lockedDaemon(), test.ShouldEqual, connOff.lockedDaemon())

		waitForAgentForwardReady(t, connOn)

		out, ok := runOneShotViaConn(t, connOn, nil, "ssh-add", []string{"-l"})
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, out, test.ShouldContainSubstring, fingerprint)

		// The non-forwarding connection, on the SAME remote daemon, must
		// never see SSH_AUTH_SOCK set. Checked repeatedly, spanning multiple
		// reconcile ticks, since forwarding state is per-connection and must
		// stay that way on every tick, not just transiently.
		for range 5 {
			out, ok := runOneShotViaConn(t, connOff, nil, "sh", []string{"-c", `echo "[$SSH_AUTH_SOCK]"`})
			test.That(t, ok, test.ShouldBeTrue)
			test.That(t, strings.TrimSpace(out), test.ShouldEqual, "[]")
		}
	})
}

// TestForwardAgentAtConnectE2E proves ConnectParams.ForwardAgent (the
// `graft connect --forward-agent` flag): a single InitializeRemoteConnection
// call, over a real gRPC connection to a real local daemon, must be enough to
// have agent forwarding live on a brand new connection - no separate
// SetForwardAgent/`graft forward --agent` call required.
func TestForwardAgentAtConnectE2E(t *testing.T) {
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	sockPath, fingerprint := startFakeSSHAgent(t)
	t.Setenv("SSH_AUTH_SOCK", sockPath)

	localDir := mkShortTempDir(t, "src-")

	sc := env.startSSHContainerInfo(t)
	remoteDir := "/home/" + e2eContainerUser + "/agentforward-connect"

	connName := sanitizeContainerName("graft-e2e-agentforward-connect-" + t.Name())

	srv, err := NewServer(&RootConfig{}, ServerRoleLocal, "", true, &BufferedLineWriter{MaxLines: 100}, "", slog.LevelDebug)
	test.That(t, err, test.ShouldBeNil)

	srv.connMgr.RegisterConnectorFactory(sshSchemeName, env.sshConnectorFactory(t))

	runCtx, runCancel := context.WithCancel(context.Background())
	test.That(t, srv.Run(runCtx), test.ShouldBeNil)

	defer func() {
		runCancel()
		srv.Close()
	}()

	client := newLocalClientForRunningServer(t)

	err = client.InitializeRemoteConnection(t.Context(), ConnectParams{
		Name:         connName,
		LocalRoot:    localDir,
		RemoteRoot:   remoteDir,
		Destination:  env.sshDestURL(t, sc.port).String(),
		ForwardAgent: true,
	})
	test.That(t, err, test.ShouldBeNil)

	conn := waitForConnectedConn(t, srv, connName)

	waitForAgentForwardReady(t, conn)

	out, ok := runOneShotViaConn(t, conn, nil, "ssh-add", []string{"-l"})
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, out, test.ShouldContainSubstring, fingerprint)
}

// TestSSHAgentForwardStopRestartE2E verifies a stop-then-restart cycle works
// end-to-end against a real remote daemon: stopping a forward must fully
// release its remote-side socket and map entry so a later restart for the
// same connection succeeds rather than being rejected as already active.
func TestSSHAgentForwardStopRestartE2E(t *testing.T) {
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	sockPath, fingerprint := startFakeSSHAgent(t)
	t.Setenv("SSH_AUTH_SOCK", sockPath)

	localDir := mkShortTempDir(t, "src-")

	sc := env.startSSHContainerInfo(t)
	remoteDir := "/home/" + e2eContainerUser + "/agentforward-restart"

	connName := sanitizeContainerName("graft-e2e-agentrestart-" + t.Name())
	destination := fmt.Sprintf("ssh://%s@127.0.0.1:%s", e2eContainerUser, sc.port)

	config := &RootConfig{
		Connections: []ConnectionConfig{{
			Name:         connName,
			Destination:  destination,
			LocalRoot:    localDir,
			RemoteRoot:   remoteDir,
			ForwardAgent: true,
		}},
	}

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		conn := waitForConnectedConn(t, srv, connName)

		waitForAgentForwardReady(t, conn)

		out, ok := runOneShotViaConn(t, conn, nil, "ssh-add", []string{"-l"})
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, out, test.ShouldContainSubstring, fingerprint)

		// Stop forwarding via the same RPC `graft forward remove --agent` uses.
		test.That(t, srv.SetForwardAgent(false, connName), test.ShouldBeNil)

		waitErr := waitFor(t, func() bool {
			out, ok := runOneShotViaConn(t, conn, nil, "sh", []string{"-c", `echo "[$SSH_AUTH_SOCK]"`})

			return ok && strings.TrimSpace(out) == "[]"
		})
		test.That(t, waitErr, test.ShouldBeNil)

		// Restarting must succeed: the remote-side socket and map entry from
		// the first forward must have been fully released by the stop above.
		test.That(t, srv.SetForwardAgent(true, connName), test.ShouldBeNil)

		waitForAgentForwardReady(t, conn)

		out2, ok2 := runOneShotViaConn(t, conn, nil, "ssh-add", []string{"-l"})
		test.That(t, ok2, test.ShouldBeTrue)
		test.That(t, out2, test.ShouldContainSubstring, fingerprint)
	})
}

// waitForAgentForwardReady polls until a real remote command through conn
// observes a live (existing, socket-backed) SSH_AUTH_SOCK.
func waitForAgentForwardReady(t *testing.T, conn *Connection) {
	t.Helper()

	var lastCheck string

	waitErr := waitFor(t, func() bool {
		out, ok := runOneShotViaConn(t, conn, nil, "sh",
			[]string{"-c", "test -n \"$SSH_AUTH_SOCK\" && test -S \"$SSH_AUTH_SOCK\" && echo " + agentForwardReadyMarker})
		lastCheck = out

		return ok && strings.TrimSpace(out) == agentForwardReadyMarker
	})
	if waitErr != nil {
		t.Fatalf("agent forward never became ready: %v (last check output: %q)", waitErr, lastCheck)
	}
}

// newLocalClientForRunningServer connects a real LocalClient to the local
// daemon already listening on the socket implied by GRAFT_STATE_HOME (the
// same resolution NewServer uses), rather than a fake/injected server.
func newLocalClientForRunningServer(t *testing.T) *LocalClient {
	t.Helper()

	sockPath, err := DaemonSocketPathForCurrentHost(ServerRoleLocal)
	test.That(t, err, test.ShouldBeNil)

	conn, err := grpc.NewClient(
		"unix://"+sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(func() { conn.Close() })

	var outBuf, errBuf bytes.Buffer

	return &LocalClient{
		GraftServiceClient: graftv1.NewGraftServiceClient(conn),
		outWriter:          nopWriteCloser{&outBuf},
		errWriter:          nopWriteCloser{&errBuf},
	}
}

// waitForConnectedConn polls until name reaches ConnectionStateConnected and
// returns it.
func waitForConnectedConn(t *testing.T, srv *Server, name string) *Connection {
	t.Helper()

	var conn *Connection

	test.That(t, waitFor(t, func() bool {
		c, err := srv.connMgr.Connection(name)
		if err != nil {
			return false
		}

		state, _ := c.State()
		if state != ConnectionStateConnected {
			return false
		}

		conn = c

		return true
	}), test.ShouldBeNil)

	return conn
}

// runOneShotViaConn runs a one-shot command through conn against its real
// remote daemon and returns combined stdout+stderr and whether it exited zero.
func runOneShotViaConn(t *testing.T, conn *Connection, extraEnv []string, command string, args []string) (string, bool) {
	t.Helper()

	runningCmd, err := conn.RunCommand(
		t.Context(), "", false, command, args, extraEnv, false, false, true, true,
		graftv1.CommandPersistence_COMMAND_PERSISTENCE_UNKNOWN,
	)
	test.That(t, err, test.ShouldBeNil)

	var stdout, stderr bytes.Buffer

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		io.Copy(&stdout, runningCmd.Stdout()) //nolint:errcheck
	}()

	go func() {
		defer wg.Done()

		io.Copy(&stderr, runningCmd.Stderr()) //nolint:errcheck
	}()

	status, waitErr := runningCmd.Wait()
	test.That(t, waitErr, test.ShouldBeNil)

	wg.Wait()
	runningCmd.Release()

	return stdout.String() + stderr.String(), status == 0
}

// startFakeSSHAgent serves a real SSH agent protocol implementation (loaded
// with one freshly generated key) over a local unix socket, standing in for
// the user's real local ssh-agent. It returns the socket path (for
// SSH_AUTH_SOCK) and the loaded key's fingerprint.
func startFakeSSHAgent(t *testing.T) (string, string) {
	t.Helper()

	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	test.That(t, err, test.ShouldBeNil)

	sshPubKey, err := ssh.NewPublicKey(pubKey)
	test.That(t, err, test.ShouldBeNil)

	keyring := agent.NewKeyring()
	test.That(t, keyring.Add(agent.AddedKey{PrivateKey: privKey}), test.ShouldBeNil)

	sockPath := testSocketPath(t, "fake-agent.sock")

	var lc net.ListenConfig

	listener, err := lc.Listen(t.Context(), "unix", sockPath)
	test.That(t, err, test.ShouldBeNil)

	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			c, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			go agent.ServeAgent(keyring, c) //nolint:errcheck
		}
	}()

	return sockPath, ssh.FingerprintSHA256(sshPubKey)
}
