//go:build embed_binaries

package embedded

import (
	"bytes"
	"embed"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/edaniels/graft/errors"
)

// HasEmbeddedBinaries reports whether this build has embedded remote daemon binaries.
func HasEmbeddedBinaries() bool {
	return true
}

//go:embed binaries/*.zst
var embeddedBinaries embed.FS

// GetBinary returns the decompressed embedded binary for the given name (e.g. "graft-linux-amd64").
// Returns NotEmbeddedError if the requested binary wasn't embedded.
func GetBinary(binaryName string) ([]byte, error) {
	filename := fmt.Sprintf("binaries/%s.zst", binaryName)

	compressed, err := embeddedBinaries.ReadFile(filename)
	if err != nil {
		return nil, NotEmbeddedError{BinaryName: binaryName}
	}

	decoder, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, errors.WrapPrefix(err, "error creating zstd decoder")
	}
	defer decoder.Close()

	data, err := io.ReadAll(decoder)
	if err != nil {
		return nil, errors.WrapPrefix(err, fmt.Sprintf("error decompressing %s", binaryName))
	}

	return data, nil
}
