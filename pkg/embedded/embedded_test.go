package embedded

import (
	"errors"
	"testing"

	"go.viam.com/test"
)

func TestHasEmbeddedBinaries(t *testing.T) {
	// In test builds (no embed_binaries tag), embedded binaries are not available.
	test.That(t, HasEmbeddedBinaries(), test.ShouldBeFalse)
}

func TestGetBinaryReturnsNotEmbeddedInDevel(t *testing.T) {
	data, err := GetBinary("graft-linux-amd64")

	var notEmbedded NotEmbeddedError
	test.That(t, errors.As(err, &notEmbedded), test.ShouldBeTrue)
	test.That(t, notEmbedded.BinaryName, test.ShouldEqual, "graft-linux-amd64")
	test.That(t, data, test.ShouldBeNil)
}
