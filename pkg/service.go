package graft

// ServiceManager manages a system-level service for the graft daemon.
type ServiceManager interface {
	Install(binaryPath string) error
	Uninstall() error
	Status() (ServiceStatus, error)
	Start() error
	Stop() error
}

// ServiceStatus describes the current state of the daemon service.
type ServiceStatus struct {
	Installed  bool
	Loaded     bool
	Running    bool
	PID        int
	BinaryPath string
	Label      string
}
