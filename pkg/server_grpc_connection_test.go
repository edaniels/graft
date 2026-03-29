package graft

import (
	"path/filepath"
	"testing"

	"go.viam.com/test"
)

func TestDefaultSyncRemotePath(t *testing.T) {
	t.Run("basic structure", func(t *testing.T) {
		result := defaultSyncRemotePath("/home/ubuntu", "bright-falcon-soar", "/Users/alice/projects/foo")
		dir, base := filepath.Split(result)
		test.That(t, dir, test.ShouldEqual, "/home/ubuntu/.graft/sync/bright-falcon-soar/")
		test.That(t, base, test.ShouldStartWith, "foo-")
		test.That(t, len(base), test.ShouldEqual, len("foo-")+6)
	})

	t.Run("same basename different source dirs produce different paths", func(t *testing.T) {
		a := defaultSyncRemotePath("/home/ubuntu", "bright-falcon-soar", "/Users/alice/bar/foo")
		b := defaultSyncRemotePath("/home/ubuntu", "bright-falcon-soar", "/Users/alice/baz/foo")
		test.That(t, a, test.ShouldNotEqual, b)
	})

	t.Run("same source dir different identities produce different paths", func(t *testing.T) {
		a := defaultSyncRemotePath("/home/ubuntu", "bright-falcon-soar", "/Users/alice/foo")
		b := defaultSyncRemotePath("/home/ubuntu", "calm-ember-soar", "/Users/alice/foo")
		test.That(t, a, test.ShouldNotEqual, b)
	})

	t.Run("deterministic", func(t *testing.T) {
		a := defaultSyncRemotePath("/home/ubuntu", "bright-falcon-soar", "/Users/alice/foo")
		b := defaultSyncRemotePath("/home/ubuntu", "bright-falcon-soar", "/Users/alice/foo")
		test.That(t, a, test.ShouldEqual, b)
	})
}
