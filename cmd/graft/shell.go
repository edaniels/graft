package main

import "github.com/spf13/cobra"

var shellTo string

var shellCmd = &cobra.Command{
	Use:   "shell",
	Short: "Open a shell on a remote connection",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, false)
		defer client.Close()

		exitCode, err := client.RemoteShell(ctx, shellTo)
		if err != nil {
			return cliExit(cmd, args, err, 1)
		}

		return cliExit(cmd, args, "", exitCode)
	},
}

func init() {
	shellCmd.Flags().StringVarP(&shellTo, "to", "t", "", "Target connection")
	shellCmd.RegisterFlagCompletionFunc("to", completeConnectionNames) //nolint:errcheck

	rootCmd.AddCommand(shellCmd)
}
