package graft

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/creack/pty"

	"github.com/edaniels/graft/errors"
)

// A LocalRunningCommand is a command running natively on a daemon that started by some client's request.
// Each input/output stream is directly tied to a native process unless redirected. Out-of-band events like
// signal, env var changes, and window changes, are pushed directly via syscalls to the underlying pty.
//
// Compared to a [RemoteRunningCommand], you won't see any background processing done here because that's
// simply handled by the process running itself and the input/output streams being handled by the OS.
type LocalRunningCommand struct {
	execCmd *exec.Cmd
	stdin   io.WriteCloser
	stdoutR io.Reader
	stdoutW io.Closer
	stderrR io.Reader
	stderrW io.Closer

	isPty       bool
	childCloser func() error // any child process cleanup (like tty slave close on parents end)
}

// NewLocalRunningCommand returns and starts processing a running command originating from this host.
//
// See [ExecuteLocalCommand] for creating the [exec.Cmd].
func NewLocalRunningCommand(
	execCmd *exec.Cmd,
	stdin io.WriteCloser, // possibly a pty (https://en.wikipedia.org/wiki/Pseudoterminal)
	stdoutR io.Reader,
	stdoutW io.Closer,
	stderrR io.Reader,
	stderrW io.Closer,
	isPty bool,
	closer func() error,
) *LocalRunningCommand {
	return &LocalRunningCommand{
		execCmd:     execCmd,
		stdin:       stdin,
		stdoutR:     stdoutR,
		stdoutW:     stdoutW,
		stderrR:     stderrR,
		stderrW:     stderrW,
		isPty:       isPty,
		childCloser: closer,
	}
}

func (rc *LocalRunningCommand) Stdin() io.WriteCloser {
	if rc.isPty {
		// Closing the pty closes it from both sides which is bad for reading output
		// and there's no stdin to close really, that's signaled by the raw interpretation
		// of the program running.
		return noopWriteCloser{rc.stdin}
	}

	return rc.stdin
}

func (rc *LocalRunningCommand) Stdout() io.Reader {
	return rc.stdoutR
}

func (rc *LocalRunningCommand) Stderr() io.Reader {
	return rc.stderrR
}

func (rc *LocalRunningCommand) Signal(sig string) error {
	var sigNum syscall.Signal

	switch sig {
	case SignalTerminate:
		sigNum = syscall.SIGTERM
	default:
		return errors.WrapSuffix(errUnknownSignal, sig)
	}

	if err := syscall.Kill(rc.execCmd.Process.Pid, sigNum); err != nil {
		return errors.WrapPrefix(err, "error sending kill signal via syscall")
	}

	return nil
}

// PID returns the process ID of the running command.
func (rc *LocalRunningCommand) PID() int {
	return rc.execCmd.Process.Pid
}

// Wait simply waits on the underlying process to finish, cleans up any
// resources it created after waiting, and returns either the exit status or an unexpected error.
func (rc *LocalRunningCommand) Wait() (int, error) {
	defer errors.UncheckedFunc(rc.childCloser)

	state, err := rc.execCmd.Process.Wait()
	if err != nil {
		return -1, errors.Wrap(err)
	}

	return state.ExitCode(), nil
}

// Release cleans up the stdout/err pipes/ptys which is important to do after Wait
// since a pty may have buffered data that if we were to close it during wait, may
// be racily discarded by readers of the command.
func (rc *LocalRunningCommand) Release() {
	defer rc.stdoutW.Close()
	defer rc.stderrW.Close()
}

func (rc *LocalRunningCommand) SetEnvVar(_, _ string) error {
	return errors.New("cannot set env var after start")
}

func (rc *LocalRunningCommand) NotifyWindowChange(h, w int) error {
	if !rc.isPty {
		return errors.New("not a pseudoterminal")
	}

	ptyFile, ok := rc.stdin.(*os.File)
	if !ok {
		return errors.Errorf("not a pseudoterminal but a %T", ptyFile)
	}

	if err := pty.Setsize(ptyFile, &pty.Winsize{
		Cols: uint16(w), //nolint:gosec // overflow okay
		Rows: uint16(h), //nolint:gosec // overflow okay
	}); err != nil {
		return errors.WrapPrefix(err, "error notifying window change to pty")
	}

	return nil
}

// ExecuteLocalCommand prepares an exec.Cmd based on the given input to be executed in an interactive
// terminal and processed by a LocalRunningCommand.
//
//nolint:gocognit // disagree
func ExecuteLocalCommand(
	ctx context.Context,
	command []string,
	allocatePty bool,
	redirectStdout bool,
	redirectStderr bool,
	extraEnv ...string,
) (*LocalRunningCommand, error) {
	slog.DebugContext(ctx, "starting command", "command", command)
	execCmd := exec.Command(command[0], command[1:]...) //nolint:gosec,noctx // assumed authorized

	execCmd.Env = append(os.Environ(), "_GRAFT_SPAWNED=true")

	if allocatePty {
		execCmd.Env = append(execCmd.Env, "TERM=xterm-256color")
	}

	execCmd.Env = append(execCmd.Env, extraEnv...)

	if runtime.GOOS == osDarwin && redirectStdout && redirectStderr {
		// We need at least one of the outputs to be the pty FD so that
		// the process can close it and terminate. This only happens on macOS
		// so far and I haven't bothered to figure it out because it feels
		// pretty hard to debug. Either way the process turns into
		// E: "the process is trying to exit" state when this happens.
		return nil, errors.New("cannot do double redirection on macOS")
	}

	var (
		stdoutR, stderrR io.Reader
		stdoutW, stderrW io.Closer
	)

	if redirectStdout {
		stdoutPipeR, stdoutPipeW, err := os.Pipe()
		if err != nil {
			return nil, errors.WrapPrefix(err, "error creating stdout pipe")
		}

		execCmd.Stdout = stdoutPipeW
		stdoutR = stdoutPipeR
		stdoutW = stdoutPipeW
	}

	if redirectStderr {
		stderrPipeR, stderrPipeW, err := os.Pipe()
		if err != nil {
			return nil, errors.WrapPrefix(err, "error creating stderr pipe")
		}

		execCmd.Stderr = stderrPipeW
		stderrR = stderrPipeR
		stderrW = stderrPipeW
	}

	var stdin io.WriteCloser

	childCloser := func() error { return nil }

	if allocatePty {
		ptyFile, ttyFile, err := pty.Open()
		if err != nil {
			return nil, errors.WrapPrefix(err, "error opening pty")
		}
		// if err := syscall.SetNonblock(int(ptyFile.Fd()), true); err != nil {
		// 	return nil, errors.WrapPrefix(err, "error setting pty to nonblocking")
		// }
		if err := pty.Setsize(ptyFile, &pty.Winsize{
			X: 40,
			Y: 80,
		}); err != nil {
			_ = ptyFile.Close()
			_ = ttyFile.Close()

			return nil, errors.WrapPrefix(err, "error setting pty window size")
		}

		childCloser = ttyFile.Close

		if execCmd.Stdout == nil {
			execCmd.Stdout = ttyFile
		}

		if execCmd.Stderr == nil {
			execCmd.Stderr = ttyFile
		}

		if execCmd.Stdin == nil {
			execCmd.Stdin = ttyFile
		}

		execCmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
		}

		if err := execCmd.Start(); err != nil {
			_ = ptyFile.Close()
			_ = ttyFile.Close()

			return nil, errors.WrapPrefix(err, "error starting command")
		}

		// Close parent's copy of redirect pipe write ends so readers get
		// natural EOF when the child exits.
		if redirectStdout {
			stdoutW.Close()
			stdoutW = noopWriteCloser{}
		}

		if redirectStderr {
			stderrW.Close()
			stderrW = noopWriteCloser{}
		}

		stdin = ptyFile

		// Check this before stdout since we rely on the value of stdout.
		if stderrR == nil {
			if stdoutR == nil {
				// In this case, stdout is coming from one pty and there's no distinction between the two streams.
				// Note(erd): 99% sure this case is the standard interactive app. There's no point to distinguish the two.
				stderrR = io.NopCloser(bytes.NewReader(nil))
				stderrW = noopWriteCloser{}
			} else {
				stderrR = ptyFile
				stderrW = ptyFile
			}
		}

		if stdoutR == nil {
			stdoutR = ptyFile
			stdoutW = ptyFile
		}
	} else {
		stdinPipeW, err := execCmd.StdinPipe()
		if err != nil {
			return nil, errors.Wrap(err)
		}

		stdin = stdinPipeW

		if stderrR == nil {
			stderrPipeR, err := execCmd.StderrPipe()
			if err != nil {
				return nil, errors.Wrap(err)
			}

			stderrR = stderrPipeR
			stderrW = noopWriteCloser{}
		}

		if stdoutR == nil {
			stdoutPipeR, err := execCmd.StdoutPipe()
			if err != nil {
				return nil, errors.Wrap(err)
			}

			stdoutR = stdoutPipeR
			stdoutW = noopWriteCloser{}
		}

		if err := execCmd.Start(); err != nil {
			return nil, errors.WrapPrefix(err, "error starting command")
		}

		// Close parent's copy of redirect pipe write ends so readers get
		// natural EOF when the child exits.
		if redirectStdout {
			stdoutW.Close()
			stdoutW = noopWriteCloser{}
		}

		if redirectStderr {
			stderrW.Close()
			stderrW = noopWriteCloser{}
		}
	}

	return NewLocalRunningCommand(
		execCmd,
		stdin,
		stdoutR,
		stdoutW,
		stderrR,
		stderrW,
		allocatePty,
		childCloser,
	), nil
}

// ListeningPort represents a port being listened on by a process.
type ListeningPort struct {
	Port     int
	Host     string
	Protocol string // "tcp" or "udp"
	PID      int
	Inode    uint64
}

// parseHexIP converts a hex-encoded IP address from /proc/net/{tcp,udp}{,6} to a string.
// IPv4 addresses are 8 hex chars (4 bytes in host byte order).
// IPv6 addresses are 32 hex chars (16 bytes, 4 groups of 4 bytes each in host byte order).
//
// Note: This assumes little-endian host byte order (x86_64, arm64). All platforms this code
// runs on (Linux remote daemons) are little-endian. If Go's /proc parsing were ever needed on
// big-endian, the byte reversal here would need to be conditional.
func parseHexIP(hexStr string) string {
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return hexStr
	}

	switch len(b) {
	case 4:
		// IPv4: stored in host byte order (little-endian on x86), so bytes are reversed.
		return net.IP{b[3], b[2], b[1], b[0]}.String()
	case 16:
		// IPv6: 4 groups of 4 bytes, each group in host byte order.
		ip := make(net.IP, 16)

		for i := range 4 {
			ip[i*4+0] = b[i*4+3]
			ip[i*4+1] = b[i*4+2]
			ip[i*4+2] = b[i*4+1]
			ip[i*4+3] = b[i*4+0]
		}

		return ip.String()
	default:
		return hexStr
	}
}

// parseProcNetEntries parses the content of a /proc/net/{tcp,udp}{,6} file and returns
// listening ports matching the given state.
func parseProcNetEntries(content, protocol, state string) []ListeningPort {
	lines := strings.Split(content, "\n")
	ports := make([]ListeningPort, 0, len(lines))

	for _, line := range lines[1:] { // skip header
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}

		if fields[3] != state {
			continue
		}
		// field 1 is local_address (hex_ip:hex_port)
		parts := strings.Split(fields[1], ":")
		if len(parts) != 2 {
			continue
		}

		port, err := strconv.ParseInt(parts[1], 16, 32)
		if err != nil {
			continue
		}

		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}

		ports = append(ports, ListeningPort{
			Port:     int(port),
			Host:     parseHexIP(parts[0]),
			Protocol: protocol,
			Inode:    inode,
		})
	}

	return ports
}

// findShellPath finds the most relevant set SHELL for the current user.
func findShellPath() (string, error) {
	shellPath, ok := os.LookupEnv("SHELL")
	if ok {
		return shellPath, nil
	}

	currentUser, err := user.Current()
	if err != nil {
		return "", errors.WrapPrefix(err, "error determining current user")
	}

	passwdRd, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "", errors.WrapPrefix(err, "error reading /etc/passwd")
	}

	// Match user to the "shell" (login command)
	/*
		root:x:0:0:root:/root:/bin/bash
		daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
		bin:x:2:2:bin:/bin:/usr/sbin/nologin
		sys:x:3:3:sys:/dev:/usr/sbin/nologin
		sync:x:4:65534:sync:/bin:/bin/sync
		games:x:5:60:games:/usr/games:/usr/sbin/nologin
		man:x:6:12:man:/var/cache/man:/usr/sbin/nologin
		lp:x:7:7:lp:/var/spool/lpd:/usr/sbin/nologin
		mail:x:8:8:mail:/var/mail:/usr/sbin/nologin
		news:x:9:9:news:/var/spool/news:/usr/sbin/nologin
		uucp:x:10:10:uucp:/var/spool/uucp:/usr/sbin/nologin
		proxy:x:13:13:proxy:/bin:/usr/sbin/nologin
		www-data:x:33:33:www-data:/var/www:/usr/sbin/nologin
		backup:x:34:34:backup:/var/backups:/usr/sbin/nologin
		list:x:38:38:Mailing List Manager:/var/list:/usr/sbin/nologin
		irc:x:39:39:ircd:/run/ircd:/usr/sbin/nologin
		_apt:x:42:65534::/nonexistent:/usr/sbin/nologin
		nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin
	*/

	scanner := bufio.NewScanner(bytes.NewReader(passwdRd))
	for scanner.Scan() {
		lineSplit := strings.Split(scanner.Text(), ":")
		if len(lineSplit) != 7 {
			// invalid format
			continue
		}

		if lineSplit[0] != currentUser.Username {
			continue
		}

		shellPath = lineSplit[6]

		break
	}

	if shellPath == "" {
		return "", errors.New("failed to find shell path")
	}

	return shellPath, nil
}

// makeShellCommand sets up a command to for running a shell starting in a CWD.
func makeShellCommand(shellPath string, cwd string) []string {
	cmd := []string{shellPath}

	if len(cwd) == 0 {
		return cmd
	}

	return append(cmd, "-c", fmt.Sprintf("cd %s && %s", cwd, shellPath))
}

// makeCommandWrappedInShell sets up a command to for running a command in a chosen shell starting in a CWD.
func makeCommandWrappedInShell(
	shellPath string,
	cwd string,
	rawCommand string,
	arguments []string,
	withSudo bool,
	shellHookPrefix string,
) []string {
	wrappedCmd := make([]string, 0, 1)
	if shellHookPrefix != "" {
		wrappedCmd = append(wrappedCmd, shellHookPrefix)
	}

	if len(cwd) != 0 {
		wrappedCmd = append(wrappedCmd, fmt.Sprintf("cd %s && ", cwd))
	}

	if withSudo {
		wrappedCmd = append(wrappedCmd, "sudo ")
	}

	wrappedCmd = append(wrappedCmd, rawCommand)

	// reconstruct the arguments into one string, adding quotes where needed
	for _, arg := range arguments {
		if strings.ContainsFunc(arg, unicode.IsSpace) {
			arg = fmt.Sprintf("%q", arg)
		}

		wrappedCmd = append(wrappedCmd, " "+arg)
	}

	// TODO(erd): flags based on type of shell?
	return []string{shellPath, "-c", strings.Join(wrappedCmd, "")}
}
