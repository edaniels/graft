package graft

import (
	"context"
	"log/slog"
	"strings"

	"github.com/edaniels/graft/errors"
)

// syncLoopTerminatedMsg prefixes the one mutagen controller log line that
// says WHY a sync loop died (controller.run). Mutagen emits it at DEBUG; it
// is promoted to WARN so a perpetually dying sync loop cannot restart
// unnoticed.
//
// TODO(erd): investigate the sync loop death cadence observed in production:
// the loop died and restarted every ~17-20 minutes (36 restarts/day) while
// the daemon logged "health check failed; triggering reconnect" 278 times in
// a day. Suspected Tailscale SSH check-mode cycling the transport. Correlate
// loop terminations (now visible at WARN via this bridge) with health-check
// reconnects to confirm.
const syncLoopTerminatedMsg = "Synchronization loop terminated with error:"

// mutagenSyncLogBridge adapts the daemon's slog handler for mutagen's
// loggers. Mutagen's non-f log calls bypass slog's Enabled gate entirely
// (they invoke Handler.Handle directly), so below-level records reach the
// handler no matter what the daemon's level is; the bridge reports itself
// enabled for everything and applies the daemon handler's own level in
// Handle. The one exception: the sync loop termination line, which is
// promoted to WARN so sync loop failures are always visible.
type mutagenSyncLogBridge struct {
	inner slog.Handler
}

func (h *mutagenSyncLogBridge) Enabled(context.Context, slog.Level) bool {
	// Let every record reach Handle, which applies the daemon's level there;
	// slog drops disabled records before they ever reach a handler.
	return true
}

func (h *mutagenSyncLogBridge) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < slog.LevelInfo && strings.HasPrefix(r.Message, syncLoopTerminatedMsg) && !isSyncLoopCancellation(r.Message) {
		promoted := r.Clone()
		promoted.Level = slog.LevelWarn

		return errors.Wrap(h.inner.Handle(ctx, promoted))
	}

	if !h.inner.Enabled(ctx, r.Level) {
		return nil
	}

	return errors.Wrap(h.inner.Handle(ctx, r))
}

// isSyncLoopCancellation reports whether a loop termination message is just
// the loop's context being canceled - the expected end of every Pause,
// Terminate, and daemon shutdown, not a failure worth a WARN. Mutagen spells
// these "cancelled during ..."; context.Canceled spells it "canceled".
func isSyncLoopCancellation(msg string) bool {
	return strings.Contains(msg, "cancelled") || strings.Contains(msg, "context canceled")
}

func (h *mutagenSyncLogBridge) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &mutagenSyncLogBridge{inner: h.inner.WithAttrs(attrs)}
}

func (h *mutagenSyncLogBridge) WithGroup(name string) slog.Handler {
	return &mutagenSyncLogBridge{inner: h.inner.WithGroup(name)}
}
