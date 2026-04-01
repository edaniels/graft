package graft

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// WatchPorts polls for listening ports owned by this daemon's process tree and
// streams snapshots to the local daemon whenever the set changes.
func (srv *Server) WatchPorts(_ *graftv1.WatchPortsRequest, stream graftv1.GraftService_WatchPortsServer) error {
	if srv.role != ServerRoleRemote {
		return errors.New("port tracking not available (not a remote daemon)")
	}

	var lastPorts []ListeningPort

	// Send initial snapshot (may be empty).
	ports, err := GetPortsForParent(os.Getpid())
	if err == nil {
		lastPorts = ports
	}

	if err := stream.Send(listeningPortsToProto(lastPorts)); err != nil {
		return errors.Wrap(err)
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ports, err := GetPortsForParent(os.Getpid())
			if err != nil {
				continue
			}

			if listeningPortsEqual(lastPorts, ports) {
				continue
			}

			lastPorts = ports

			if err := stream.Send(listeningPortsToProto(ports)); err != nil {
				return errors.Wrap(err)
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

func listeningPortsEqual(a, b []ListeningPort) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i].Port != b[i].Port || a[i].Protocol != b[i].Protocol || a[i].Host != b[i].Host {
			return false
		}
	}

	return true
}

func listeningPortsToProto(ports []ListeningPort) *graftv1.WatchPortsResponse {
	resp := &graftv1.WatchPortsResponse{
		Ports: make([]*graftv1.PortInfo, 0, len(ports)),
	}

	for _, lp := range ports {
		resp.Ports = append(resp.Ports, &graftv1.PortInfo{
			Port:     uint32(lp.Port), //nolint:gosec // overflow okay
			Host:     lp.Host,
			Protocol: lp.Protocol,
		})
	}

	return resp
}

// ForwardPort handles a single forwarded TCP connection or UDP session.
// The first message must be ForwardPortStart identifying the target; subsequent messages
// relay raw bytes between the gRPC stream and a local network connection on the remote.
//

func (srv *Server) ForwardPort(stream graftv1.GraftService_ForwardPortServer) error {
	req, err := stream.Recv()
	if err != nil {
		return errors.Wrap(err)
	}

	startMsg := req.GetStart()
	if startMsg == nil {
		return errors.New("first ForwardPort message must be ForwardPortStart")
	}

	protocol := startMsg.GetProtocol()

	switch protocol {
	case "tcp", "udp":
	default:
		return errors.Errorf("unsupported protocol %q; must be \"tcp\" or \"udp\"", protocol)
	}

	host := startMsg.GetHost()

	// Normalize unspecified addresses (including IPv6-mapped forms like ::ffff:0.0.0.0)
	// to loopback for dialing.
	if host == "" {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}

	addr := net.JoinHostPort(host, strconv.Itoa(int(startMsg.GetPort())))

	slog.DebugContext(stream.Context(), "forwarding port",
		"protocol", protocol, "addr", addr)

	dialer := net.Dialer{
		Timeout: 10 * time.Second,
	}

	conn, err := dialer.DialContext(stream.Context(), protocol, addr)
	if err != nil {
		return errors.WrapPrefix(err, "error dialing forwarded port")
	}
	defer conn.Close()

	return relayBidi(stream, conn)
}

// relayBidi copies data bidirectionally between a gRPC ForwardPort stream and a net.Conn.
// It propagates TCP half-close so that protocols relying on FIN (e.g. HTTP request then
// close write, wait for response) work correctly.
//
// On error, the function returns immediately so that ForwardPort returns and gRPC tears
// down the stream, unblocking any goroutine stuck on stream.Recv. On clean half-close,
// it waits for the second direction to finish normally. errCh has buffer 2, so goroutines
// that exit after the function returns can always send without blocking.
//
//nolint:gocognit,cyclop // bidirectional relay inherently has branching in two goroutines
func relayBidi(stream graftv1.GraftService_ForwardPortServer, conn net.Conn) error {
	errCh := make(chan error, 2) //nolint:mnd // two directions

	// gRPC stream -> network connection
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					// Client closed send side; propagate half-close to the network conn.
					if tc, ok := conn.(*net.TCPConn); ok {
						tc.CloseWrite() //nolint:errcheck // best-effort half-close during shutdown
					}

					errCh <- nil

					return
				}

				errCh <- err

				return
			}

			if payload := req.GetPayload(); len(payload) > 0 {
				if _, err := conn.Write(payload); err != nil {
					errCh <- err

					return
				}
			}
		}
	}()

	// network connection -> gRPC stream
	go func() {
		buf := make([]byte, 32*1024)

		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&graftv1.ForwardPortResponse{
					Payload: buf[:n],
				}); sendErr != nil {
					errCh <- sendErr

					return
				}
			}

			if err != nil {
				if errors.Is(err, io.EOF) {
					errCh <- nil
				} else {
					errCh <- err
				}

				return
			}
		}
	}()

	err1 := <-errCh
	if err1 != nil {
		// Error: close conn to unblock the Read goroutine promptly. This is not
		// redundant with the deferred conn.Close() in ForwardPort; it ensures the
		// goroutine makes progress immediately rather than waiting for gRPC teardown.
		// Returning causes ForwardPort to return, tearing down the stream and
		// unblocking any goroutine stuck on stream.Recv.
		conn.Close()

		return err1
	}

	// For non-TCP connections (e.g. UDP), CloseWrite is not available so the half-close
	// above was a no-op. Close the connection to unblock the Read goroutine, then drain
	// the second result and return. conn.Close() unblocks Read but NOT stream.Recv();
	// returning from the handler is what tears down the gRPC stream and unblocks Recv.
	if _, isTCP := conn.(*net.TCPConn); !isTCP {
		conn.Close()
		<-errCh

		return nil
	}

	// TCP clean half-close: wait for the second direction to finish normally.
	err2 := <-errCh

	return err2
}

// AddPortForwards adds explicit port forwards to a connection.
func (srv *Server) AddPortForwards(
	ctx context.Context,
	req *graftv1.AddPortForwardsRequest,
) (*graftv1.AddPortForwardsResponse, error) {
	srv.serverMu.Lock()
	defer srv.serverMu.Unlock()

	conn, err := srv.connMgr.Connection(req.GetConnectionName())
	if err != nil {
		return nil, err
	}

	daemon := conn.lockedDaemon()

	// Add each forward and collect spec strings for config persistence.
	specStrs := make([]string, 0, len(req.GetPorts()))

	for _, p := range req.GetPorts() {
		spec := PortForwardSpecFromProto(p)

		if err := daemon.AddExplicitPortForward(ctx, spec); err != nil {
			return nil, err
		}

		specStrs = append(specStrs, spec.String())
	}

	for idx, connConfig := range srv.rootConfig.Connections {
		if connConfig.Name == conn.Name() {
			for _, specStr := range specStrs {
				if !slices.Contains(connConfig.Ports, specStr) {
					connConfig.Ports = append(connConfig.Ports, specStr)
				}
			}

			srv.rootConfig.Connections[idx] = connConfig

			break
		}
	}

	srv.persistConfig()

	return &graftv1.AddPortForwardsResponse{}, nil
}

// RemovePortForwards removes explicit port forwards from a connection.
func (srv *Server) RemovePortForwards(
	_ context.Context,
	req *graftv1.RemovePortForwardsRequest,
) (*graftv1.RemovePortForwardsResponse, error) {
	srv.serverMu.Lock()
	defer srv.serverMu.Unlock()

	conn, err := srv.connMgr.Connection(req.GetConnectionName())
	if err != nil {
		return nil, err
	}

	daemon := conn.lockedDaemon()

	var autoDetected []*graftv1.ExplicitPortForwardSpec

	toRemove := make(map[string]struct{}, len(req.GetPorts()))

	for _, p := range req.GetPorts() {
		spec := PortForwardSpecFromProto(p)

		if !daemon.RemoveExplicitPortForward(spec) {
			autoDetected = append(autoDetected, p)
		}

		toRemove[spec.String()] = struct{}{}
	}

	for idx, connConfig := range srv.rootConfig.Connections {
		if connConfig.Name == conn.Name() {
			connConfig.Ports = slices.DeleteFunc(connConfig.Ports, func(s string) bool {
				_, remove := toRemove[s]

				return remove
			})

			srv.rootConfig.Connections[idx] = connConfig

			break
		}
	}

	srv.persistConfig()

	return &graftv1.RemovePortForwardsResponse{
		AutoDetectedPorts: autoDetected,
	}, nil
}
