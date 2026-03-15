//go:build linux

package main

import (
	"fmt"
	"os"

	graft "github.com/edaniels/graft/pkg"
)

// warnIfServiceManaged checks if the daemon is managed by systemd and prints
// a warning if so. Returns true if the user should use service commands instead.
func warnIfServiceManaged() bool {
	mgr, mgrErr := graft.NewServiceManager()
	if mgrErr != nil {
		return false
	}

	status, statusErr := mgr.Status()
	if statusErr != nil || !status.Installed || !status.Loaded {
		return false
	}

	fmt.Fprintln(os.Stderr, "The daemon is managed by systemd. Use 'graft daemon service stop' to stop it,")
	fmt.Fprintln(os.Stderr, "or 'graft daemon service uninstall' to remove the service entirely.")

	return true
}
