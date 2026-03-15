// Package embedded provides access to embedded daemon binaries for cross-platform deployment.
package embedded

// NotEmbeddedError is returned when requesting a binary that wasn't embedded.
type NotEmbeddedError struct {
	BinaryName string
}

func (e NotEmbeddedError) Error() string {
	return "no embedded binary: " + e.BinaryName
}
