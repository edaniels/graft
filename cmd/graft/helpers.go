package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v2"

	"github.com/edaniels/graft/errors"
	graft "github.com/edaniels/graft/pkg"
)

var logger = graft.NewLogger(slog.LevelError)

func init() {
	slog.SetDefault(logger)
}

type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string {
	return e.msg
}

func cliExit(err any, code int) error {
	if code == 0 {
		return nil
	}

	var msg string

	switch e := err.(type) {
	case error:
		if s := status.Convert(e); s != nil {
			msg = s.Message()
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

func newClient(ctx context.Context, withOOBMsgs bool) (*graft.LocalClient, context.Context) {
	client, ctx, err := graft.NewLocalClient(
		ctx,
		os.Stdout,
		os.Stderr,
		func(err error) error {
			return cliExit(err, 1)
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
