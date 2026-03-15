//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
	graft "github.com/edaniels/graft/pkg"
	"github.com/edaniels/graft/pkg/embedded"
)

var daemonServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage the graft daemon as a system service",
}

var daemonServiceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and start the graft daemon service",
	RunE: func(_ *cobra.Command, _ []string) error {
		mgr, err := graft.NewServiceManager()
		if err != nil {
			return errors.Wrap(err)
		}

		binaryPath, err := os.Executable()
		if err != nil {
			return errors.Wrap(err)
		}

		binaryPath, err = filepath.EvalSymlinks(binaryPath)
		if err != nil {
			return errors.Wrap(err)
		}

		if !embedded.HasEmbeddedBinaries() && !graft.VersionIsInSourceTree() {
			return errors.New(
				"cannot install as a service: this binary has no embedded remote daemon binaries and is not running from a source tree.\n" +
					"Install graft using the official release or build with 'just build-all' before installing the service.",
			)
		}

		// Refuse to install if a daemon is already running.
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			client, _, clientErr := graft.NewLocalClient(
				ctx,
				os.Stdout,
				os.Stderr,
				func(err error) error { return err },
				false,
				logger,
			)
			if clientErr != nil {
				return
			}

			client.Close()

			err = errors.New(
				"daemon is already running; stop it first with 'graft daemon stop' before installing the service",
			)
		}()

		if err != nil {
			return err
		}

		if err := mgr.Install(binaryPath); err != nil {
			return errors.Wrap(err)
		}

		fmt.Fprintf(os.Stderr, "Daemon service installed and started.\n")
		fmt.Fprintf(os.Stderr, "The daemon will start automatically on login.\n")

		return nil
	},
}

var daemonServiceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop and remove the graft daemon service",
	RunE: func(_ *cobra.Command, _ []string) error {
		mgr, err := graft.NewServiceManager()
		if err != nil {
			return errors.Wrap(err)
		}

		if err := mgr.Uninstall(); err != nil {
			return errors.Wrap(err)
		}

		fmt.Fprintf(os.Stderr, "Daemon service uninstalled.\n")

		return nil
	},
}

var daemonServiceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon service status",
	RunE: func(_ *cobra.Command, _ []string) error {
		mgr, err := graft.NewServiceManager()
		if err != nil {
			return errors.Wrap(err)
		}

		status, err := mgr.Status()
		if err != nil {
			return errors.Wrap(err)
		}

		fmt.Fprintf(os.Stderr, "Service:   %s\n", status.Label)
		fmt.Fprintf(os.Stderr, "Installed: %s\n", boolToYesNo(status.Installed))
		fmt.Fprintf(os.Stderr, "Loaded:    %s\n", boolToYesNo(status.Loaded))
		fmt.Fprintf(os.Stderr, "Running:   %s\n", boolToYesNo(status.Running))

		if status.PID > 0 {
			fmt.Fprintf(os.Stderr, "PID:       %d\n", status.PID)
		}

		if status.BinaryPath != "" {
			fmt.Fprintf(os.Stderr, "Binary:    %s\n", status.BinaryPath)

			currentBinary, binErr := os.Executable()
			if binErr == nil {
				currentBinary, binErr = filepath.EvalSymlinks(currentBinary)
				if binErr == nil && currentBinary != status.BinaryPath {
					fmt.Fprintf(os.Stderr, "Warning:   service binary differs from current executable (%s)\n", currentBinary)
				}
			}
		}

		return nil
	},
}

var daemonServiceStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon service",
	RunE: func(_ *cobra.Command, _ []string) error {
		mgr, err := graft.NewServiceManager()
		if err != nil {
			return errors.Wrap(err)
		}

		if err := mgr.Start(); err != nil {
			return errors.Wrap(err)
		}

		fmt.Fprintf(os.Stderr, "Daemon service started.\n")

		return nil
	},
}

var daemonServiceStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the daemon service",
	RunE: func(_ *cobra.Command, _ []string) error {
		mgr, err := graft.NewServiceManager()
		if err != nil {
			return errors.Wrap(err)
		}

		if err := mgr.Stop(); err != nil {
			return errors.Wrap(err)
		}

		fmt.Fprintf(os.Stderr, "Daemon service stopped.\n")

		return nil
	},
}

func boolToYesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}

func init() {
	daemonServiceCmd.AddCommand(daemonServiceInstallCmd)
	daemonServiceCmd.AddCommand(daemonServiceUninstallCmd)
	daemonServiceCmd.AddCommand(daemonServiceStatusCmd)
	daemonServiceCmd.AddCommand(daemonServiceStartCmd)
	daemonServiceCmd.AddCommand(daemonServiceStopCmd)

	daemonCmd.AddCommand(daemonServiceCmd)
}
