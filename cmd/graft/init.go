package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	"github.com/edaniels/graft/errors"
)

var (
	initWorkspace bool
	initSyncTo    string
	initForward   []string
	initPrefix    bool
	initForce     bool
)

var initCmd = &cobra.Command{
	Use:   "init [flags] [name] [destination]",
	Short: "Generate a graft.yaml configuration file",
	Long: `Generate a graft.yaml configuration file.

Workspace mode (--workspace, 0 args):
  graft init --workspace

Project mode (2 args):
  graft init <name> <destination> [flags]`,
	RunE: func(_ *cobra.Command, args []string) error {
		if initWorkspace {
			if len(args) != 0 {
				return cliExit("--workspace does not accept arguments", 1)
			}

			return runInitWorkspace()
		}

		if len(args) != 2 {
			return cliExit("expected 2 arguments: name and destination", 1)
		}

		return runInitProject(args[0], args[1])
	},
}

func runInitWorkspace() error {
	if err := checkExistingConfig(); err != nil {
		return err
	}

	cfg := buildWorkspaceConfig()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return errors.Wrap(err)
	}

	if err := os.WriteFile("graft.yaml", data, 0o600); err != nil {
		return errors.Wrap(err)
	}

	fmt.Fprintln(os.Stderr, "created graft.yaml (workspace, syncWorkspace: true)")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "next: create a project with `graft init` in a subdirectory")

	return nil
}

func runInitProject(name, destination string) error {
	if err := checkExistingConfig(); err != nil {
		return err
	}

	cfg := buildProjectConfig(name, destination, initSyncTo, initForward, initPrefix)

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return errors.Wrap(err)
	}

	if err := os.WriteFile("graft.yaml", data, 0o600); err != nil {
		return errors.Wrap(err)
	}

	fmt.Fprintln(os.Stderr, "created graft.yaml")

	dest := cfg.Destinations[name]

	displayHost := dest.Host
	if dest.User != "" {
		displayHost = dest.User + "@" + dest.Host
	}

	fmt.Fprintf(os.Stderr, "  destination: %s -> %s\n", name, displayHost)

	if dest.SyncTo != "" {
		fmt.Fprintf(os.Stderr, "  sync to:     %s\n", dest.SyncTo)
	}

	if len(cfg.Forwards) > 0 {
		displayForwards := cfg.Forwards
		if dest.Prefix {
			prefixed := make([]string, len(displayForwards))
			for i, f := range displayForwards {
				prefixed[i] = name + "-" + f
			}

			displayForwards = prefixed
		}

		fmt.Fprintf(os.Stderr, "  forward:     %s\n", strings.Join(displayForwards, ", "))
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "next: run `graft connect` to establish the connection")

	return nil
}

func checkExistingConfig() error {
	if initForce {
		return nil
	}

	if _, err := os.Stat("graft.yaml"); err == nil {
		return cliExit("graft.yaml already exists (use --force to overwrite)", 1)
	}

	return nil
}

func buildWorkspaceConfig() WorkspaceConfig {
	return WorkspaceConfig{
		Version:   "v1",
		Workspace: true,
		Defaults:  WorkspaceConfigDefaults{SyncWorkspace: true},
	}
}

func buildProjectConfig(name, destination, syncTo string, forwards []string, prefix bool) ProjectConfig {
	var user, host string
	if i := strings.LastIndex(destination, "@"); i != -1 {
		user = destination[:i]
		host = destination[i+1:]
	} else {
		host = destination
	}

	destConfig := ProjectDestinationConfig{
		Host:   host,
		User:   user,
		SyncTo: syncTo,
		Prefix: prefix,
	}

	return ProjectConfig{
		Version:      "v1",
		Forwards:     forwards,
		Destinations: map[string]ProjectDestinationConfig{name: destConfig},
	}
}

func init() {
	initCmd.Flags().BoolVar(&initWorkspace, "workspace", false, "Generate workspace config instead of project config")
	initCmd.Flags().StringVar(&initSyncTo, "sync-to", "", "Remote sync destination path")
	initCmd.Flags().StringSliceVar(&initForward, "forward", nil, "Commands to forward")
	initCmd.Flags().BoolVar(&initPrefix, "prefix", false, "Prefix forwarded commands with connection name")
	initCmd.Flags().BoolVar(&initForce, "force", false, "Overwrite existing graft.yaml")

	rootCmd.AddCommand(initCmd)
}
