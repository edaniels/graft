package graft

import (
	"context"
	"io"
	"slices"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	CorrelationIDHeaderName = "co_id"
)

func (srv *Server) OOBUnaryServerInterceptor(
	ctx context.Context,
	req any,
	_ *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		coID := md[CorrelationIDHeaderName]
		if len(coID) != 0 {
			ctx = ContextWithOOBWriter(ctx, srv.ClientOOBWriter(coID[0]))
		}
	}

	return handler(ctx, req)
}

// A clientWriter is an io.Writer used to send out-of-band (OOB) messages to clients connected to a daemon.
type clientWriter struct {
	srv  *Server
	coID string
}

func (cw clientWriter) Write(b []byte) (int, error) {
	return cw.srv.WriteOOB(cw.coID, slices.Clone(b))
}

func (srv *Server) ClientOOBWriter(coID string) io.Writer {
	return &clientWriter{srv: srv, coID: coID}
}

// WriteOOB is used to explicitly send a message to an intended target (coID).
func (srv *Server) WriteOOB(coID string, b []byte) (int, error) {
	// TODO(erd): O(n) listener scan; expensive at high message volume.
	srv.oobListenersMu.Lock()

	listeners := make([]*oobListener, 0, len(srv.oobListeners))
	for listener := range srv.oobListeners {
		listeners = append(listeners, listener)
	}

	srv.oobListenersMu.Unlock()

	msg := oobMessage{coID: coID, data: b}
	for _, listener := range listeners {
		// TODO(erd): maybe timeout
		// TODO(erd): what about stalls
		listener.messages <- msg
	}

	return len(b), nil
}

type oobMessage struct {
	coID string
	data []byte
}

type oobListener struct {
	messages chan oobMessage
}

// listenToOOB returns a new listener that can be used to write out-of-band messages to (see: [Server.WriteOOB]).
func (srv *Server) listenToOOB() (*oobListener, func()) {
	// TODO(erd): buffer?
	listener := &oobListener{messages: make(chan oobMessage)}

	srv.serverMu.Lock()
	srv.oobListeners[listener] = struct{}{}
	srv.serverMu.Unlock()

	return listener, func() {
		srv.serverMu.Lock()
		delete(srv.oobListeners, listener)
		srv.serverMu.Unlock()
	}
}
