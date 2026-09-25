package graft

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"go.viam.com/test"

	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// bridgeTestHandler records handled slog records and gates them by a minimum
// level, standing in for the daemon's leveled handler. Derived handlers
// (WithAttrs) apply their attributes to each record, as slog requires.
type bridgeTestHandler struct {
	minLevel slog.Level
	attrs    []slog.Attr
	sink     *bridgeTestSink
}

type bridgeTestSink struct {
	mu   sync.Mutex
	recs []bridgeTestRecord
}

type bridgeTestRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func newBridgeTestHandler(minLevel slog.Level) *bridgeTestHandler {
	return &bridgeTestHandler{minLevel: minLevel, sink: &bridgeTestSink{}}
}

func (h *bridgeTestHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minLevel
}

func (h *bridgeTestHandler) Handle(_ context.Context, r slog.Record) error {
	rec := bridgeTestRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}

	for _, a := range h.attrs {
		rec.attrs[a.Key] = a.Value.String()
	}

	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()

		return true
	})

	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()

	h.sink.recs = append(h.sink.recs, rec)

	return nil
}

func (h *bridgeTestHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &bridgeTestHandler{minLevel: h.minLevel, attrs: append(h.attrs, attrs...), sink: h.sink}
}

func (h *bridgeTestHandler) WithGroup(string) slog.Handler { return h }

func (h *bridgeTestHandler) records() []bridgeTestRecord {
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()

	out := make([]bridgeTestRecord, len(h.sink.recs))
	copy(out, h.sink.recs)

	return out
}

func TestMutagenSyncLogBridge(t *testing.T) {
	t.Run("enabled for every level so records reach Handle", func(t *testing.T) {
		bridge := &mutagenSyncLogBridge{inner: newBridgeTestHandler(slog.LevelInfo)}
		test.That(t, bridge.Enabled(t.Context(), slog.LevelDebug-4), test.ShouldBeTrue)
	})

	t.Run("sync loop termination is promoted to WARN", func(t *testing.T) {
		inner := newBridgeTestHandler(slog.LevelInfo)
		logger := slog.New(&mutagenSyncLogBridge{inner: inner})

		logger.Debug("Synchronization loop terminated with error: connection reset by peer")

		recs := inner.records()
		test.That(t, len(recs), test.ShouldEqual, 1)
		test.That(t, recs[0].level, test.ShouldEqual, slog.LevelWarn)
		test.That(t, recs[0].msg, test.ShouldContainSubstring, "connection reset by peer")
	})

	t.Run("promotion keeps record attributes", func(t *testing.T) {
		inner := newBridgeTestHandler(slog.LevelInfo)
		logger := slog.New(&mutagenSyncLogBridge{inner: inner}).With("scope", "sync_abc")

		logger.Debug("Synchronization loop terminated with error: boom")

		recs := inner.records()
		test.That(t, len(recs), test.ShouldEqual, 1)
		test.That(t, recs[0].level, test.ShouldEqual, slog.LevelWarn)
		test.That(t, recs[0].attrs["scope"], test.ShouldEqual, "sync_abc")
	})

	t.Run("other debug records stay below the daemon's level", func(t *testing.T) {
		inner := newBridgeTestHandler(slog.LevelInfo)
		logger := slog.New(&mutagenSyncLogBridge{inner: inner})

		logger.Debug("Entering synchronization loop")
		logger.Debug("Scanning endpoints")

		test.That(t, inner.records(), test.ShouldBeEmpty)
	})

	t.Run("canceled loop terminations are not promoted", func(t *testing.T) {
		// Pause, Terminate, and daemon shutdown all end the loop by canceling
		// its context; promoting those to WARN would cry wolf on every clean
		// stop.
		inner := newBridgeTestHandler(slog.LevelInfo)
		logger := slog.New(&mutagenSyncLogBridge{inner: inner})

		logger.Debug("Synchronization loop terminated with error:cancelled during polling")
		logger.Debug("Synchronization loop terminated with error:context canceled")

		test.That(t, inner.records(), test.ShouldBeEmpty)
	})

	t.Run("debug records pass when the daemon runs at debug", func(t *testing.T) {
		inner := newBridgeTestHandler(slog.LevelDebug)
		logger := slog.New(&mutagenSyncLogBridge{inner: inner})

		logger.Debug("Entering synchronization loop")

		recs := inner.records()
		test.That(t, len(recs), test.ShouldEqual, 1)
		test.That(t, recs[0].level, test.ShouldEqual, slog.LevelDebug)
	})

	t.Run("info and above pass through unchanged", func(t *testing.T) {
		inner := newBridgeTestHandler(slog.LevelInfo)
		logger := slog.New(&mutagenSyncLogBridge{inner: inner})

		logger.Info("Session initialized")
		logger.Warn("Failed to load session sync_abc: boom")

		recs := inner.records()
		test.That(t, len(recs), test.ShouldEqual, 2)
		test.That(t, recs[0].level, test.ShouldEqual, slog.LevelInfo)
		test.That(t, recs[1].level, test.ShouldEqual, slog.LevelWarn)
	})
}

func TestFormatSyncStatusDescriptionRendersLastError(t *testing.T) {
	// A sync loop dying surfaces its termination error via SyncStatus.LastError;
	// the CLI's status rendering must show it, or the failure is invisible.
	desc := formatSyncStatusDescription(&graftv1.SyncStatus{
		Status:    "Watching",
		LastError: "unable to connect to beta: connection reset",
	})

	test.That(t, desc, test.ShouldContainSubstring, "Last error")
	test.That(t, desc, test.ShouldContainSubstring, "connection reset")
}
