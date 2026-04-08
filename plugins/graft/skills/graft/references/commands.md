# graft commands reference

Full per-command reference for `graft`. For the high-level cheat sheet, see
`SKILL.md`. Commands marked **internal** are invoked by graft's shell hooks
and PATH shims; do not call them directly.

## Connection lifecycle

### `graft connect [flags] <local_dir> <destination>[:<remote_dir>]`

Establish a new SSH or Docker connection. With zero positional args, reads
`graft.yaml` from the current directory.

Destination forms:
- `user@host` or `ssh://user@host` - SSH (default)
- `docker://image[:tag]` - Docker container

Flags:
| flag | meaning |
| --- | --- |
| `-n, --name <name>` | Connection name (defaults to host) |
| `--sync` | Enable bidirectional file synchronization |
| `--forward <arg>` | Repeatable. Adds a command shim or port forward; graft auto-partitions by spec shape |
| `--forward-prefix` | Prefix shimmed commands with the connection name (e.g. `python3` -> `conn1-python3`) |
| `--background` | Exclude this connection from CWD-based auto-selection |
| `--os <name>` | Container OS hint (`docker://` only) |

Two equivalent paths produce the same connection: an ad-hoc `graft connect`
with flags, or `graft connect` with no args reading a `graft.yaml`. Both go
through the same `ConnectParams` plumbing.

### `graft disconnect <connection>`

Tear down a connection and remove it from daemon state. Destructive; confirm
with the user before running.

### `graft connection set-root <connection> <local_dir> [remote_dir]`

Reassign a connection's local (and optionally remote) root directory after
the fact. Useful when a project moves on disk.

### `graft connection available-commands [--to <connection>]`

List all commands the remote knows about. Useful for debugging shim
availability and figuring out what's safe to add to `--forward`.

## Running things on the remote

### `graft run [-t/--to <connection>] [-m/--match <pattern>] <command> [args...]`

Run a command on a remote connection. Connection selection follows the
hierarchy (explicit `--to` -> session pin -> CWD-based).

- `--to <conn>` runs against a specific connection.
- `--match <pattern>` runs against **all** connections whose names match
  the pattern. The pattern is a shell glob (`path.Match` semantics, not
  regex), so use `build-*` not `^build-.*$`. Execution is parallel across
  matching connections, output lines are prefixed `[connection-name]`,
  stdin is broadcast to every target, and the overall exit code is the
  maximum exit code from any target.
- Use `--` to separate graft's flags from the command if there is any
  ambiguity.

### `graft shell [--to <connection>]`

Open an interactive remote shell with a TTY.

### `graft sync [--to <connection>] [--dest-dir <path>] [source]`

Trigger an immediate sync. Usually unnecessary - if a connection was created
with `--sync`, the daemon syncs continuously in the background. Use this when
you want a one-shot push of a specific source.

### `graft logs <connection>`

Export connection-scoped logs (sync, command execution, daemon
communication).

### `graft lsp <remote-executable>`

Run a remote language server with local-side I/O. Marked experimental in the
proto definition; falls back to local execution if the daemon is unavailable.

## Forwarding

### `graft forward [--to <connection>] [--prefix] <command|port>...`

Add forwards to a connection. Each argument is auto-classified:
- looks like a port spec -> port forward
- otherwise -> command shim (added to PATH)

Port spec grammar: `[local_port:]remote_port[/protocol]`
- `8080` - remote 8080 to local 8080 (tcp)
- `3000:8080` - remote 8080 to local 3000
- `5432/tcp` - explicit protocol
- `5353/udp` - UDP forward
- `3000:8080/udp` - full form

`--prefix` makes shimmed commands prefix-named to avoid clashes.

### `graft forward list [--to <connection>]`

Show every shimmed command and every active port forward
(auto-detected and explicit) for the connection.

### `graft forward remove [--to <connection>] <command|port>...`

Remove a forward. Removing an **auto-detected** port forward emits a
warning: it will start forwarding again as soon as the remote process listens
on it. To stop auto-detection entirely, the user has to stop the remote
process.

### `graft forward which <command>`

Show which connection a forwarded command currently resolves to. Rejects
port specs (port forwards are connection-wide, not command-shaped).

## Session and selection

### `graft use [--clear] [connection]`

Pin the current shell session to a connection. With `--clear`, removes the
pin and resumes CWD-based auto-selection.

### `graft current`

Print the connection that's currently active for this session, following the
selection hierarchy.

### `graft status [--json] [-w/--watch]`

Show all connections with state, sync status, and port forward status.
- `--json` emits the structured `ListConnectionsResponse`. See
  `references/status-json-shape.md`.
- `--watch` redraws once per second.

## Diagnostics

### `graft doctor [destination]`

Run the full diagnostic suite. Local-only by default; pass a destination to
also run the remote checks. All checks are read-only. Returns non-zero if any
check fails. See `references/doctor-playbook.md` for the full check list.

### `graft ping`

Round-trip ping to the local daemon, repeating once per second until
interrupted. Confirms the daemon is reachable.

### `graft env`

Print resolved `GRAFT_CONFIG_HOME` and `GRAFT_STATE_HOME` paths.

## Daemon and service management

### `graft daemon`

Run the daemon in the foreground. Flags:
- `--replace` - kill any existing daemon and take over
- `-d, --detach` - run in background

Most users do not run this directly; use `graft daemon service install`.

### `graft daemon logs`

Print recent daemon logs.

### `graft daemon stop`

Stop the running daemon. Refuses if the daemon is service-managed (use
`graft daemon service stop` instead).

### `graft daemon restart`

Restart the running daemon.

### `graft daemon status [--connection <name>]`

Show daemon health, version, uptime, and recent logs.

### `graft daemon service install`

Install graft as a system service (launchd on macOS, systemd on Linux) so
the daemon starts automatically on login. Refuses if a daemon is already
running.

### `graft daemon service uninstall|status|start|stop`

Standard service management commands.

## Setup helpers

### `graft init [--workspace] [flags] [<local_dir> <destination>[:<remote_dir>]]`

Generate a `graft.yaml` from flags. Two modes:

- Workspace mode: `graft init --workspace` (no positional args). Creates a
  workspace root config with `syncWorkspace: true`.
- Project mode: `graft init <local_dir> <destination> --name <name> [flags]`.
  Creates a project config with one destination.

Flags:
| flag | meaning |
| --- | --- |
| `--workspace` | Workspace mode |
| `-n, --name <name>` | Connection name (required in project mode) |
| `--sync` | Enable file synchronization |
| `--forward <cmd>` | Forwarded commands (repeatable) |
| `--forward-prefix` | Prefix forwarded commands with connection name |
| `--force` | Overwrite an existing `graft.yaml` |

### `graft activate <bash|zsh|fish>`

Print the shell activation script. Users typically `eval` this in their
shell rc file. The script:

- Sets `GRAFT_SESSION` to the shell PID
- Adds the shimmed-commands directory to `PATH`
- Aliases `gr` to `graft` for interactive use
- Installs preexec/precmd hooks that report CWD changes to the daemon
- Adds the connection name to `PS1` when one is active

### `graft update`

Self-update graft to the latest release.

## Internal commands (do not call directly)

These are invoked by the shell hooks and PATH shims installed via
`graft activate`. Listed only so you recognize them in process trees:

- `graft report-cwd` - shell hook reports working directory to the daemon
- `graft run-shimmed-cmd` - PATH shim entry point that forwards a command
- `graft raw` - raw stdin/stdout forwarder used by remote daemon transport
