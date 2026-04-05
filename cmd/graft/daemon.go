package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
	graft "github.com/edaniels/graft/pkg"
)

var (
	daemonReplace      bool
	daemonDetach       bool
	daemonAsRemote     bool
	daemonIsDaemonized bool
	daemonIsRestart    bool
	daemonIdentity     string
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run the graft daemon server",
	RunE: func(_ *cobra.Command, _ []string) error {
		return runDaemon(daemonReplace, daemonDetach, daemonAsRemote, daemonIsDaemonized, daemonIsRestart, daemonIdentity)
	},
}

var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show recent daemon logs",
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.PrintDaemonLogs(ctx)
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon",
	RunE: func(cmd *cobra.Command, _ []string) error {
		if warnIfServiceManaged() {
			return nil
		}

		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.Shutdown(ctx)
	},
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the running daemon",
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.Restart(ctx)
	},
}

var daemonStatusConnection string

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon health, version, and recent logs",
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.PrintDaemonStatus(ctx, daemonStatusConnection)
	},
}

func init() {
	daemonCmd.Flags().BoolVar(&daemonReplace, "replace", false, "Kill any existing daemon and take over")
	daemonCmd.Flags().BoolVarP(&daemonDetach, "detach", "d", false, "Run daemon in background")
	daemonCmd.Flags().BoolVar(&daemonAsRemote, "as-remote", false, "Run as a remote daemon")
	daemonCmd.Flags().StringVar(&daemonIdentity, "identity", "", "Remote daemon identity for socket namespacing")
	daemonCmd.Flags().BoolVar(&daemonIsDaemonized, "is-daemonized", false, "")
	daemonCmd.Flags().BoolVar(&daemonIsRestart, "is-restart", false, "")
	errors.Unchecked(daemonCmd.Flags().MarkHidden("is-daemonized"))
	errors.Unchecked(daemonCmd.Flags().MarkHidden("is-restart"))
	errors.Unchecked(daemonCmd.Flags().MarkHidden("identity"))

	daemonStatusCmd.Flags().StringVar(&daemonStatusConnection, "connection", "", "Connection to check status for")
	daemonStatusCmd.RegisterFlagCompletionFunc("connection", completeConnectionNames) //nolint:errcheck

	daemonCmd.AddCommand(daemonLogsCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	daemonCmd.AddCommand(daemonStatusCmd)

	rootCmd.AddCommand(daemonCmd)
}

func runDaemon(replace, detach, asRemote, isDaemonized, isRestart bool, identity string) error {
	serverRole := graft.ServerRoleLocal
	if asRemote {
		serverRole = graft.ServerRoleRemote
	}

	if !isDaemonized && (detach || asRemote) {
		return serveDaemonize(serverRole)
	}

	runtimeCtx := setupRuntimeContext()
	defer runtimeCtx.Close(context.Canceled)

	logger, buffWriter := graft.NewBufferedLogger(slog.LevelInfo)
	slog.SetDefault(logger)

	logVersion(runtimeCtx.Ctx, logger)

	go checkForUpdateInBackground(runtimeCtx.Ctx, logger)

	// For local daemons, load persistent identity to use when connecting to remotes.
	if serverRole == graft.ServerRoleLocal {
		localIdentity, identityErr := graft.DaemonIdentity()
		if identityErr != nil {
			return errors.WrapPrefix(identityErr, "error loading daemon identity")
		}

		identity = localIdentity
		logger.InfoContext(runtimeCtx.Ctx, "daemon identity", "identity", identity)
	}

	var success bool

	defer func() {
		if success {
			return
		}

		if err := respondToDaemonizerSignal(success, isDaemonized, isRestart); err != nil {
			panic(err)
		}
	}()

	var rootConfig graft.RootConfig

	rootConfigPath, err := graft.RootConfigPathForCurrentHost(serverRole)
	if err != nil {
		return errors.Wrap(err)
	}

	if _, statErr := os.Stat(rootConfigPath); statErr != nil {
		if !errors.Is(statErr, os.ErrNotExist) {
			return errors.Wrap(statErr)
		}

		mkErr := os.MkdirAll(filepath.Dir(rootConfigPath), graft.DirPerms)
		if mkErr != nil {
			return errors.Wrap(mkErr)
		}

		persistErr := rootConfig.Persist(rootConfigPath)
		if persistErr != nil {
			return errors.Wrap(persistErr)
		}
	}

	reloadErr := rootConfig.Reload(rootConfigPath)
	if reloadErr != nil {
		slog.ErrorContext(runtimeCtx.Ctx, "error reloading config", "error", reloadErr.Error())

		return errors.Wrap(reloadErr)
	}

	server, err := graft.NewServer(&rootConfig, serverRole, rootConfigPath, replace, buffWriter, identity)
	if err != nil {
		return errors.Wrap(err)
	}

	runtimeCtx.SignalStartupDone()

	success = true
	if respondErr := respondToDaemonizerSignal(success, isDaemonized, isRestart); respondErr != nil {
		return respondErr
	}

	runErr := server.Run(runtimeCtx.Ctx)
	if runErr != nil {
		return errors.Wrap(runErr)
	}

	<-runtimeCtx.ShutdownSignal
	runtimeCtx.Close(errors.Wrap(graft.ErrShuttingDown))
	server.Close()

	if !server.RestartRequested() {
		return nil
	}

	return restart(runtimeCtx.Ctx, os.Args, os.Environ())
}

func restart(ctx context.Context, originalArgs []string, originalEnv []string) error {
	slog.WarnContext(ctx, "restarting")

	execAs, err := os.Executable()
	if err != nil {
		return errors.Wrap(err)
	}

	newArgs := slices.DeleteFunc(originalArgs, func(arg string) bool {
		return strings.Contains(arg, "is-restart")
	})
	newArgs = append(newArgs, "--is-restart")

	slog.WarnContext(ctx, "restarting with command", "arg0", execAs, "args", newArgs)

	if err := syscall.Exec(execAs, newArgs, originalEnv); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

type runtimeContext struct {
	Ctx               context.Context //nolint:containedctx // owned
	ShutdownSignal    <-chan os.Signal
	SignalStartupDone func()
	Close             func(reason error)
}

func setupRuntimeContext() runtimeContext {
	runCtx, cancel := context.WithCancelCause(context.Background())
	startupCtx, startupDone := context.WithCancel(runCtx)

	go func() {
		slowTimer := time.NewTimer(5 * time.Second)
		defer slowTimer.Stop()

		select {
		case <-startupCtx.Done():
		case <-slowTimer.C:
			fmt.Fprintln(os.Stderr, "startup is taking a while")
			graft.DumpGoroutines()
		}
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	var wg sync.WaitGroup

	dump := make(chan os.Signal, 1)
	signal.Notify(dump, syscall.SIGUSR1)
	wg.Go(func() {
		for {
			select {
			case <-dump:
				graft.DumpGoroutines()
			case <-runCtx.Done():
				return
			}
		}
	})

	return runtimeContext{runCtx, shutdown, startupDone, func(reason error) {
		defer wg.Wait()
		defer cancel(reason)
	}}
}

func logVersion(ctx context.Context, logger *slog.Logger) {
	buildVersion := graft.BuildVersion()
	verFields := []any{"pid", os.Getpid()}

	if buildVersion.Version != nil {
		verFields = append(verFields, "version", buildVersion.GetVersion())
	}

	if buildVersion.VcsRevision != nil {
		verFields = append(verFields, "vcs_revision", buildVersion.GetVcsRevision())
	}

	if buildVersion.VcsModified != nil {
		verFields = append(verFields, "vcs_modified", buildVersion.GetVcsModified())
	}

	if buildVersion.VcsTime != nil {
		verFields = append(verFields, "vcs_time", buildVersion.GetVcsTime())
	}

	if buildVersion.Notes != nil {
		verFields = append(verFields, "notes", buildVersion.GetNotes())
	}

	logger.InfoContext(ctx, graft.Name, verFields...)
}

func serveDaemonize(role graft.ServerRole) error {
	newArgs := os.Args[1:]
	newArgs = append(newArgs, "--is-daemonized=true")
	cmd := exec.CommandContext(context.Background(), os.Args[0], newArgs...) //nolint:gosec // our args

	// TODO(erd): they will need to be truncated/rotated over time
	logsPath, err := graft.DaemonLogsPathForCurrentHost(role)
	if err != nil {
		panic(err)
	}

	if mkErr := os.MkdirAll(logsPath, graft.DirPerms); mkErr != nil {
		panic(mkErr)
	}

	outFile, err := os.Create(filepath.Join(logsPath, "out.log"))
	if err != nil {
		panic(err)
	}

	errorFile, err := os.Create(filepath.Join(logsPath, "error.log"))
	if err != nil {
		panic(err)
	}

	signalPipeR, signalPipeW, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	defer signalPipeW.Close()

	cmd.Stdout = outFile
	cmd.Stderr = errorFile
	cmd.ExtraFiles = append(cmd.ExtraFiles, signalPipeW)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if startErr := cmd.Start(); startErr != nil {
		panic(startErr)
	}

	go func() {
		var buf [64]byte
		// ignoring truncation possibly happening
		seekPos := int64(0)

		for {
			rd, readErr := errorFile.Read(buf[:])
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					errorFile.Seek(seekPos, 0) //nolint:errcheck

					time.Sleep(time.Millisecond)

					continue
				}
			}

			seekPos += int64(rd) + 1
			os.Stderr.Write(buf[:rd])
		}
	}()

	var buf [1]byte
	// just read 1 byte
	rd, err := signalPipeR.Read(buf[:])
	if err != nil {
		panic(err)
	}

	if rd != 1 || len(buf) != 1 {
		return errors.New("did not get first and only ack byte")
	}

	if buf[0] != 0 {
		// sleep a bit for some logs to maybe print above
		time.Sleep(3 * time.Second)

		return errors.Errorf("failed to daemonize (code=%d)", buf[0])
	}

	return nil
}

func checkForUpdateInBackground(ctx context.Context, logger *slog.Logger) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := graft.ReleaseClientFromConfig()

	result, err := graft.CheckForUpdate(checkCtx, client, graft.VersionString())
	if err != nil {
		return
	}

	if result.UpdateAvailable {
		logger.WarnContext(ctx, "a new version of graft is available",
			"latest", result.LatestVersion, "current", result.CurrentVersion)
	} else if !result.Skipped {
		logger.DebugContext(ctx, "no update available",
			"latest", result.LatestVersion, "current", result.CurrentVersion)
	}
}

func respondToDaemonizerSignal(ok bool, isDaemonized bool, isRestart bool) error {
	if !isDaemonized || isRestart {
		return nil
	}

	signalPipe := os.NewFile(3, "signal") // ExtraFiles are 3+i

	val := byte(0)
	if !ok {
		val = 1
	}

	if _, writeErr := signalPipe.Write([]byte{val}); writeErr != nil {
		return errors.Wrap(writeErr)
	}

	if closeErr := signalPipe.Close(); closeErr != nil {
		return errors.Wrap(closeErr)
	}

	return nil
}
