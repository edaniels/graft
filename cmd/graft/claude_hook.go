package main

import (
	"os"

	"github.com/spf13/cobra"

	graft "github.com/edaniels/graft/pkg"
)

// claudeSessionStartCmd is invoked from a Claude Code SessionStart hook
// (see docs/claude-code.md). It is silent (exits 0, prints nothing) when
// the daemon isn't running or the current directory has no graft
// connection, since most Claude Code sessions have nothing to do with
// graft.
var claudeSessionStartCmd = &cobra.Command{
	Use:    "claude-session-start",
	Short:  "Print a Claude Code SessionStart hook payload for this directory (Claude Code hook plumbing)",
	Hidden: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx, err := graft.NewLocalClient(cmd.Context(), os.Stdout, os.Stderr, nil, false, logger)
		if err != nil {
			return nil
		}
		defer client.Close()

		return client.PrintClaudeSessionStartHook(ctx)
	},
}

func init() {
	rootCmd.AddCommand(claudeSessionStartCmd)
}
