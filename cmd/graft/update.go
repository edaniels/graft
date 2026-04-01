package main

import (
	"context"
	"fmt"
	"os"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	graft "github.com/edaniels/graft/pkg"
)

var updateCheck bool

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update graft to the latest version",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runUpdate(cmd.Context())
	},
}

func init() {
	updateCmd.Flags().BoolVar(&updateCheck, "check", false, "Only check if an update is available")

	rootCmd.AddCommand(updateCmd)
}

func runUpdate(ctx context.Context) error {
	lock, err := graft.AcquireUpdateLock()
	if err != nil {
		return cliExit(err, 1)
	}
	defer lock.Close()

	client := graft.ReleaseClientFromConfig()

	currentVersion := graft.VersionString()
	fmt.Fprintf(os.Stderr, "Current version: %s\n", currentVersion)
	fmt.Fprintf(os.Stderr, "Checking for updates...\n")

	result, err := graft.CheckForUpdate(ctx, client, currentVersion)
	if err != nil {
		return cliExit(err, 1)
	}

	if !result.UpdateAvailable {
		if result.IsDevBuild {
			fmt.Fprintf(os.Stderr, "Dev build (%s) cannot be updated. Install a release version first.\n", currentVersion)
		} else {
			fmt.Fprintf(os.Stderr, "Already up to date (%s).\n", result.LatestVersion)
		}

		return nil
	}

	fmt.Fprintf(os.Stderr, "New version available: %s\n", color.GreenString(result.LatestVersion))

	if updateCheck {
		fmt.Fprintf(os.Stderr, "Run 'graft update' to install it.\n")

		return nil
	}

	fmt.Fprintf(os.Stderr, "Downloading %s...\n", result.LatestVersion)

	tmpPath, targetPath, err := graft.DownloadAndVerify(ctx, client, result.LatestVersion)
	if err != nil {
		return cliExit(err, 1)
	}

	fmt.Fprintf(os.Stderr, "Checksum verified.\n")
	fmt.Fprintf(os.Stderr, "Replacing binary...\n")

	if err := graft.ReplaceBinary(tmpPath, targetPath); err != nil {
		os.Remove(tmpPath)

		return cliExit(err, 1)
	}

	fmt.Fprintf(os.Stderr, "Updated: %s → %s\n", currentVersion, color.GreenString(result.LatestVersion))

	restartDaemonAfterUpdate(ctx)

	return nil
}

func restartDaemonAfterUpdate(ctx context.Context) {
	sockPath, err := graft.DaemonSocketPathForCurrentHost(graft.ServerRoleLocal)
	if err != nil {
		return
	}

	if _, statErr := os.Stat(sockPath); statErr != nil {
		fmt.Fprintf(os.Stderr, "No daemon running.\n")

		return
	}

	fmt.Fprintf(os.Stderr, "Restarting daemon...\n")

	client, rpcCtx := newClient(ctx, false)
	defer client.Close()

	if restartErr := client.Restart(rpcCtx); restartErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to restart daemon: %s\n", restartErr)

		return
	}

	fmt.Fprintf(os.Stderr, "Daemon restarted.\n")
}
