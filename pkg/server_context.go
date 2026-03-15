package graft

import (
	"context"
	"io"
)

// all context keys used for passing around data as basically globals when it doesn't feel
// like there's a better way.
type ctxKey int

const (
	ctxKeyOOBWriter ctxKey = iota
	ctxKeyConnRemoteClientConn
)

// OOBWriterFromContext attempts to grab an OOB writer from context and if it's not found, the returned writer
// will discard all writes.
func OOBWriterFromContext(ctx context.Context) io.Writer {
	writer, ok := ctx.Value(ctxKeyOOBWriter).(io.Writer)
	if !ok {
		return io.Discard
	}

	return writer
}

// ContextWithOOBWriter attaches an OOB writer to the context and returns that context.
func ContextWithOOBWriter(ctx context.Context, writer io.Writer) context.Context {
	return context.WithValue(ctx, ctxKeyOOBWriter, writer)
}
