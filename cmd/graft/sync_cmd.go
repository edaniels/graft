package main

import (
	"github.com/spf13/cobra"

	graft "github.com/edaniels/graft/pkg"
)

var (
	syncTo      string
	syncDestDir string
	syncGit     bool
	syncInclude []string
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
			SyncInclude:      syncInclude,
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
	// StringArray (not StringSlice) so brace patterns like '**/*.{pb.go,pb2.py}'
	// are not split on their commas.
	syncCmd.Flags().StringArrayVar(&syncInclude, "include-ignored", nil,
		"Gitignore-style pattern synced bidirectionally even though .gitignore excludes it (repeatable, e.g. '**/*_pb2.py')")

	rootCmd.AddCommand(syncCmd)
}
