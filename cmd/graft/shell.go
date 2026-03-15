package main

import "github.com/spf13/cobra"

var shellTo string

var shellCmd = &cobra.Command{
	Use:   "shell",
	Short: "Open a shell on a remote connection",
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx := newClient(cmd.Context(), false)
		defer client.Close()

		exitCode, err := client.RemoteShell(ctx, shellTo)
		if err != nil {
			return cliExit(err, 1)
		}

		return cliExit("", exitCode)
	},
}

func init() {
	shellCmd.Flags().StringVarP(&shellTo, "to", "t", "", "Target connection")

	rootCmd.AddCommand(shellCmd)
}
