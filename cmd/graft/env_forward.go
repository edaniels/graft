package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// envForwardTo is the persistent --to/-t flag shared by `env forward` and its subcommands.
var envForwardTo string

var envForwardCmd = &cobra.Command{
	Use:   "forward [flags] <VAR|GLOB> [names...]",
	Short: "Forward local environment variables to a connection",
	Long: `Forward local environment variables to a connection.

Names are resolved from the invoking shell's live environment at graft run
time, never stored. Glob patterns are supported (e.g. "FOO_*").`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, true)
		defer client.Close()

		toConn := envForwardTo
		if toConn == "" {
			selectResp, err := client.SelectConnectionForCWD(ctx)
			if err != nil {
				return cliExit(cmd, args, "--to required (no connection detected for current directory)", 1)
			}

			toConn = selectResp.GetConnectionName()
		}

		return client.EnvForward(ctx, args, toConn)
	},
}

var envForwardListCmd = &cobra.Command{
	Use:   "list",
	Short: "List forwarded environment variable names/patterns",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, true)
		defer client.Close()

		toConn := envForwardTo
		if toConn == "" {
			selectResp, err := client.SelectConnectionForCWD(ctx)
			if err != nil {
				return cliExit(cmd, args, "--to required (no connection detected for current directory)", 1)
			}

			toConn = selectResp.GetConnectionName()
		}

		names, err := client.EnvForwardList(ctx, toConn)
		if err != nil {
			return err //nolint:wrapcheck
		}

		var buf strings.Builder
		for _, name := range names {
			fmt.Fprintf(&buf, "%s\n", name)
		}

		os.Stdout.WriteString(buf.String())

		return nil
	},
}

var envForwardRemoveCmd = &cobra.Command{
	Use:   "remove [flags] <VAR|GLOB> [names...]",
	Short: "Remove forwarded environment variable names/patterns from a connection",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, true)
		defer client.Close()

		toConn := envForwardTo
		if toConn == "" {
			selectResp, err := client.SelectConnectionForCWD(ctx)
			if err != nil {
				return cliExit(cmd, args, "--to required (no connection detected for current directory)", 1)
			}

			toConn = selectResp.GetConnectionName()
		}

		return client.RemoveEnvForward(ctx, args, toConn)
	},
}

func init() {
	envForwardCmd.PersistentFlags().StringVarP(&envForwardTo, "to", "t", "", "Target connection (detected from CWD if omitted)")
	envForwardCmd.RegisterFlagCompletionFunc("to", completeConnectionNames) //nolint:errcheck

	envForwardCmd.AddCommand(envForwardListCmd)
	envForwardCmd.AddCommand(envForwardRemoveCmd)

	envCmd.AddCommand(envForwardCmd)
}
