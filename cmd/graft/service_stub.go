//go:build !darwin && !linux

package main

import (
	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
)

var daemonServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage the graft daemon as a system service",
	RunE: func(_ *cobra.Command, _ []string) error {
		return errors.New("service management is only supported on macOS and Linux")
	},
}

func init() {
	daemonCmd.AddCommand(daemonServiceCmd)
}
