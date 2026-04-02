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
// all executables. extraPathDirs are additional directories (e.g. from env managers like
// mise) to scan beyond the shell's PATH.
// TODO(erd): this needs to be dynamic as the PATH changes based on user selected shell
// and corresponding profiles. Can probably just do a shell printenv PATH command.
func collectCommandsFromPATH(extraPathDirs ...string) []string {
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

	// Scan extra PATH dirs first so env-manager-provided commands appear.
	for _, pathElem := range extraPathDirs {
		collectExecutablesFromDir(pathElem, seen, &collected)
	}

	for pathElem := range strings.SplitSeq(pathVar, ":") {
		collectExecutablesFromDir(pathElem, seen, &collected)
	}

	return collected
}

// collectExecutablesFromDir scans a directory for executable files and appends them
// to collected, skipping names already in seen.
func collectExecutablesFromDir(dir string, seen map[string]bool, collected *[]string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("error reading directory", "for", dir, "error", err)
		}

		return
	}

	for _, entry := range entries {
		if entry.Type().IsDir() ||
			seen[entry.Name()] {
			continue
		}

		if info, err := entry.Info(); err != nil || info.Mode()&0o111 == 0 {
			continue
		}

		seen[entry.Name()] = true

		*collected = append(*collected, filepath.Join(dir, entry.Name()))
	}
}
