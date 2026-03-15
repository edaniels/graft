package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
	graft "github.com/edaniels/graft/pkg"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor [destination]",
	Short: "Check environment setup and diagnose issues",
	Long: `Run diagnostic checks on the local environment and optionally on a remote
destination. All checks are read-only and do not modify state.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		var hasFailure bool

		// === Local checks ===
		fmt.Fprintln(os.Stderr, "=== Local ===")

		printResult(graft.CheckShellActivation(os.LookupEnv))

		daemonResult := graft.CheckLocalDaemon(func(ctx context.Context) (*graftv1.StatusResponse, error) {
			sockPath, err := graft.DaemonSocketPathForCurrentHost(graft.ServerRoleLocal)
			if err != nil {
				return nil, errors.Wrap(err)
			}

			return dialDaemonStatus(ctx, sockPath)
		})
		if daemonResult.Status == graft.CheckFail {
			hasFailure = true
		}

		printResult(daemonResult)

		updateResult := checkForUpdates(ctx)
		printResult(updateResult)

		// === Remote checks ===
		if len(args) > 0 {
			destination := args[0]
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "=== Remote: %s ===\n", destination)

			if failed := runRemoteChecks(ctx, destination); failed {
				hasFailure = true
			}
		}

		if hasFailure {
			return cliExit("", 1)
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

func printResult(r graft.CheckResult) {
	var tag string

	switch r.Status {
	case graft.CheckPass:
		tag = color.GreenString("[PASS]")
	case graft.CheckWarn:
		tag = color.YellowString("[WARN]")
	case graft.CheckFail:
		tag = color.RedString("[FAIL]")
	}

	fmt.Fprintf(os.Stderr, "%s %s: %s\n", tag, r.Name, r.Message)

	for _, d := range r.Details {
		fmt.Fprintf(os.Stderr, "       %s\n", d)
	}
}

func checkForUpdates(ctx context.Context) graft.CheckResult {
	client := graft.NewGitHubReleaseClient(graft.GithubToken())

	return graft.CheckUpdates(ctx, client, graft.VersionString())
}

func dialDaemonStatus(ctx context.Context, sockPath string) (*graftv1.StatusResponse, error) {
	if _, err := os.Stat(sockPath); err != nil {
		if os.IsNotExist(err) {
			return nil, graft.ErrDaemonNotRunning
		}

		return nil, errors.Wrap(err)
	}

	conn, err := graft.DialDaemonSocket(sockPath)
	if err != nil {
		return nil, errors.Wrap(err)
	}
	defer conn.Close()

	svcClient := graftv1.NewGraftServiceClient(conn)

	statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	resp, err := svcClient.Status(statusCtx, &graftv1.StatusRequest{})
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return resp, nil
}

func runRemoteChecks(ctx context.Context, destination string) bool {
	var hasFailure bool

	destURL, err := graft.ParseDestination(destination)
	if err != nil {
		printResult(graft.CheckResult{
			Name:    "SSH connection",
			Status:  graft.CheckFail,
			Message: fmt.Sprintf("invalid destination: %s", err),
		})

		return true
	}

	var destUser string
	if destURL.User != nil {
		destUser = destURL.User.Username()
	}

	sshDetails := graft.ResolveSSHDetails(
		destURL.Hostname(),
		destURL.Port(),
		destUser,
		graft.DefaultSSHConfigResolver(),
	)

	// Load local daemon identity (used to namespace remote state).
	identity, err := graft.DaemonIdentity()
	if err != nil {
		printResult(graft.CheckResult{
			Name:    "SSH connection",
			Status:  graft.CheckWarn,
			Message: fmt.Sprintf("could not load daemon identity: %s", err),
		})

		identity = ""
	}

	// Connect to remote.
	factory := graft.NewSSHConnectorFactory()

	connector, err := factory.CreateConnector(ctx, destURL, identity)
	if err != nil {
		printResult(graft.CheckResult{
			Name:    "SSH connection",
			Status:  graft.CheckFail,
			Message: fmt.Sprintf("failed to create connector: %s", err),
			Details: sshDetails,
		})

		return true
	}
	defer connector.Close()

	_, err = connector.InitializeRemote(ctx)
	if err != nil {
		printResult(graft.CheckResult{
			Name:    "SSH connection",
			Status:  graft.CheckFail,
			Message: fmt.Sprintf("failed to connect: %s", err),
			Details: sshDetails,
		})

		return true
	}

	printResult(graft.CheckResult{
		Name:    "SSH connection",
		Status:  graft.CheckPass,
		Message: "connected",
		Details: sshDetails,
	})

	// Transport mode.
	type udsProber interface {
		ProbeUDS(ctx context.Context) error
	}

	if prober, ok := connector.(udsProber); ok {
		printResult(graft.CheckTransportMode(func() error {
			return prober.ProbeUDS(ctx)
		}))
	}

	// Remote environment.
	envResult, info := graft.CheckRemoteEnvironment(ctx, connector)
	if envResult.Status == graft.CheckFail {
		hasFailure = true
	}

	printResult(envResult)

	if info.HomeDir == "" {
		return hasFailure
	}

	// Remote daemon.
	connectFn := func(ctx context.Context, binPath, socketPath string) (*graftv1.VersionInfo, bool, error) {
		remoteConn, online, connectErr := connector.ConnectToRemoteDaemon(ctx, binPath, socketPath)
		if connectErr != nil || !online {
			return nil, false, errors.Wrap(connectErr)
		}
		defer remoteConn.Close()

		svcClient := graftv1.NewGraftServiceClient(remoteConn.ClientConn())

		statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		resp, statusErr := svcClient.Status(statusCtx, &graftv1.StatusRequest{})
		if statusErr != nil {
			return nil, true, errors.Wrap(statusErr)
		}

		return resp.GetVersionInfo(), true, nil
	}
	daemonResult := graft.CheckRemoteDaemon(ctx, connector, info, connectFn, graft.BuildVersion())
	printResult(daemonResult)

	// Remote directories.
	printResult(graft.CheckRemoteDirectories(info))

	return hasFailure
}
