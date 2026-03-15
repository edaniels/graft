package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
)

var (
	errCommandRequired = errors.NewBare("command required")
	errFlagRequiresVal = errors.NewBare("requires a value")
)

var runCmd = &cobra.Command{
	Use:                "run [-t <connection>] <command> [args...]",
	Short:              "Run a command on a remote connection",
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		to, cmdArgs, helpRequested, err := parseRunArgs(args)
		if helpRequested {
			return cmd.Help()
		}
		if err != nil {
			return cliExit(err, 1)
		}

		client, ctx := newClient(cmd.Context(), false)
		defer client.Close()

		exitCode, err := client.RunCommand(ctx, cmdArgs[0], cmdArgs[1:], to)
		if err != nil {
			return cliExit(err, 1)
		}

		return cliExit("", exitCode)
	},
}

func init() {
	rootCmd.AddCommand(runCmd)
}

func parseRunArgs(args []string) (string, []string, bool, error) {
	var to string

	i := 0
	for i < len(args) {
		arg := args[i]

		if arg == "--" {
			rest := args[i+1:]
			if len(rest) == 0 {
				return "", nil, false, errors.Errorf("%w", errCommandRequired)
			}

			return to, rest, false, nil
		}

		if arg == "--help" || arg == "-h" {
			return "", nil, true, nil
		}

		if arg == "--to" || arg == "-t" {
			if i+1 >= len(args) {
				return "", nil, false, errors.Errorf("flag %q %w", arg, errFlagRequiresVal)
			}

			to = args[i+1]
			i += 2

			continue
		}

		if strings.HasPrefix(arg, "--to=") {
			to = arg[len("--to="):]
			i++

			continue
		}

		// First non-flag argument starts the command.
		return to, args[i:], false, nil
	}

	return "", nil, false, errors.Errorf("%w", errCommandRequired)
}
