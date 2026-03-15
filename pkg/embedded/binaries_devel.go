//go:build !embed_binaries

package embedded

// HasEmbeddedBinaries reports whether this build has embedded remote daemon binaries.
func HasEmbeddedBinaries() bool {
	return false
}

// GetBinary returns NotEmbeddedError in development builds, signaling that the binary
// should be compiled locally instead of extracted from embedded storage.
func GetBinary(binaryName string) ([]byte, error) {
	return nil, NotEmbeddedError{BinaryName: binaryName}
}
