package graft

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.viam.com/test"
)

func TestGenerateDaemonIdentity(t *testing.T) {
	id := generateDaemonIdentity()

	// Format: adjective-noun-verb
	parts := strings.Split(id, "-")
	test.That(t, len(parts), test.ShouldEqual, 3)

	// Check adjective is from the list
	test.That(t, identityAdjectives, test.ShouldContain, parts[0])

	// Check noun is from the list
	test.That(t, identityNouns, test.ShouldContain, parts[1])

	// Check verb is from the list
	test.That(t, identityVerbs, test.ShouldContain, parts[2])

	// Two generated identities should differ (with very high probability)
	id2 := generateDaemonIdentity()
	test.That(t, id, test.ShouldNotEqual, id2)
}

func TestDaemonIdentityPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	identityPath := filepath.Join(tmpDir, "identity")

	// First call generates and persists
	id1, err := daemonIdentityFromPath(identityPath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, id1, test.ShouldNotBeEmpty)

	// File was written
	data, err := os.ReadFile(identityPath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, id1)

	// Second call reads the same identity
	id2, err := daemonIdentityFromPath(identityPath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, id2, test.ShouldEqual, id1)
}

func TestDaemonSocketPathWithIdentity(t *testing.T) {
	stateHome := "/home/user/.local/state/graft"

	// Without identity
	path, err := daemonSocketPath(stateHome, ServerRoleRemote, "")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, path, test.ShouldEqual, "/home/user/.local/state/graft/remote/graftd.sock")

	// With identity
	path, err = daemonSocketPath(stateHome, ServerRoleRemote, "bright-falcon-soar")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, path, test.ShouldEqual, "/home/user/.local/state/graft/remote/bright-falcon-soar/graftd.sock")
}

func TestWordListSizes(t *testing.T) {
	test.That(t, len(identityAdjectives) >= 50, test.ShouldBeTrue)
	test.That(t, len(identityNouns) >= 100, test.ShouldBeTrue)
	test.That(t, len(identityVerbs) >= 100, test.ShouldBeTrue)
}
