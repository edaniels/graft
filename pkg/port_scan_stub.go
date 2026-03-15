//go:build !linux

package graft

// GetPortsForParent is not supported on non-Linux platforms because it requires /proc.
func GetPortsForParent(_ int) ([]ListeningPort, error) {
	return []ListeningPort{}, nil
}
