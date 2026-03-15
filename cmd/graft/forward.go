package main

import (
	"github.com/spf13/cobra"
)

var (
	forwardTo     string
	forwardPrefix bool
)

var forwardCmd = &cobra.Command{
	Use:   "forward [flags] <command> [commands...]",
	Short: "Forward local commands to a remote connection",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		toConn := forwardTo
		if toConn == "" {
			selectResp, err := client.SelectConnectionForCWD(ctx)
			if err != nil {
				return cliExit("--to required (no connection detected for current directory)", 1)
			}

			toConn = selectResp.GetConnectionName()
		}

		return client.ForwardCommands(ctx, args, toConn, forwardPrefix)
	},
}

var forwardListCmd = &cobra.Command{
	Use:   "list",
	Short: "List forwarded commands",
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.PrintShimmedCommands(ctx)
	},
}

var forwardWhichCmd = &cobra.Command{
	Use:   "which <command>",
	Short: "Show which connection a forwarded command uses",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.Which(ctx, args[0])
	},
}

func init() {
	forwardCmd.Flags().StringVarP(&forwardTo, "to", "t", "", "Target connection (detected from CWD if omitted)")
	forwardCmd.Flags().BoolVar(&forwardPrefix, "prefix", false, "Forward with connection name prefix")

	forwardCmd.AddCommand(forwardListCmd)
	forwardCmd.AddCommand(forwardWhichCmd)

	rootCmd.AddCommand(forwardCmd)
}
