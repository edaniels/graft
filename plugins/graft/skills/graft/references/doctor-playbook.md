# `graft doctor` playbook

`graft doctor` runs a series of read-only checks against the local
environment and (optionally) a remote destination. Each check returns one of
`PASS`, `WARN`, or `FAIL`. The check definitions live in `pkg/doctor.go` and
the orchestration is in `cmd/graft/doctor.go`.

Run it as:

```bash
graft doctor               # local checks only
graft doctor user@host     # local + remote checks for that destination
```

## Local checks

### Shell activation

**What it checks** (`pkg/doctor.go:39-62`): looks for `GRAFT_SESSION` in the
environment and verifies it parses as a PID.

| status | meaning | fix |
| --- | --- | --- |
| PASS | `GRAFT_SESSION` is set and parseable | none |
| WARN | not set | user has not run `eval "$(graft activate <shell>)"` in this shell. Add it to `~/.zshrc` / `~/.bashrc` / `~/.config/fish/config.fish` and start a new shell |
| WARN | `GRAFT_SESSION=<garbage>` | the activation script is broken or some other tool clobbered the env var. Re-source the shell rc |

This is a WARN, not a FAIL: graft commands work without shell activation,
but CWD-based connection selection and command shimming will not.

### Local daemon

**What it checks** (`pkg/doctor.go:67-99`): dials the local daemon socket and
calls `Status`. Reports version and uptime.

| status | meaning | fix |
| --- | --- | --- |
| PASS | daemon running and responds to `Status` | none |
| FAIL | "not running (start with 'graft daemon')" | start the daemon: `graft daemon -d` for a one-off, or `graft daemon service install` to install as a system service |
| FAIL | other error | `graft daemon logs` for details. Common cause: stale socket file from a crashed daemon - `graft daemon --replace` will clear it |

### Updates

**What it checks** (`pkg/doctor.go:102-138`): queries the release endpoint
for a newer version.

| status | meaning | fix |
| --- | --- | --- |
| PASS | up to date | none |
| WARN | "update available: X -> Y" | `graft update` to upgrade |
| WARN | "dev build" | running a locally-built binary; version comparison skipped |
| WARN | "could not check for updates" | network or release endpoint issue. Not blocking |

## Remote checks (only when destination is passed)

### SSH connection

**What it checks** (`cmd/graft/doctor.go:126-197`): parses the destination,
resolves SSH config (hostname, port, user, identity files, proxy command),
creates an SSH connector, and calls `InitializeRemote`.

| status | meaning | fix |
| --- | --- | --- |
| PASS | "connected", with hostname/port/user/identity details | none |
| FAIL | "invalid destination" | check the destination format. Should be `user@host` or `ssh://user@host` |
| FAIL | "failed to create connector" or "failed to connect" | the details list shows what graft resolved. Common causes: wrong username, missing identity file, host key mismatch, ProxyCommand failure. Try `ssh <host>` directly to see the underlying error |

### Transport

**What it checks** (`pkg/doctor.go:172-210`): probes whether the SSH session
supports Unix domain socket forwarding (the preferred transport). Falls back
to stdio tunneling if not.

| status | meaning | fix |
| --- | --- | --- |
| PASS | "UDS (Unix domain socket forwarding supported)" | none. This is the fast path |
| WARN | "stdio (Unix domain socket forwarding not supported, will use stdio tunnel)" | graft will work but slower. Cause: the remote sshd has `AllowStreamLocalForwarding no` or the SSH version is too old. Fix on the remote: `AllowStreamLocalForwarding yes` in `/etc/ssh/sshd_config` and restart sshd |
| WARN | "unable to determine transport mode" | unexpected error during the probe. Check `graft daemon logs` |

### Remote environment

**What it checks** (`pkg/doctor.go:230-251`): runs discovery on the remote
to determine OS, architecture, and home directory.

| status | meaning | fix |
| --- | --- | --- |
| PASS | "linux/amd64" (or similar) | none |
| FAIL | discovery error | the SSH connection works but graft cannot run basic commands on the remote. Check that `uname`, `echo $HOME`, etc. work over `ssh <host>` |

Currently graft only supports Linux remote targets. macOS as a remote target
is not supported per the README.

### Remote daemon

**What it checks** (`pkg/doctor.go:253-308`): looks for the remote daemon
binary and tries to connect to it.

| status | meaning | fix |
| --- | --- | --- |
| PASS | "running (vX.Y.Z, matches local)" | none |
| WARN | "not installed (binary not found at <path>)" | first time connecting to this remote. The next `graft connect` will copy the binary across automatically |
| WARN | "not running (will be started on next 'graft connect')" | binary is there but daemon is not currently up. The next `graft connect` will start it |
| WARN | "running, version mismatch with local" | the remote daemon was installed by an older or newer graft. Reconnect to push the matching binary, or run `graft update` locally |

### Remote directories

**What it checks** (`pkg/doctor.go:310-337`): reports the resolved paths for
state, config, logs, socket, and binary on the remote. Always passes; this
is informational only.

Use this output if you need to inspect remote state files manually.

## Failure-to-action ladder

When something is broken, walk these in order. Stop at the first one that
explains the problem:

1. `graft status --json` - is the connection in `CONNECTION_STATE_CONNECTED`?
   Read `state_reason`, sync `last_error`, and port `conflict_reason` first.
2. `graft doctor` (local) - shell activation, daemon, version sanity.
3. `graft doctor user@host` (remote) - SSH, transport, remote daemon.
4. `graft daemon logs` - recent local daemon log lines.
5. `graft ping` - bare round-trip; confirms the daemon is even reachable.
6. `graft daemon restart` - last resort. Confirm with the user before
   running, since it tears down all in-flight commands and syncs.
