package graft

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

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

// Interactive shell PATH refresh state. refreshPATHFromShellInit periodically
// re-captures the interactive shell's PATH (sourcing ~/.bashrc or ~/.zshrc) so
// modifications installers make after daemon startup become visible.
var (
	freshPATHMu    sync.Mutex
	freshPATHValue string
	freshPATHAt    time.Time
)

const (
	freshPATHTTL            = 5 * time.Second
	freshPATHCaptureTimeout = 10 * time.Second
	freshPATHStartMarker    = "__GRAFT_PATH_START__"
	freshPATHEndMarker      = "__GRAFT_PATH_END__"
)

// refreshPATHShellRunner is the function used by refreshPATHFromShellInit
// to invoke the user's shell with -ic. Tests may replace this to stub the subshell.
var refreshPATHShellRunner = defaultRefreshPATHShellRunner

func defaultRefreshPATHShellRunner(ctx context.Context, shell string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Stderr = io.Discard

	out, err := cmd.Output()
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return out, nil
}

// refreshPATHFromShellInit runs an interactive (non-login) subshell so
// that modifications made to the user's shell init files after the daemon
// started are picked up. The captured PATH is written back to the daemon's own
// $PATH so subsequent child processes inherit it.
//
// Goal: behave like opening a fresh terminal tab. `-i` causes bash/zsh to
// source ~/.bashrc / ~/.zshrc respectively, and we additionally source the
// user-level profile files (~/.profile, ~/.bash_profile, ~/.bash_login for
// bash; ~/.zprofile, ~/.zlogin for zsh) inside the -c script. We deliberately
// do NOT use `-l` (login shell), which would source /etc/profile and
// /etc/profile.d/*; those can clobber the daemon's inherited env on some
// systems (e.g. Debian's /etc/profile rewrites PATH from scratch).
//
// The subshell inherits the daemon's current env, so vars the daemon already
// had survive unless one of the user-level files overwrites them.
//
// Uses markers to isolate the PATH line from any noise .bashrc / .profile
// writes to stdout (nvm/conda/asdf banners, etc.). Discards stderr so
// interactive-bash job control warnings in non-TTY contexts don't leak. On any
// error, leaves $PATH unchanged and respects the TTL before retrying.
func refreshPATHFromShellInit() {
	freshPATHMu.Lock()
	defer freshPATHMu.Unlock()

	if !freshPATHAt.IsZero() && time.Since(freshPATHAt) < freshPATHTTL {
		return
	}
	// Set the timestamp early so failures also respect the TTL and we don't
	// hammer the subshell on every tick.
	freshPATHAt = time.Now()

	// Prefer $SHELL, but fall back to the user's login shell from /etc/passwd
	// if it's unset which is common in container environments where the daemon
	// process inherits an empty SHELL.
	shellVar, err := findShellPath()
	if err != nil {
		slog.Debug("refreshPATHFromShellInit: no shell available", "error", err)

		return
	}

	base := filepath.Base(shellVar)

	var sourceProfile string

	switch base {
	case "bash":
		// `bash -i` already sources ~/.bashrc. Add the user-level profile files
		// (which a login shell would source) so installers that write to
		// .profile / .bash_profile are also picked up. Skip /etc/profile and
		// /etc/profile.d/*; those can clobber inherited PATH on some systems
		// (Debian, Ubuntu) which is why we don't simply use `bash -l`.
		sourceProfile = `{ ` +
			`[ -f ~/.profile ] && . ~/.profile; ` +
			`[ -f ~/.bash_profile ] && . ~/.bash_profile; ` +
			`[ -f ~/.bash_login ] && . ~/.bash_login; ` +
			`} >/dev/null 2>&1; `
	case "zsh":
		// `zsh -i` already sources ~/.zshenv (always) and ~/.zshrc. Add the
		// login-only files so .zprofile / .zlogin contributions show up too.
		sourceProfile = `{ ` +
			`[ -f ~/.zprofile ] && . ~/.zprofile; ` +
			`[ -f ~/.zlogin ] && . ~/.zlogin; ` +
			`} >/dev/null 2>&1; `
	default:
		// Other shells (sh, fish, nu, ...) either don't use -ic the same way or
		// wouldn't source .bashrc/.zshrc; skip and leave the daemon's PATH alone.
		return
	}

	script := fmt.Sprintf("%sprintf '%%s\\n' '%s'; printenv PATH; printf '%%s\\n' '%s'",
		sourceProfile, freshPATHStartMarker, freshPATHEndMarker)

	ctx, cancel := context.WithTimeout(context.Background(), freshPATHCaptureTimeout)
	defer cancel()

	out, err := refreshPATHShellRunner(ctx, shellVar, "-ic", script)
	if err != nil {
		slog.Debug("refreshPATHFromShellInit: shell exec failed", "error", err)

		return
	}

	newPath, ok := parsePATHBetweenMarkers(out)
	if !ok {
		slog.Debug("refreshPATHFromShellInit: markers not found in shell output")

		return
	}

	if newPath == "" {
		return
	}

	newPath = dedupePATH(newPath)
	if newPath == freshPATHValue {
		return
	}

	freshPATHValue = newPath
	if err := os.Setenv("PATH", newPath); err != nil {
		slog.Debug("refreshPATHFromShellInit: os.Setenv failed", "error", err)
	}
}

// parsePATHBetweenMarkers extracts the PATH text between the fresh PATH start
// and end markers in the given output, tolerating arbitrary noise before the
// start marker (e.g. banners written by .bashrc). Returns the trimmed text and
// true if both markers are found.
func parsePATHBetweenMarkers(out []byte) (string, bool) {
	text := string(out)

	startIdx := strings.Index(text, freshPATHStartMarker)
	if startIdx < 0 {
		return "", false
	}

	text = text[startIdx+len(freshPATHStartMarker):]

	before, _, ok := strings.Cut(text, freshPATHEndMarker)
	if !ok {
		return "", false
	}

	return strings.TrimSpace(before), true
}

// dedupePATH removes duplicate and empty entries from a colon-separated PATH,
// preserving the order of first occurrence. Used to keep the captured PATH
// stable in the face of non-idempotent .bashrc files that unconditionally
// prepend to PATH on every source.
func dedupePATH(path string) string {
	parts := strings.Split(path, ":")
	seen := make(map[string]struct{}, len(parts))

	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}

		if _, dup := seen[p]; dup {
			continue
		}

		seen[p] = struct{}{}

		kept = append(kept, p)
	}

	return strings.Join(kept, ":")
}

// collectCommandsFromPATH searches each component of the current user's PATH and returns
// all executables. extraPathDirs are additional directories (e.g. from env managers like
// mise) to scan beyond the shell's PATH.
func collectCommandsFromPATH(additionalSearchPaths map[string][]string) ([]string, map[string][]string) {
	// Re-source the interactive shell rc files (~/.bashrc / ~/.zshrc) if the
	// TTL has expired. This lets newly-installed tools whose installers mutate
	// shell init files become visible without daemon restart.
	refreshPATHFromShellInit()

	shellVar, shellPathVar := os.LookupEnv("SHELL")
	if !shellPathVar {
		shellVar = defaultShellPath
	}

	printCmd := exec.Command(shellVar, "-c", "printenv PATH") //nolint:noctx

	pathVarBytes, err := printCmd.CombinedOutput()
	if err != nil {
		slog.Debug("error getting PATH", "error", err)

		return nil, nil
	}

	commandsByDirectory := make(map[string][]string, len(additionalSearchPaths))
	for srcDir, paths := range additionalSearchPaths {
		commandsByDirectory[srcDir] = collectExecutablesFromDirs(paths)
	}

	pathVar := strings.TrimSpace(string(pathVarBytes))

	return collectExecutablesFromDirs(strings.Split(pathVar, ":")), commandsByDirectory
}

// collectExecutablesFromDir scans a directory for executable files and appends them
// to collected, skipping names already in seen.
func collectExecutablesFromDirs(dirs []string) []string {
	seen := map[string]bool{}

	var collected []string

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				slog.Debug("error reading directory", "for", dir, "error", err)
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

			collected = append(collected, filepath.Join(dir, entry.Name()))
		}
	}

	return collected
}
