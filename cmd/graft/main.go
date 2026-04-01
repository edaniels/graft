package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
	graft "github.com/edaniels/graft/pkg"
)

var rootCmd = &cobra.Command{
	Use:           "graft",
	Short:         "Local-first remote development platform",
	Version:       versionOutput(),
	SilenceUsage:  true,
	SilenceErrors: true,
}

func versionOutput() string {
	return graft.VersionString()
}

func main() {
	// Handle raw forwarder mode before CLI parsing; must bypass CLI framework
	// for raw stdin/stdout forwarding to work correctly with remote daemon
	if len(os.Args) >= 2 && os.Args[1] == "raw" {
		var identity string
		if len(os.Args) == 3 {
			identity = os.Args[2]
		}

		rawForwarderForRemote(identity)

		return
	}

	code := runCLI()
	if code != 0 {
		os.Exit(code)
	}
}

func runCLI() int {
	sigCtx, sigCancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer sigCancel()

	err := rootCmd.ExecuteContext(sigCtx)
	if err == nil {
		return 0
	}

	var exitErr *exitCodeError
	if errors.As(err, &exitErr) {
		if exitErr.msg != "" {
			fmt.Fprintln(os.Stderr, exitErr.msg)
		}

		return exitErr.code
	}

	fmt.Fprintln(os.Stderr, err.Error())

	return 1
}

func rawForwarderForRemote(identity string) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprint(os.Stderr, err.Error())
		os.Exit(1)
	}

	sockPath, err := graft.DaemonSocketPathForRemote(homeDir, identity)
	if err != nil {
		fmt.Fprint(os.Stderr, err.Error())
		os.Exit(1)
	}

	conn, err := net.Dial("unix", sockPath) //nolint:noctx
	if err != nil {
		fmt.Fprint(os.Stderr, err.Error())
		os.Exit(1)
	}

	//nolint:printf // okay
	fmt.Fprintf(os.Stdout, "ACK")

	//nolint:errcheck
	go io.Copy(conn, os.Stdin)
	//nolint:errcheck
	io.Copy(os.Stdout, conn)
}
