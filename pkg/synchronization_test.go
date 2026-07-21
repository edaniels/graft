package graft

import (
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"go.viam.com/test"
)

func TestParseSyncMode(t *testing.T) {
	t.Run("accepted spellings of the same mode", func(t *testing.T) {
		for _, s := range []string{"644", "0644", "0o644"} {
			mode, err := parseSyncMode(s)
			test.That(t, err, test.ShouldBeNil)
			test.That(t, mode, test.ShouldEqual, filesystem.Mode(0o644))
		}
	})

	t.Run("rejects invalid values", func(t *testing.T) {
		for _, s := range []string{"", "abc", "999", "-644", "0x644"} {
			_, err := parseSyncMode(s)
			test.That(t, err, test.ShouldNotBeNil)
		}
	})
}

func TestValidateSyncModes(t *testing.T) {
	t.Run("empty modes are valid (defaults apply)", func(t *testing.T) {
		test.That(t, validateSyncModes("", ""), test.ShouldBeNil)
	})

	t.Run("typical loosened and tightened modes are valid", func(t *testing.T) {
		test.That(t, validateSyncModes("644", "755"), test.ShouldBeNil)
		test.That(t, validateSyncModes("600", "700"), test.ShouldBeNil)
		test.That(t, validateSyncModes("640", "750"), test.ShouldBeNil)
	})

	t.Run("file modes with executability bits are rejected", func(t *testing.T) {
		// Mutagen manages the executable bit itself (propagated from the
		// source), so a file mode carrying exec bits is a misconfiguration.
		test.That(t, validateSyncModes("755", "755"), test.ShouldNotBeNil)
	})

	t.Run("non-permission bits are rejected", func(t *testing.T) {
		test.That(t, validateSyncModes("644", "1777"), test.ShouldNotBeNil)
	})

	t.Run("unparseable modes are rejected", func(t *testing.T) {
		test.That(t, validateSyncModes("banana", "755"), test.ShouldNotBeNil)
		test.That(t, validateSyncModes("644", "888"), test.ShouldNotBeNil)
	})
}

func TestSyncModesCompatible(t *testing.T) {
	t.Run("empty desired modes match any existing", func(t *testing.T) {
		test.That(t, syncModesCompatible("640", "750", "", ""), test.ShouldBeTrue)
	})

	t.Run("explicit equal modes match", func(t *testing.T) {
		test.That(t, syncModesCompatible("640", "750", "640", "750"), test.ShouldBeTrue)
	})

	t.Run("explicit differing modes do not match", func(t *testing.T) {
		test.That(t, syncModesCompatible("644", "755", "640", "755"), test.ShouldBeFalse)
		test.That(t, syncModesCompatible("644", "755", "644", "750"), test.ShouldBeFalse)
	})
}

func TestSyncBetaConfiguration(t *testing.T) {
	t.Run("defaults to world-readable working tree modes", func(t *testing.T) {
		cfg, err := syncBetaConfiguration(SynchronizationIntent{})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, cfg.GetDefaultFileMode(), test.ShouldEqual, uint32(0o644))
		test.That(t, cfg.GetDefaultDirectoryMode(), test.ShouldEqual, uint32(0o755))
	})

	t.Run("explicit intent modes win", func(t *testing.T) {
		cfg, err := syncBetaConfiguration(SynchronizationIntent{
			DefaultFileMode:      "640",
			DefaultDirectoryMode: "750",
		})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, cfg.GetDefaultFileMode(), test.ShouldEqual, uint32(0o640))
		test.That(t, cfg.GetDefaultDirectoryMode(), test.ShouldEqual, uint32(0o750))
	})

	t.Run("invalid intent modes error", func(t *testing.T) {
		_, err := syncBetaConfiguration(SynchronizationIntent{DefaultFileMode: "banana"})
		test.That(t, err, test.ShouldNotBeNil)
	})
}

func TestGitReplicaBetaConfiguration(t *testing.T) {
	t.Run("defaults to unset modes (mutagen private 0600/0700)", func(t *testing.T) {
		cfg, err := gitReplicaBetaConfiguration(SynchronizationIntent{})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, cfg.GetDefaultFileMode(), test.ShouldEqual, uint32(0))
		test.That(t, cfg.GetDefaultDirectoryMode(), test.ShouldEqual, uint32(0))
	})

	t.Run("explicit intent modes apply to the replica too", func(t *testing.T) {
		cfg, err := gitReplicaBetaConfiguration(SynchronizationIntent{
			DefaultFileMode:      "644",
			DefaultDirectoryMode: "755",
		})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, cfg.GetDefaultFileMode(), test.ShouldEqual, uint32(0o644))
		test.That(t, cfg.GetDefaultDirectoryMode(), test.ShouldEqual, uint32(0o755))
	})
}

func TestMakeSyncRootCommand(t *testing.T) {
	t.Run("pins roots graft creates to the directory mode in octal", func(t *testing.T) {
		test.That(t, makeSyncRootCommand("/home/u/proj", 0o755),
			test.ShouldEqual, "test -d /home/u/proj || (mkdir -p /home/u/proj && chmod 755 /home/u/proj)")
		test.That(t, makeSyncRootCommand("/home/u/proj/.git", 0o700),
			test.ShouldEqual, "test -d /home/u/proj/.git || (mkdir -p /home/u/proj/.git && chmod 700 /home/u/proj/.git)")
	})
}

func TestActiveSyncInheritModesInto(t *testing.T) {
	t.Run("empty intent modes inherit the active sync's modes", func(t *testing.T) {
		existing := activeSync{defaultFileMode: "640", defaultDirectoryMode: "750"}
		intent := SynchronizationIntent{FromLocal: "/a", ToRemote: "/A"}

		existing.inheritModesInto(&intent)

		test.That(t, intent.DefaultFileMode, test.ShouldEqual, "640")
		test.That(t, intent.DefaultDirectoryMode, test.ShouldEqual, "750")
	})

	t.Run("explicit intent modes are kept", func(t *testing.T) {
		existing := activeSync{defaultFileMode: "640", defaultDirectoryMode: "750"}
		intent := SynchronizationIntent{DefaultFileMode: "600", DefaultDirectoryMode: "700"}

		existing.inheritModesInto(&intent)

		test.That(t, intent.DefaultFileMode, test.ShouldEqual, "600")
		test.That(t, intent.DefaultDirectoryMode, test.ShouldEqual, "700")
	})
}

func TestSyncSessionConfigStale(t *testing.T) {
	desiredMain := &synchronization.Configuration{Ignores: []string{"a", "b"}}
	desiredBeta := &synchronization.Configuration{
		DefaultFileMode:      0o644,
		DefaultDirectoryMode: 0o755,
	}

	t.Run("matching config is not stale", func(t *testing.T) {
		existingMain := &synchronization.Configuration{Ignores: []string{"a", "b"}}
		existingBeta := &synchronization.Configuration{
			DefaultFileMode:      0o644,
			DefaultDirectoryMode: 0o755,
		}

		test.That(t,
			syncSessionConfigStale(existingMain, existingBeta, desiredMain, desiredBeta),
			test.ShouldBeFalse)
	})

	t.Run("differing ignores are stale", func(t *testing.T) {
		existingMain := &synchronization.Configuration{Ignores: []string{"a"}}
		existingBeta := &synchronization.Configuration{
			DefaultFileMode:      0o644,
			DefaultDirectoryMode: 0o755,
		}

		test.That(t,
			syncSessionConfigStale(existingMain, existingBeta, desiredMain, desiredBeta),
			test.ShouldBeTrue)
	})

	t.Run("differing beta file mode is stale", func(t *testing.T) {
		existingMain := &synchronization.Configuration{Ignores: []string{"a", "b"}}
		existingBeta := &synchronization.Configuration{
			DefaultFileMode:      0o600,
			DefaultDirectoryMode: 0o755,
		}

		test.That(t,
			syncSessionConfigStale(existingMain, existingBeta, desiredMain, desiredBeta),
			test.ShouldBeTrue)
	})

	t.Run("differing beta directory mode is stale", func(t *testing.T) {
		existingMain := &synchronization.Configuration{Ignores: []string{"a", "b"}}
		existingBeta := &synchronization.Configuration{
			DefaultFileMode:      0o644,
			DefaultDirectoryMode: 0o700,
		}

		test.That(t,
			syncSessionConfigStale(existingMain, existingBeta, desiredMain, desiredBeta),
			test.ShouldBeTrue)
	})

	t.Run("pre-permissions-fix session is stale", func(t *testing.T) {
		// Sessions created before graft set beta endpoint modes recorded a
		// nil/empty beta configuration; they must be recreated so new remote
		// files stop landing 0600.
		existingMain := &synchronization.Configuration{Ignores: []string{"a", "b"}}

		test.That(t,
			syncSessionConfigStale(existingMain, nil, desiredMain, desiredBeta),
			test.ShouldBeTrue)
	})

	t.Run("replica sessions with unset modes on both sides are not stale", func(t *testing.T) {
		existingMain := &synchronization.Configuration{Ignores: []string{"a", "b"}}

		test.That(t,
			syncSessionConfigStale(existingMain, nil, desiredMain, &synchronization.Configuration{}),
			test.ShouldBeFalse)
	})
}

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
