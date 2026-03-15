package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
)

var connectionCmd = &cobra.Command{
	Use:   "connection",
	Short: "Manage connections",
}

var connectionSetRootCmd = &cobra.Command{
	Use:   "set-root <connection> <local_dir> [remote_dir]",
	Short: "Set the local (and optionally remote) root directory for a connection",
	Args:  cobra.RangeArgs(2, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		connName := args[0]
		localDir := args[1]

		absDir, err := filepath.Abs(localDir)
		if err != nil {
			return errors.Wrap(err)
		}

		if _, statErr := os.Stat(absDir); statErr != nil {
			return errors.Errorf("local directory does not exist: %s", absDir)
		}

		var remoteDir string
		if len(args) == 3 {
			remoteDir = args[2]
		}

		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		if setErr := client.SetConnectionRoots(ctx, connName, absDir, remoteDir); setErr != nil {
			return errors.Wrap(setErr)
		}

		fmt.Fprintf(cmd.ErrOrStderr(), "updated %s\n", connName)
		fmt.Fprintf(cmd.ErrOrStderr(), "  local root:   %s\n", absDir)

		if remoteDir != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "  remote root:  %s\n", remoteDir)
		}

		return nil
	},
}

func init() {
	connectionCmd.AddCommand(connectionSetRootCmd)
	rootCmd.AddCommand(connectionCmd)
}
