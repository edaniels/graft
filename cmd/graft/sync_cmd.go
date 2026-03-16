package main

import (
	"github.com/spf13/cobra"
)

var (
	syncTo      string
	syncDestDir string
)

var syncCmd = &cobra.Command{
	Use:   "sync [flags] [source]",
	Short: "Sync files to a remote connection",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		toConn := syncTo
		if toConn == "" {
			selectResp, err := client.SelectConnectionForCWD(ctx)
			if err != nil {
				return cliExit("--to required (no connection detected for current directory)", 1)
			}

			toConn = selectResp.GetConnectionName()
		}

		return client.Sync(ctx, parseSyncArgs(args), syncDestDir, toConn)
	},
}

func parseSyncArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}

	return args[0]
}

func init() {
	syncCmd.Flags().StringVarP(&syncTo, "to", "t", "", "Target connection (detected from CWD if omitted)")
	syncCmd.Flags().StringVar(&syncDestDir, "dest-dir", "", "Destination directory")

	rootCmd.AddCommand(syncCmd)
}
