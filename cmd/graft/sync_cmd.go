package main

import (
	"github.com/spf13/cobra"

	graft "github.com/edaniels/graft/pkg"
)

var (
	syncTo      string
	syncDestDir string
	syncGit     bool
)

var syncCmd = &cobra.Command{
	Use:   "sync [flags] [source]",
	Short: "Sync files to a remote connection",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, true)
		defer client.Close()

		toConn := syncTo
		if toConn == "" {
			selectResp, err := client.SelectConnectionForCWD(ctx)
			if err != nil {
				return cliExit(cmd, args, "--to required (no connection detected for current directory)", 1)
			}

			toConn = selectResp.GetConnectionName()
		}

		return client.Sync(ctx, graft.SyncParams{
			SourceDir:        parseSyncArgs(args),
			DestDir:          syncDestDir,
			ToConnectionName: toConn,
			SyncGit:          syncGit,
		})
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
	syncCmd.RegisterFlagCompletionFunc("to", completeConnectionNames) //nolint:errcheck
	syncCmd.Flags().StringVar(&syncDestDir, "dest-dir", "", "Destination directory")
	syncCmd.Flags().BoolVar(&syncGit, "git", false, "Also replicate the source's .git one-way (remote git is read-only)")

	rootCmd.AddCommand(syncCmd)
}
