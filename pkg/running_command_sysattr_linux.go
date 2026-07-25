package graft

import "syscall"

// commandSysProcAttr returns the process attributes for a spawned user
// command. Every command becomes a process group leader (a full session
// leader with a controlling tty for pty commands) so signals can address the
// whole group. Pdeathsig is a backstop that hangs up commands if the daemon
// dies without running its shutdown path; commands are not expected to
// survive the daemon itself, only client disconnects.
func commandSysProcAttr(pty bool) *syscall.SysProcAttr {
	if pty {
		return &syscall.SysProcAttr{
			Setsid:    true,
			Setctty:   true,
			Pdeathsig: syscall.SIGHUP,
		}
	}

	return &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGHUP,
	}
}
