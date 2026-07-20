package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v2"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
	graft "github.com/edaniels/graft/pkg"
)

var logger = graft.NewLogger(slog.LevelError)

func init() {
	slog.SetDefault(logger)
}

func completeConnectionNames(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sockPath, err := graft.DaemonSocketPathForCurrentHost(graft.ServerRoleLocal)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	clientConn, svcClient, _, err := graft.ConnectAndCheck(ctx, sockPath)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer clientConn.Close()

	resp, err := svcClient.ListConnections(ctx, &graftv1.ListConnectionsRequest{})
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	names := make([]string, 0, len(resp.GetConnections()))
	for name := range resp.GetConnections() {
		names = append(names, name)
	}

	sort.Strings(names)

	return names, cobra.ShellCompDirectiveNoFileComp
}

type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string {
	return e.msg
}

func approximateOriginalCommand(cmd *cobra.Command, args []string) []string {
	parts := []string{cmd.CalledAs()}
	for p := cmd.Parent(); p != nil; p = p.Parent() {
		name := p.CalledAs()
		if p.Parent() == nil { // p is the root
			name = os.Args[0]
		}

		parts = append([]string{name}, parts...)
	}

	cmd.Flags().Visit(func(f *pflag.Flag) {
		parts = append(parts, "--"+f.Name+"="+f.Value.String())
	})

	return append(parts, args...)
}

func cliExit(cmd *cobra.Command, args []string, err any, code int) error {
	if code == 0 {
		return nil
	}

	var msg string

	switch e := err.(type) {
	case error:
		if s := status.Convert(e); s != nil {
			msg = s.Message()
			// check if we should suggest a connection
			if cmd.Flag("to") != nil {
				for _, details := range s.Details() {
					if errInfo, ok := details.(*errdetails.ErrorInfo); ok {
						connNameHint, ok := errInfo.GetMetadata()[errors.ErrorMetadataFieldConnectionNameHint]
						if ok {
							originalCmd := append(approximateOriginalCommand(cmd, args), "--to", connNameHint)
							msg = fmt.Sprintf(
								"%s; the following might work:\n\t"+
									"%s\n"+
									"OR pin the connection to this session with:\n\t"+
									"%s use %s", msg,
								strings.Join(originalCmd, " "),
								os.Args[0],
								connNameHint,
							)

							break
						}
					}
				}
			}
		} else {
			msg = e.Error()
		}
	case string:
		msg = e
	default:
		msg = fmt.Sprintf("%v", err)
	}

	return &exitCodeError{code: code, msg: msg}
}

func newClient(ctx context.Context, cmd *cobra.Command, args []string, withOOBMsgs bool) (*graft.LocalClient, context.Context) {
	client, ctx, err := graft.NewLocalClient(
		ctx,
		os.Stdout,
		os.Stderr,
		func(err error) error {
			return cliExit(cmd, args, err, 1)
		},
		withOOBMsgs,
		logger,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	return client, ctx
}

type ProjectDestinationConfig struct {
	Host   string `yaml:"host"`
	User   string `yaml:"user"`
	SyncTo string `yaml:"syncTo"`
	Prefix bool   `yaml:"prefix"`
	Sync   bool   `yaml:"sync"`
	// SyncGit additionally replicates the project's .git directory one-way
	// so the remote has a read-only git view.
	SyncGit bool `yaml:"syncGit,omitempty"`
}

type ProjectConfig struct {
	Version      string                              `yaml:"version"`
	Forwards     []string                            `yaml:"forward"`
	Destinations map[string]ProjectDestinationConfig `yaml:"destinations"`
}

type WorkspaceConfigDefaults struct {
	SyncWorkspace bool `yaml:"syncWorkspace"`
}

type WorkspaceConfig struct {
	Version   string                  `yaml:"version"`
	Workspace bool                    `yaml:"workspace"`
	Defaults  WorkspaceConfigDefaults `yaml:"defaults"`
}

// findWorkspaceDir searches parent directories of startDir for a graft.yaml
// with workspace: true. Returns the workspace directory path, or "" if none found.
func findWorkspaceDir(startDir string) (string, error) {
	prevDir := startDir
	for searchDir := filepath.Dir(startDir); searchDir != prevDir; searchDir = filepath.Dir(searchDir) {
		prevDir = searchDir

		checkPath := filepath.Join(searchDir, "graft.yaml")

		data, readErr := os.ReadFile(checkPath)
		if readErr != nil {
			if !errors.Is(readErr, os.ErrNotExist) {
				return "", errors.Wrap(readErr)
			}

			continue
		}

		var cfg WorkspaceConfig
		if unmarshalErr := yaml.Unmarshal(data, &cfg); unmarshalErr != nil {
			continue
		}

		if cfg.Workspace {
			return searchDir, nil
		}
	}

	return "", nil
}
