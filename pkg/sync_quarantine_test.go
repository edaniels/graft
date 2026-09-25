package graft

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/encoding"
	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
	"go.viam.com/test"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// seedSyncSessionFile writes a protobuf-encoded session into dir under its
// own identifier, mimicking what mutagen persists for a live session.
func seedSyncSessionFile(t *testing.T, dir string, sess *synchronization.Session) {
	t.Helper()
	test.That(t, encoding.MarshalAndSaveProtobuf(filepath.Join(dir, sess.GetIdentifier()), sess), test.ShouldBeNil)
}

// graftSyncDirForTest returns the sync state dir NewServer will use.
func graftSyncDirForTest(t *testing.T) string {
	t.Helper()

	homeDir, err := os.UserHomeDir()
	test.That(t, err, test.ShouldBeNil)

	syncDir, err := graftSyncDir(homeDir, ServerRoleLocal, "")
	test.That(t, err, test.ShouldBeNil)

	return syncDir
}

func TestQuarantineUnloadableSyncSessionsOnStartup(t *testing.T) {
	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	syncDir := graftSyncDirForTest(t)
	sessionsDir := filepath.Join(syncDir, "sessions")
	archivesDir := filepath.Join(syncDir, "archives")

	test.That(t, os.MkdirAll(sessionsDir, DirPerms), test.ShouldBeNil)
	test.That(t, os.MkdirAll(archivesDir, DirPerms), test.ShouldBeNil)

	// 1. Garbage content under a valid session identifier (load fails at
	// protobuf unmarshal), plus its orphaned archive sibling.
	garbageID := "sync_" + strings.Repeat("1", 43)
	test.That(t, os.WriteFile(filepath.Join(sessionsDir, garbageID), []byte("not a session"), 0o600), test.ShouldBeNil)
	test.That(t, os.WriteFile(filepath.Join(archivesDir, garbageID), []byte("not an archive"), 0o600), test.ShouldBeNil)

	// 2. A structurally valid session whose alpha path is relative: the exact
	// "invalid session found on disk" failure behind the duplicate-session
	// incident (mutagen's EnsureValid rejects relative local paths on load).
	relativeID := "sync_" + strings.Repeat("2", 43)
	seedSyncSessionFile(t, sessionsDir, &synchronization.Session{
		Identifier:   relativeID,
		Version:      synchronization.Version_Version1,
		CreationTime: timestamppb.Now(),
		Alpha:        &urlpkg.URL{Protocol: urlpkg.Protocol_Local, Path: "."},
		Beta:         &urlpkg.URL{Protocol: urlpkg.Protocol(syncProtoNum), Host: "conn", Path: "/remote/x"},
		Name:         "graft-deadbeef",
	})

	// 3. A file whose name is not a valid session identifier at all.
	test.That(t, os.WriteFile(filepath.Join(sessionsDir, "not-a-session"), []byte("junk"), 0o600), test.ShouldBeNil)

	srv, err := NewServer(&RootConfig{}, ServerRoleLocal, "", false, &BufferedLineWriter{MaxLines: 100}, "", slog.LevelDebug)
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(srv.synchronizationManager.Shutdown)

	// Everything unloadable left the sessions directory.
	entries, err := os.ReadDir(sessionsDir)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, entries, test.ShouldBeEmpty)

	// ...and landed in quarantine, archive sibling included.
	for _, name := range []string{garbageID, relativeID, "not-a-session"} {
		_, statErr := os.Stat(filepath.Join(syncDir, "quarantine", "sessions", name))
		test.That(t, statErr, test.ShouldBeNil)
	}

	_, statErr := os.Stat(filepath.Join(syncDir, "quarantine", "archives", garbageID))
	test.That(t, statErr, test.ShouldBeNil)

	// A fresh session can be created with the same name as the quarantined
	// one; nothing on disk blocks or confuses it anymore.
	sessionID, err := srv.synchronizationManager.Create(
		t.Context(),
		&urlpkg.URL{Protocol: urlpkg.Protocol_Local, Path: t.TempDir()},
		&urlpkg.URL{Protocol: urlpkg.Protocol(syncProtoNum), Host: "conn", Path: "/remote/x"},
		&synchronization.Configuration{},
		&synchronization.Configuration{},
		&synchronization.Configuration{},
		"graft-deadbeef",
		nil,
		true,
		"",
	)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, sessionID, test.ShouldNotBeEmpty)
}

func TestQuarantineKeepsLoadableSessions(t *testing.T) {
	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	syncDir := graftSyncDirForTest(t)
	t.Setenv("MUTAGEN_DATA_DIRECTORY", syncDir)

	// Seed a genuinely loadable session via a real manager (paused: no
	// endpoint connections attempted).
	seedMgr, err := synchronization.NewManager(logging.NewLoggerOnSlogger(slog.Default()))
	test.That(t, err, test.ShouldBeNil)

	_, err = seedMgr.Create(
		t.Context(),
		&urlpkg.URL{Protocol: urlpkg.Protocol_Local, Path: t.TempDir()},
		&urlpkg.URL{Protocol: urlpkg.Protocol(syncProtoNum), Host: "conn", Path: "/remote/x"},
		&synchronization.Configuration{},
		&synchronization.Configuration{},
		&synchronization.Configuration{},
		"graft-validname",
		nil,
		true,
		"",
	)
	test.That(t, err, test.ShouldBeNil)
	seedMgr.Shutdown()

	srv, err := NewServer(&RootConfig{}, ServerRoleLocal, "", false, &BufferedLineWriter{MaxLines: 100}, "", slog.LevelDebug)
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(srv.synchronizationManager.Shutdown)

	// The loadable session was loaded and left in place.
	test.That(t, sessionNames(t, srv.synchronizationManager), test.ShouldContain, "graft-validname")

	_, statErr := os.Stat(filepath.Join(syncDir, "quarantine"))
	test.That(t, os.IsNotExist(statErr), test.ShouldBeTrue)
}

// recordingSlogHandler is a minimal slog.Handler that notes every message it
// receives.
type recordingSlogHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recordingSlogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingSlogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.msgs = append(h.msgs, r.Message)

	return nil
}

func (h *recordingSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingSlogHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingSlogHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.msgs)
}

func TestSessionLoadFailureLogHandler(t *testing.T) {
	capture := newSessionLoadFailureCapture()
	inner := &recordingSlogHandler{}
	handler := &sessionLoadFailureLogHandler{Handler: inner, capture: capture}
	logger := slog.New(handler)

	logger.Warn("Failed to load session sync_abc: invalid session found on disk: invalid alpha URL: local URL with relative path")
	logger.Warn("Ignoring invalid session identifier: junk-name")
	// Mutagen's non-f Warn builds this message with fmt.Sprint, which puts no
	// space between the trailing-colon string and the id.
	logger.Warn("Ignoring invalid session identifier:junk-name-nospace")
	logger.Info("unrelated message")

	test.That(t, capture.reason("sync_abc"), test.ShouldContainSubstring, "local URL with relative path")
	test.That(t, capture.reason("junk-name"), test.ShouldEqual, "invalid session identifier")
	test.That(t, capture.reason("junk-name-nospace"), test.ShouldEqual, "invalid session identifier")
	test.That(t, capture.reason("sync_other"), test.ShouldBeEmpty)

	// Every record still reached the inner handler.
	test.That(t, inner.count(), test.ShouldEqual, 4)

	// Derived loggers (mutagen's subloggers use WithAttrs) share the capture.
	slog.New(handler.WithAttrs([]slog.Attr{slog.String("scope", "sync_abc")})).Warn("Failed to load session sync_def: boom")
	test.That(t, capture.reason("sync_def"), test.ShouldEqual, "boom")
}
