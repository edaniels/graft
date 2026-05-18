package graft

import (
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/selection"
	"go.viam.com/test"
)

func TestSyncSessionName(t *testing.T) {
	t.Run("deterministic for same inputs", func(t *testing.T) {
		intent := SynchronizationIntent{FromLocal: "/home/u/src", ToRemote: "/remote/src"}

		name1 := syncSessionName("conn", intent)
		name2 := syncSessionName("conn", intent)

		test.That(t, name1, test.ShouldEqual, name2)
	})

	t.Run("differs when connection name changes", func(t *testing.T) {
		intent := SynchronizationIntent{FromLocal: "/a", ToRemote: "/b"}

		test.That(t, syncSessionName("c1", intent), test.ShouldNotEqual, syncSessionName("c2", intent))
	})

	t.Run("differs when FromLocal changes", func(t *testing.T) {
		test.That(t,
			syncSessionName("c", SynchronizationIntent{FromLocal: "/a", ToRemote: "/x"}),
			test.ShouldNotEqual,
			syncSessionName("c", SynchronizationIntent{FromLocal: "/b", ToRemote: "/x"}),
		)
	})

	t.Run("differs when ToRemote changes", func(t *testing.T) {
		test.That(t,
			syncSessionName("c", SynchronizationIntent{FromLocal: "/x", ToRemote: "/a"}),
			test.ShouldNotEqual,
			syncSessionName("c", SynchronizationIntent{FromLocal: "/x", ToRemote: "/b"}),
		)
	})

	t.Run("field separator prevents tuple collisions", func(t *testing.T) {
		// Without a separator, ("a", "b/c") and ("a/b", "c") would hash the
		// same. They must not.
		a := syncSessionName("a", SynchronizationIntent{FromLocal: "b/c", ToRemote: "x"})
		b := syncSessionName("a/b", SynchronizationIntent{FromLocal: "c", ToRemote: "x"})

		test.That(t, a, test.ShouldNotEqual, b)
	})

	t.Run("normalizes equivalent paths", func(t *testing.T) {
		// filepath.Clean folds trailing slashes and "." segments; different
		// spellings of the same path must yield the same name.
		canonical := syncSessionName("c", SynchronizationIntent{FromLocal: "/a/b", ToRemote: "/x"})
		trailing := syncSessionName("c", SynchronizationIntent{FromLocal: "/a/b/", ToRemote: "/x"})
		dotted := syncSessionName("c", SynchronizationIntent{FromLocal: "/a/./b", ToRemote: "/x"})

		test.That(t, trailing, test.ShouldEqual, canonical)
		test.That(t, dotted, test.ShouldEqual, canonical)
	})

	t.Run("name passes mutagen validation", func(t *testing.T) {
		// Names that fail EnsureNameValid crash session creation. Cover paths
		// with characters mutagen forbids (/, :, ., ~).
		intents := []SynchronizationIntent{
			{FromLocal: "/home/u/src", ToRemote: "/remote/src"},
			{FromLocal: "~/projects/foo.bar", ToRemote: "/tmp/x:y"},
		}

		for _, intent := range intents {
			name := syncSessionName("my-conn", intent)
			test.That(t, selection.EnsureNameValid(name), test.ShouldBeNil)
			test.That(t, strings.HasPrefix(name, graftSyncNamePrefix), test.ShouldBeTrue)
		}
	})
}

func TestIsGraftSyncName(t *testing.T) {
	t.Run("true for graft-prefixed names", func(t *testing.T) {
		intent := SynchronizationIntent{FromLocal: "/a", ToRemote: "/b"}
		test.That(t, isGraftSyncName(syncSessionName("conn", intent)), test.ShouldBeTrue)
	})

	t.Run("false for non-graft names", func(t *testing.T) {
		// Don't reap sessions a user created with the standalone mutagen CLI.
		test.That(t, isGraftSyncName("my-project"), test.ShouldBeFalse)
		test.That(t, isGraftSyncName(""), test.ShouldBeFalse)
	})
}
