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
	Use:                "run [-t <connection>] [-m <pattern>] <command> [args...]",
	Short:              "Run a command on a remote connection",
	DisableFlagParsing: true,
	ValidArgsFunction:  completeRunArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ra, helpRequested, err := parseRunArgs(args)
		if helpRequested {
			return cmd.Help()
		}
		if err != nil {
			return cliExit(err, 1)
		}

		client, ctx := newClient(cmd.Context(), false)
		defer client.Close()

		var exitCode int
		if ra.match != "" {
			exitCode, err = client.RunCommandMultiTarget(ctx, ra.command[0], ra.command[1:], ra.match)
		} else {
			exitCode, err = client.RunCommand(ctx, ra.command[0], ra.command[1:], ra.to)
		}
		if err != nil {
			return cliExit(err, 1)
		}

		return cliExit("", exitCode)
	},
}

func init() {
	rootCmd.AddCommand(runCmd)
}

func completeRunArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Complete connection names after --to/-t.
	if len(args) > 0 && (args[len(args)-1] == "--to" || args[len(args)-1] == "-t") {
		return completeConnectionNames(cmd, args, toComplete)
	}

	if after, ok := strings.CutPrefix(toComplete, "--to="); ok {
		return completeConnectionNames(cmd, args, after)
	}

	return nil, cobra.ShellCompDirectiveDefault
}

type runArgs struct {
	to      string
	match   string
	command []string
}

func parseRunArgs(args []string) (runArgs, bool, error) {
	var ra runArgs

	i := 0
	for i < len(args) {
		arg := args[i]

		if arg == "--" {
			rest := args[i+1:]
			if len(rest) == 0 {
				return runArgs{}, false, errors.Errorf("%w", errCommandRequired)
			}

			ra.command = rest

			return ra, false, nil
		}

		if arg == "--help" || arg == "-h" {
			return runArgs{}, true, nil
		}

		if arg == "--to" || arg == "-t" {
			if i+1 >= len(args) {
				return runArgs{}, false, errors.Errorf("flag %q %w", arg, errFlagRequiresVal)
			}

			ra.to = args[i+1]
			i += 2

			continue
		}

		if strings.HasPrefix(arg, "--to=") {
			ra.to = arg[len("--to="):]
			i++

			continue
		}

		if arg == "--match" || arg == "-m" {
			if i+1 >= len(args) {
				return runArgs{}, false, errors.Errorf("flag %q %w", arg, errFlagRequiresVal)
			}

			ra.match = args[i+1]
			i += 2

			continue
		}

		if strings.HasPrefix(arg, "--match=") {
			ra.match = arg[len("--match="):]
			i++

			continue
		}

		// First non-flag argument starts the command.
		ra.command = args[i:]

		return ra, false, nil
	}

	return runArgs{}, false, errors.Errorf("%w", errCommandRequired)
}
