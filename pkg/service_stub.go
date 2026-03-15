//go:build !darwin && !linux

package graft

import "github.com/edaniels/graft/errors"

// NewServiceManager returns an error on non-macOS platforms.
func NewServiceManager() (ServiceManager, error) {
	return nil, errors.New("service management is only supported on macOS and Linux")
}
