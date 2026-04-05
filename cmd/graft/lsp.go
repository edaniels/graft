package main

import (
	"os"

	"github.com/spf13/cobra"

	graft "github.com/edaniels/graft/pkg"
)

var lspCmd = &cobra.Command{
	Use:   "lsp <remote-executable>",
	Short: "Serve LSP proxy to a remote language server",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx, err := graft.NewLocalClient(
			cmd.Context(),
			os.Stdout,
			os.Stderr,
			func(err error) error {
				return cliExit(err, 1)
			},
			false,
			logger,
		)
		if err != nil {
			return graft.ExecLocalLSP(args[0])
		}
		defer client.Close()

		return client.ServeLSP(ctx, args[0])
	},
}

func init() {
	rootCmd.AddCommand(lspCmd)
}
