package main

import "github.com/spf13/cobra"

var lspCmd = &cobra.Command{
	Use:   "lsp <remote-executable>",
	Short: "Serve LSP proxy to a remote language server",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.ServeLSP(ctx, args[0])
	},
}

func init() {
	rootCmd.AddCommand(lspCmd)
}
