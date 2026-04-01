package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	"github.com/edaniels/graft/errors"
	graft "github.com/edaniels/graft/pkg"
)

var (
	connectName          string
	connectForward       []string
	connectForwardPrefix bool
	connectSyncFlag      bool
	connectOS            string
	connectBackground    bool
)

var connectCmd = &cobra.Command{
	Use:   "connect [flags] <local_dir> <destination>[:<remote_dir>]",
	Short: "Connect to a remote machine or container",
	Long: `Connect to a remote machine or container.

Arguments:
  2 args: local_dir destination[:remote_dir]

Destination formats:
  user@host              SSH connection (default)
  ssh://user@host        SSH connection (explicit)
  docker://image[:tag]   Docker container

Use --sync with local_dir and remote_dir to enable file synchronization.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		localDir, destination, err := parseConnectArgs(args)
		if err != nil {
			return err
		}

		if destination == "" {
			return connectFromProject(cmd)
		}

		var remoteDir string

		// Parse remote_dir from destination (SCP-style colon syntax).
		// Only for non-docker destinations.
		if !strings.HasPrefix(destination, "docker://") {
			destination, remoteDir = parseDestination(destination)
		}

		// Resolve local_dir to absolute path.
		if localDir != "" {
			absDir, err := filepath.Abs(localDir)
			if err != nil {
				return errors.Wrap(err)
			}

			localDir = absDir
		}

		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		fwdCommands, fwdPorts := partitionForwardArgs(connectForward)

		params := graft.ConnectParams{
			Name:            connectName,
			LocalRoot:       localDir,
			RemoteRoot:      remoteDir,
			ForwardCommands: fwdCommands,
			ForwardPrefix:   connectForwardPrefix,
			PortForwards:    fwdPorts,
			WithSync:        connectSyncFlag,
			Background:      connectBackground,
		}

		switch {
		case strings.HasPrefix(destination, "docker://"):
			params.ImageTag = strings.TrimPrefix(destination, "docker://")
			if params.ImageTag == "" {
				return cliExit("docker:// requires an image tag", 1)
			}

			params.OSName = connectOS

			return client.InitializeDockerConnection(ctx, params)
		default:
			if connectOS != "" {
				return cliExit("--os is only valid for docker:// destinations", 1)
			}

			params.Destination = destination

			return client.InitializeRemoteConnection(ctx, params)
		}
	},
}

// parseSSHURLRemoteDir handles ssh:// URLs where the remote directory may be
// specified as either:
//   - SCP-style colon: ssh://user@host:~/proj    (colon part looks like a path, not a port)
//   - With port:       ssh://user@host:22         (port only, no remote dir)
//   - Port + dir:      ssh://user@host:22:~/proj  (port and remote dir)
//
// A numeric-only colon part is treated as a port (ssh://user@host:22).
func parseSSHURLRemoteDir(dest string) (string, string) {
	// Strip the prefix and work with the bare user@host[:port][:remoteDir].
	bare := strings.TrimPrefix(dest, "ssh://")
	bareDest, afterHost := parseDestinationRemoteDir(bare)

	if afterHost == "" {
		return dest, ""
	}

	// afterHost may be "22:~/proj", "22", "~/proj", etc.
	// Check if it starts with a numeric port.
	before, after, ok := strings.Cut(afterHost, ":")
	if !ok {
		// No second colon. Either it's all a port or all a remote dir.
		if isNumeric(afterHost) {
			return dest, ""
		}

		return "ssh://" + bareDest, afterHost
	}

	// There's a colon in afterHost. Check if the part before it is a port.
	possiblePort := before
	if isNumeric(possiblePort) {
		// It's port:remoteDir (e.g., "22:~/proj").
		return "ssh://" + bareDest + ":" + possiblePort, after
	}

	// Not a port: the whole thing is a remote dir (e.g., "some:path").
	return "ssh://" + bareDest, afterHost
}

// parseDestination parses a raw destination string into a bare host (user@host, no scheme)
// and an optional remote directory. It handles both ssh:// URLs and SCP-style user@host:dir.
func parseDestination(raw string) (string, string) {
	if strings.HasPrefix(raw, "ssh://") {
		dest, dir := parseSSHURLRemoteDir(raw)

		return strings.TrimPrefix(dest, "ssh://"), dir
	}

	return parseDestinationRemoteDir(raw)
}

func isNumeric(s string) bool {
	_, err := strconv.Atoi(s)

	return err == nil
}

// parseDestinationRemoteDir splits a destination string into the host part
// and an optional remote directory. For example:
//
//	"user@host:~/proj" -> ("user@host", "~/proj")
//	"user@host"        -> ("user@host", "")
//
// It handles the case where the colon separates the host from the remote dir
// by looking for the user@host pattern.
func parseDestinationRemoteDir(dest string) (string, string) {
	// Find the last @ to locate the host portion.
	atIdx := strings.LastIndex(dest, "@")
	if atIdx == -1 {
		// No @, treat plain hostname. Look for colon after the hostname.
		if before, after, ok := strings.Cut(dest, ":"); ok {
			return before, after
		}

		return dest, ""
	}

	// Has @. Look for colon after the @ (i.e., after the host part).
	hostStart := atIdx + 1
	rest := dest[hostStart:]

	if colonIdx := strings.Index(rest, ":"); colonIdx != -1 {
		return dest[:hostStart+colonIdx], dest[hostStart+colonIdx+1:]
	}

	return dest, ""
}

func parseConnectArgs(args []string) (string, string, error) {
	switch len(args) {
	case 0:
		return "", "", nil
	case 2:
		return args[0], args[1], nil
	default:
		return "", "", cliExit("local_dir and destination required", 1)
	}
}

func connectFromProject(cmd *cobra.Command) error {
	// look for project
	graftYamlFile, err := os.Open("graft.yaml")
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return errors.Wrap(err)
		}

		return cliExit("destination required", 1)
	}
	defer graftYamlFile.Close()

	dec := yaml.NewDecoder(graftYamlFile)

	var graftProject ProjectConfig
	if decErr := dec.Decode(&graftProject); decErr != nil {
		return errors.Wrap(decErr)
	}

	if len(graftProject.Destinations) == 0 {
		return errors.New("no destinations")
	}

	if len(graftProject.Destinations) != 1 {
		return errors.New("only one destination supported right now")
	}

	var (
		firstDestName   string
		firstDestConfig ProjectDestinationConfig
	)

	for destName, destConfig := range graftProject.Destinations {
		firstDestName = destName
		firstDestConfig = destConfig

		break
	}

	curDir, err := os.Getwd()
	if err != nil {
		return errors.Wrap(err)
	}

	wsDir, err := findWorkspaceDir(curDir)
	if err != nil {
		return errors.Wrap(err)
	}

	var syncPath string

	if wsDir != "" {
		wsData, readErr := os.ReadFile(filepath.Join(wsDir, "graft.yaml"))
		if readErr != nil {
			return errors.Wrap(readErr)
		}

		var graftWorkspace WorkspaceConfig
		if wsErr := yaml.Unmarshal(wsData, &graftWorkspace); wsErr != nil {
			return errors.Wrap(wsErr)
		}

		if graftWorkspace.Defaults.SyncWorkspace {
			syncPath = wsDir
		}
	}

	client, ctx := newClient(cmd.Context(), true)
	defer client.Close()

	params := resolveProjectConnectParams(resolveProjectConnectInput{
		projectDir:    curDir,
		destName:      firstDestName,
		destConfig:    firstDestConfig,
		forwards:      graftProject.Forwards,
		workspaceDir:  syncPath,
		syncWorkspace: syncPath != "",
	})

	params.Destination = firstDestConfig.Host
	params.Username = firstDestConfig.User

	if err = client.InitializeRemoteConnection(ctx, params); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

type resolveProjectConnectInput struct {
	projectDir    string
	destName      string
	destConfig    ProjectDestinationConfig
	forwards      []string
	workspaceDir  string
	syncWorkspace bool
}

func resolveProjectConnectParams(in resolveProjectConnectInput) graft.ConnectParams {
	commands, ports := partitionForwardArgs(in.forwards)

	params := graft.ConnectParams{
		Name:            in.destName,
		LocalRoot:       in.projectDir,
		RemoteRoot:      in.destConfig.SyncTo,
		ForwardCommands: commands,
		ForwardPrefix:   in.destConfig.Prefix,
		PortForwards:    ports,
	}

	// Workspace sync takes precedence over project-level sync because it syncs
	// the entire workspace directory (setting SyncSource/SyncDest), making a
	// narrower project sync redundant.
	if in.syncWorkspace && in.workspaceDir != "" {
		relPath, err := filepath.Rel(in.workspaceDir, in.projectDir)
		if err == nil && relPath != "." {
			params.RemoteRoot = filepath.Join(in.destConfig.SyncTo, relPath)
		}

		params.SyncSource = in.workspaceDir
		params.SyncDest = in.destConfig.SyncTo
		params.WithSync = true
	} else if in.destConfig.Sync {
		params.WithSync = true
	}

	return params
}

var disconnectCmd = &cobra.Command{
	Use:               "disconnect <connection>",
	Short:             "Disconnect from a remote connection",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeConnectionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.RemoveConnection(ctx, args[0])
	},
}

func init() {
	connectCmd.Flags().StringVarP(&connectName, "name", "n", "", "Connection name")
	connectCmd.Flags().StringSliceVar(&connectForward, "forward", nil, "Commands to forward")
	connectCmd.Flags().BoolVar(&connectForwardPrefix, "forward-prefix", false, "Forward with connection name prefix")
	connectCmd.Flags().BoolVar(&connectSyncFlag, "sync", false, "Enable file synchronization")
	connectCmd.Flags().StringVar(&connectOS, "os", "", "Container OS (docker:// only)")
	connectCmd.Flags().BoolVar(&connectBackground, "background", false, "Exclude from CWD-based auto-selection")

	rootCmd.AddCommand(connectCmd)
	rootCmd.AddCommand(disconnectCmd)
}
