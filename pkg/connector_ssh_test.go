package graft

import (
	"context"
	"log/slog"
	"runtime"
	"syscall"
	"testing"
	"time"

	"go.viam.com/test"
)

func TestDialProxyCommandEcho(t *testing.T) {
	conn, err := dialProxyCommand(context.Background(), slog.Default(), "cat")
	test.That(t, err, test.ShouldBeNil)

	msg := []byte("hello proxy")
	n, err := conn.Write(msg)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, n, test.ShouldEqual, len(msg))

	buf := make([]byte, 64)
	n, err = conn.Read(buf)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(buf[:n]), test.ShouldEqual, "hello proxy")

	err = conn.Close()
	test.That(t, err, test.ShouldBeNil)
}

func TestDialProxyCommandClose(t *testing.T) {
	conn, err := dialProxyCommand(context.Background(), slog.Default(), "cat")
	test.That(t, err, test.ShouldBeNil)

	pConn, ok := conn.(*proxyCommandConn)
	test.That(t, ok, test.ShouldBeTrue)

	pid := pConn.cmd.Process.Pid

	err = conn.Close()
	test.That(t, err, test.ShouldBeNil)

	// Wait for the process to be reaped by polling with a channel-based timeout.
	done := make(chan struct{})

	go func() {
		for syscall.Kill(pid, 0) == nil {
			// Yield to allow the background reaper goroutine to run.
			runtime.Gosched()
		}

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		test.That(t, "process still alive after Close()", test.ShouldBeEmpty)
	}
}

func TestSSHConnectorUsesResolvedConfig(t *testing.T) {
	resolver := &fakeSSHConfigResolver{
		values: map[string]map[string]string{
			"myalias": {
				"Hostname":     "real.host.example.com",
				"Port":         "2222",
				"User":         "configuser",
				"ProxyCommand": "nc %h %p",
			},
		},
		allValues: map[string]map[string][]string{},
	}

	resolved := resolveSSHConfig(resolver, "myalias", "", "")

	test.That(t, resolved.Hostname, test.ShouldEqual, "real.host.example.com")
	test.That(t, resolved.Port, test.ShouldEqual, "2222")
	test.That(t, resolved.User, test.ShouldEqual, "configuser")
	test.That(t, resolved.ProxyCommand, test.ShouldEqual, "nc real.host.example.com 2222")
}

func TestSSHConnectorNoProxyCommandUsesTCPPath(t *testing.T) {
	resolver := &fakeSSHConfigResolver{
		values: map[string]map[string]string{
			"myhost": {
				"Hostname": "resolved.host.com",
				"Port":     "22",
			},
		},
		allValues: map[string]map[string][]string{},
	}

	resolved := resolveSSHConfig(resolver, "myhost", "", "")

	test.That(t, resolved.ProxyCommand, test.ShouldBeEmpty)
	test.That(t, resolved.Hostname, test.ShouldEqual, "resolved.host.com")
}
