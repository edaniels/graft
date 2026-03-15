package main

import (
	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
)

var (
	syncTo      string
	syncDestDir string
)

var syncCmd = &cobra.Command{
	Use:   "sync [flags] <source>",
	Short: "Sync files to a remote connection",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.Sync(ctx, args[0], syncDestDir, syncTo)
	},
}

func init() {
	syncCmd.Flags().StringVarP(&syncTo, "to", "t", "", "Target connection")
	errors.Unchecked(syncCmd.MarkFlagRequired("to"))
	syncCmd.Flags().StringVar(&syncDestDir, "dest-dir", "", "Destination directory")

	rootCmd.AddCommand(syncCmd)
}
