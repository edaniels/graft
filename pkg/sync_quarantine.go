package graft

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"

	"github.com/edaniels/graft/errors"
)

// When a persisted sync session fails to load at daemon startup, mutagen logs
// a warning and skips it, leaving the file on disk. The session is then
// invisible to every loaded-session lookup (findExistingSessionByName) and to
// the orphan reaper, so graft creates a fresh duplicate under the same name
// on every daemon start; each duplicate cold-scans from an empty ancestor
// archive. This file implements the quarantine that closes the loop: anything
// in the sessions directory that did not load is moved aside (with its
// archive sibling) and logged at ERROR with the reason.

var (
	// failedSessionLoadPattern matches mutagen's per-session startup warning
	// (synchronization.Manager): "Failed to load session <id>: <reason>".
	failedSessionLoadPattern = regexp.MustCompile(`^Failed to load session (\S+): (?s)(.*)$`)
	// ignoredSessionIDPattern matches mutagen's warning for directory entries
	// that aren't valid session identifiers at all. That warning is built
	// with fmt.Sprint, which puts no space between the trailing-colon string
	// and the id, so the space is optional here.
	ignoredSessionIDPattern = regexp.MustCompile(`^Ignoring invalid session identifier:\s*(\S+)`)
)

// sessionLoadFailureCapture collects the per-session load failure reasons
// mutagen logs while its manager loads persisted sessions, so the quarantine
// can report why each file was set aside.
type sessionLoadFailureCapture struct {
	mu      sync.Mutex
	reasons map[string]string
}

func newSessionLoadFailureCapture() *sessionLoadFailureCapture {
	return &sessionLoadFailureCapture{reasons: map[string]string{}}
}

func (c *sessionLoadFailureCapture) record(id, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.reasons[id] = reason
}

func (c *sessionLoadFailureCapture) reason(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.reasons[id]
}

func (c *sessionLoadFailureCapture) snapshot() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make(map[string]string, len(c.reasons))
	maps.Copy(out, c.reasons)

	return out
}

// sessionLoadFailureLogHandler forwards every record to its inner handler
// while teeing mutagen's session load failures into a capture.
type sessionLoadFailureLogHandler struct {
	slog.Handler

	capture *sessionLoadFailureCapture
}

func (h *sessionLoadFailureLogHandler) Handle(ctx context.Context, r slog.Record) error {
	if m := failedSessionLoadPattern.FindStringSubmatch(r.Message); m != nil {
		h.capture.record(m[1], m[2])
	} else if m := ignoredSessionIDPattern.FindStringSubmatch(r.Message); m != nil {
		h.capture.record(m[1], "invalid session identifier")
	}

	return errors.Wrap(h.Handler.Handle(ctx, r))
}

func (h *sessionLoadFailureLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &sessionLoadFailureLogHandler{Handler: h.Handler.WithAttrs(attrs), capture: h.capture}
}

func (h *sessionLoadFailureLogHandler) WithGroup(name string) slog.Handler {
	return &sessionLoadFailureLogHandler{Handler: h.Handler.WithGroup(name), capture: h.capture}
}

// quarantineUnloadableSyncSessions moves every entry in the sync sessions
// directory that the manager did not load into a quarantine subdirectory
// (alongside its archive, when one exists), logging each at ERROR with the
// reason captured during loading. Best effort: a single failed move is logged
// and skipped rather than blocking startup.
func quarantineUnloadableSyncSessions(
	syncStateDir string,
	mgr *synchronization.Manager,
	reasons map[string]string,
) error {
	_, states, err := mgr.List(context.Background(), &selection.Selection{All: true}, 0)
	if err != nil {
		return errors.Wrap(err)
	}

	loaded := make(map[string]bool, len(states))
	for _, state := range states {
		loaded[state.GetSession().GetIdentifier()] = true
	}

	sessionsDir := filepath.Join(syncStateDir, "sessions")

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return errors.Wrap(err)
	}

	for _, entry := range entries {
		id := entry.Name()
		if loaded[id] {
			continue
		}

		reason := reasons[id]
		if reason == "" {
			reason = "session did not load; reason not captured (see startup logs)"
		}

		slog.Error("quarantining unloadable sync session",
			"session_id", id, "reason", reason, "path", filepath.Join(sessionsDir, id))

		if err := moveSyncSessionIntoQuarantine(syncStateDir, id); err != nil {
			slog.Error("error quarantining sync session", "session_id", id, "error", err)
		}
	}

	return nil
}

// moveSyncSessionIntoQuarantine moves the session file for id and its archive
// sibling (when present) into syncStateDir/quarantine/.
func moveSyncSessionIntoQuarantine(syncStateDir, id string) error {
	for _, sub := range []string{"sessions", "archives"} {
		src := filepath.Join(syncStateDir, sub, id)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return errors.Wrap(err)
		}

		dstDir := filepath.Join(syncStateDir, "quarantine", sub)
		if err := os.MkdirAll(dstDir, DirPerms); err != nil {
			return errors.Wrap(err)
		}

		if err := os.Rename(src, filepath.Join(dstDir, id)); err != nil {
			return errors.Wrap(err)
		}
	}

	return nil
}
