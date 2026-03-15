package graft

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/edaniels/graft/errors"
)

const (
	// SignalTerminate is the canonical signal name used to terminate a process. This is distinct
	// from killing a process which typically doesn't give the process a chance to gracefully shutdown.
	SignalTerminate = "SIGTERM"
)

// DumpGoroutines prints all goroutine traces to stderr.
//
// Use GODEBUG=tracebackancestors=N to get ancestry information.
func DumpGoroutines() {
	buf := make([]byte, 1024)

	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			if _, err := os.Stderr.Write(buf[:n]); err != nil {
				fmt.Fprintf(os.Stderr, "error writing stack dump: %s", err.Error())
			}

			return
		}

		buf = make([]byte, 2*len(buf))
	}
}

// collectCommandsFromPATH searches each component of the current user's PATH and returns
// all executables.
// TODO(erd): this needs to be dynamic as the PATH changes based on user selected shell
// and corresponding profiles. Can probably just do a shell printenv PATH command.
func collectCommandsFromPATH() []string {
	shellVar, shellPathVar := os.LookupEnv("SHELL")
	if !shellPathVar {
		shellVar = defaultShellPath
	}

	printCmd := exec.Command(shellVar, "-c", "printenv PATH") //nolint:noctx

	pathVarBytes, err := printCmd.CombinedOutput()
	if err != nil {
		slog.Debug("error getting PATH", "error", err)

		return nil
	}

	pathVar := strings.TrimSpace(string(pathVarBytes))

	seen := map[string]bool{}

	var collected []string

	for pathElem := range strings.SplitSeq(pathVar, ":") {
		entries, err := os.ReadDir(pathElem)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				slog.Debug("error reading directory", "for", pathElem, "error", err)
			}

			continue
		}

		for _, entry := range entries {
			if entry.Type().IsDir() ||
				seen[entry.Name()] {
				continue
			}

			if info, err := entry.Info(); err != nil || info.Mode()&0o111 == 0 {
				// file not executable
				continue
			}

			seen[entry.Name()] = true

			collected = append(collected, filepath.Join(pathElem, entry.Name()))
		}
	}

	return collected
}
