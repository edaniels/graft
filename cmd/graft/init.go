package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	"github.com/edaniels/graft/errors"
)

var (
	initWorkspace     bool
	initName          string
	initSync          bool
	initForward       []string
	initForwardPrefix bool
	initForce         bool
)

var initCmd = &cobra.Command{
	Use:   "init [flags] <local_dir> <destination>[:<remote_dir>]",
	Short: "Generate a graft.yaml configuration file",
	Long: `Generate a graft.yaml configuration file.

Workspace mode (--workspace, 0 args):
  graft init --workspace

Project mode (2 args):
  graft init <local_dir> <destination>[:<remote_dir>] --name <name> [flags]

Example:
  graft init . ubuntu@myhost:~/mydir --sync --name myconn --forward make`,
	RunE: func(_ *cobra.Command, args []string) error {
		if initWorkspace {
			if len(args) != 0 {
				return cliExit("--workspace does not accept arguments", 1)
			}

			return runInitWorkspace()
		}

		if len(args) != 2 {
			return cliExit("expected 2 arguments: local_dir and destination", 1)
		}

		if initName == "" {
			return cliExit("--name is required", 1)
		}

		return runInitProject(args[0], args[1])
	},
}

func runInitWorkspace() error {
	if err := checkExistingConfig("."); err != nil {
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

func runInitProject(localDir, rawDestination string) error {
	absDir, err := filepath.Abs(localDir)
	if err != nil {
		return errors.Wrap(err)
	}

	if checkErr := checkExistingConfig(absDir); checkErr != nil {
		return checkErr
	}

	destination, remoteDir := parseDestination(rawDestination)

	var user, host string
	if i := strings.LastIndex(destination, "@"); i != -1 {
		user = destination[:i]
		host = destination[i+1:]
	} else {
		host = destination
	}

	cfg := buildProjectConfig(initName, host, user, remoteDir, initForward, initForwardPrefix, initSync)

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return errors.Wrap(err)
	}

	configPath := filepath.Join(absDir, "graft.yaml")

	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		return errors.Wrap(err)
	}

	fmt.Fprintln(os.Stderr, "created graft.yaml")

	dest := cfg.Destinations[initName]

	displayHost := dest.Host
	if dest.User != "" {
		displayHost = dest.User + "@" + dest.Host
	}

	fmt.Fprintf(os.Stderr, "  destination: %s -> %s\n", initName, displayHost)

	if dest.SyncTo != "" {
		fmt.Fprintf(os.Stderr, "  sync to:     %s\n", dest.SyncTo)
	}

	if len(cfg.Forwards) > 0 {
		displayForwards := cfg.Forwards
		if dest.Prefix {
			prefixed := make([]string, len(displayForwards))
			for i, f := range displayForwards {
				prefixed[i] = initName + "-" + f
			}

			displayForwards = prefixed
		}

		fmt.Fprintf(os.Stderr, "  forward:     %s\n", strings.Join(displayForwards, ", "))
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "next: run `graft connect` to establish the connection")

	return nil
}

func checkExistingConfig(dir string) error {
	if initForce {
		return nil
	}

	if _, err := os.Stat(filepath.Join(dir, "graft.yaml")); err == nil {
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

func buildProjectConfig(name, host, user, remoteDir string, forwards []string, prefix, sync bool) ProjectConfig {
	destConfig := ProjectDestinationConfig{
		Host:   host,
		User:   user,
		SyncTo: remoteDir,
		Prefix: prefix,
		Sync:   sync,
	}

	return ProjectConfig{
		Version:      "v1",
		Forwards:     forwards,
		Destinations: map[string]ProjectDestinationConfig{name: destConfig},
	}
}

func init() {
	initCmd.Flags().BoolVar(&initWorkspace, "workspace", false, "Generate workspace config instead of project config")
	initCmd.Flags().StringVarP(&initName, "name", "n", "", "Connection name (required for project mode)")
	initCmd.Flags().BoolVar(&initSync, "sync", false, "Enable file synchronization")
	initCmd.Flags().StringSliceVar(&initForward, "forward", nil, "Commands to forward")
	initCmd.Flags().BoolVar(&initForwardPrefix, "forward-prefix", false, "Prefix forwarded commands with connection name")
	initCmd.Flags().BoolVar(&initForce, "force", false, "Overwrite existing graft.yaml")

	rootCmd.AddCommand(initCmd)
}
