package graft

import (
	"testing"

	"go.viam.com/test"
)

func TestComputeMissingSyncs(t *testing.T) {
	t.Run("nothing desired returns nothing", func(t *testing.T) {
		got := computeMissingSyncs(nil, nil)
		test.That(t, got, test.ShouldBeEmpty)
	})

	t.Run("all desired and none active returns all", func(t *testing.T) {
		desired := []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A"},
			{FromLocal: "/b", ToRemote: "/B"},
		}

		got := computeMissingSyncs(desired, nil)
		test.That(t, len(got), test.ShouldEqual, 2)
		test.That(t, got[0].FromLocal, test.ShouldEqual, "/a")
		test.That(t, got[0].ToRemote, test.ShouldEqual, "/A")
		test.That(t, got[1].FromLocal, test.ShouldEqual, "/b")
		test.That(t, got[1].ToRemote, test.ShouldEqual, "/B")
	})

	t.Run("active exact match is skipped", func(t *testing.T) {
		desired := []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A"},
			{FromLocal: "/b", ToRemote: "/B"},
		}
		active := []SynchronizationIntent{
			{FromLocal: "/a", ToRemote: "/A"},
		}

		got := computeMissingSyncs(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0].FromLocal, test.ShouldEqual, "/b")
	})

	t.Run("active with different destination is treated as missing", func(t *testing.T) {
		desired := []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A"},
		}
		active := []SynchronizationIntent{
			{FromLocal: "/a", ToRemote: "/different"},
		}

		got := computeMissingSyncs(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0].FromLocal, test.ShouldEqual, "/a")
		test.That(t, got[0].ToRemote, test.ShouldEqual, "/A")
	})
}

func TestComputeMissingForwardCommands(t *testing.T) {
	t.Run("nothing desired returns nothing", func(t *testing.T) {
		got := computeMissingForwardCommands(nil, nil)
		test.That(t, got, test.ShouldBeEmpty)
	})

	t.Run("all desired returns all when none active", func(t *testing.T) {
		desired := []ForwardCommandIntent{
			{Name: "go", Prefix: false},
			{Name: "python", Prefix: true},
		}

		got := computeMissingForwardCommands(desired, nil)
		test.That(t, len(got), test.ShouldEqual, 2)
	})

	t.Run("exact match in active is skipped", func(t *testing.T) {
		desired := []ForwardCommandIntent{
			{Name: "go", Prefix: false},
			{Name: "python", Prefix: false},
		}
		active := []ForwardCommandIntent{{Name: "go", Prefix: false}}

		got := computeMissingForwardCommands(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0].Name, test.ShouldEqual, "python")
	})

	t.Run("different Prefix flag is treated as missing", func(t *testing.T) {
		desired := []ForwardCommandIntent{{Name: "go", Prefix: true}}
		active := []ForwardCommandIntent{{Name: "go", Prefix: false}}

		got := computeMissingForwardCommands(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0].Name, test.ShouldEqual, "go")
		test.That(t, got[0].Prefix, test.ShouldBeTrue)
	})
}
