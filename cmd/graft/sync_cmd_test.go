package main

import (
	"testing"

	"go.viam.com/test"
)

func TestParseSyncArgs(t *testing.T) {
	t.Run("no args returns empty string", func(t *testing.T) {
		source := parseSyncArgs(nil)
		test.That(t, source, test.ShouldBeEmpty)
	})

	t.Run("one arg returns that arg", func(t *testing.T) {
		source := parseSyncArgs([]string{"/my/dir"})
		test.That(t, source, test.ShouldEqual, "/my/dir")
	})
}
