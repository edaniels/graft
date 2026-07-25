//go:build !linux

package graft

import "syscall"

// commandSysProcAttr returns the process attributes for a spawned user
// command. Every command becomes a process group leader (a full session
// leader with a controlling tty for pty commands) so signals can address the
// whole group. Pdeathsig is unavailable off Linux; the startup reconcile pass
// cleans up anything a crashed daemon leaves behind.
func commandSysProcAttr(pty bool) *syscall.SysProcAttr {
	if pty {
		return &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
		}
	}

	return &syscall.SysProcAttr{
		Setpgid: true,
	}
}
