package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
	graft "github.com/edaniels/graft/pkg"
)

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Print Graft environment information",
	RunE: func(_ *cobra.Command, _ []string) error {
		w := os.Stdout

		dirs, err := graft.ResolvedGraftDirs()
		if err != nil {
			return errors.Wrap(err)
		}

		fmt.Fprintf(w, "GRAFT_CONFIG_HOME=%q\n", dirs.Config)
		fmt.Fprintf(w, "GRAFT_STATE_HOME=%q\n", dirs.State)

		return nil
	},
}

func init() {
	rootCmd.AddCommand(envCmd)
}
