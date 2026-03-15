package main

import (
	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
)

var reportCwdPID uint64

var reportCwdCmd = &cobra.Command{
	Use:    "report-cwd [flags] <cwd>",
	Short:  "Report current working directory (shell hook plumbing)",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.ReportCWD(ctx, reportCwdPID, args[0])
	},
}

var (
	runShimmedPID  uint64
	runShimmedCwd  string
	runShimmedCmd  string
	runShimmedSudo bool
)

var runShimmedCmdCmd = &cobra.Command{
	Use:    "run-shimmed-cmd [flags] [args...]",
	Short:  "Run a shimmed command (shim script plumbing)",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), false)
		defer client.Close()

		exitCode, err := client.RunShimmedCommand(
			ctx,
			runShimmedCmd,
			args,
			runShimmedPID,
			runShimmedCwd,
			runShimmedSudo,
		)
		if err != nil {
			return cliExit(err, 1)
		}

		return cliExit("", exitCode)
	},
}

func init() {
	reportCwdCmd.Flags().Uint64Var(&reportCwdPID, "pid", 0, "Session PID")
	errors.Unchecked(reportCwdCmd.MarkFlagRequired("pid"))

	runShimmedCmdCmd.Flags().Uint64Var(&runShimmedPID, "pid", 0, "Session PID")
	errors.Unchecked(runShimmedCmdCmd.MarkFlagRequired("pid"))
	runShimmedCmdCmd.Flags().StringVar(&runShimmedCwd, "cwd", "", "Current working directory")
	errors.Unchecked(runShimmedCmdCmd.MarkFlagRequired("cwd"))
	runShimmedCmdCmd.Flags().StringVar(&runShimmedCmd, "cmd", "", "Command name")
	errors.Unchecked(runShimmedCmdCmd.MarkFlagRequired("cmd"))
	runShimmedCmdCmd.Flags().BoolVar(&runShimmedSudo, "sudo", false, "Run as sudo")

	rootCmd.AddCommand(reportCwdCmd)
	rootCmd.AddCommand(runShimmedCmdCmd)
}
