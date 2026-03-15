package graft

import "github.com/edaniels/graft/errors"

// Name is simply the name of graft.
const Name = "graft"

// ErrShuttingDown should be used to signal any shutdown message.
var ErrShuttingDown = errors.NewBare("shutting down")

const (
	osDarwin = "darwin"
	osLinux  = "linux"
)
