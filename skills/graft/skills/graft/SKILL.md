---
name: graft
description: This skill should be used when working in a directory managed by graft (a local-first remote development tool that makes remote files and commands feel local). Use it when the user mentions "graft", "remote machine", "build server", "sync to remote", "forward port", when GRAFT_SESSION or GRAFT_CONNECTION environment variables are set, or when a graft.yaml file is present in or above the working directory. Covers running commands on remote connections, syncing files, forwarding ports, setting up new connections (with flags or graft.yaml), reading graft status, and diagnosing issues with graft doctor.
---

# graft

`graft` is a local-first remote development tool. With shell activation in
place, you `cd` into a directory and graft transparently routes commands,
files, and ports to the right remote machine. Because Claude Code's Bash tool
runs in that same shell, graft is already in effect for any command you run -
this skill exists so Claude knows when graft is active, which graft commands
to reach for, and how to read graft's structured output.

## When this skill is in effect

Graft is active for the current session if any of these are true:

```bash
# any of these signals graft is set up
[ -n "$GRAFT_SESSION" ] || [ -n "$GRAFT_CONNECTION" ]
command -v graft >/dev/null
# or there is a graft.yaml in CWD or any parent directory
```

If none of these hold, do not use graft commands - this skill does not apply.

**Shell activation is a hard prerequisite for command shimming.** `GRAFT_SESSION`
is set by `eval "$(graft activate <shell>)"`, and only inside a shell that
sourced that activation will shimmed commands like `make` or `python3` be
forwarded to the remote automatically. Without activation, `graft run`,
`graft shell`, `graft sync`, `graft forward`, and the status/doctor family
still work - but bare commands like `make build` will execute locally even
when a connection is forwarding `make`. If `GRAFT_SESSION` is unset, reach
for `graft run -- <cmd>` explicitly rather than trusting PATH.

## Mental model

- A **local daemon** runs on the user's machine and a **remote daemon** runs
  on each connected target. They talk over gRPC.
- A **connection** binds a local directory (`localRoot`) to a remote
  destination (SSH or Docker), optionally with file sync and command/port
  forwarding.
- **Connection selection** for any graft command follows a hierarchy:
  1. explicit `--to <conn>`
  2. session pin set with `graft use <conn>`
  3. CWD-based: graft picks the connection whose `localRoot` is an ancestor
     of the current directory
- **Command shimming**: when a connection forwards `make`, a `make` shim is
  injected into PATH. Running `make build` in that directory transparently
  runs on the remote.
- **File sync** is bidirectional via mutagen, respects `.gitignore`, and
  reports conflicts/problems in status output.
- **Port forwarding** has two flavors:
  - **Auto-detected**: graft watches remote listening ports and forwards
    them automatically. Most of the time you don't need to declare anything.
  - **Explicit**: declared at connect time (`--forward 8080`), afterwards
    (`graft forward 8080`), or in `graft.yaml`. Explicit forwards persist
    even if the remote process restarts.

## Setting up a connection - two paths, both first-class

Pick whichever fits the conversation. Neither is "more correct" than the
other - they produce identical connections under the hood.

### Path A: ad-hoc, no config file

Use this when the user wants a one-shot connection or doesn't want a config
file committed to the repo.

```bash
graft connect <local_dir> <user@host>[:<remote_dir>] [flags]
```

Flags:

| flag | purpose |
| --- | --- |
| `--name <n>` | name the connection (defaults to host) |
| `--sync` | enable bidirectional file sync |
| `--forward <arg>` | forward a command OR a port (repeat the flag, or comma-separate). graft auto-partitions: anything matching a port spec becomes a port forward, everything else is a command shim |
| `--forward-prefix` | prefix shimmed commands with the connection name to avoid collisions |
| `--background` | exclude from CWD-based auto-selection (use with `--to` or `graft use`) |
| `--os <name>` | docker only: container OS |

Destination forms:

- `user@host` or `ssh://user@host` - SSH (default)
- `docker://image[:tag]` - Docker container

Example mixing commands and ports in a single `--forward`:

```bash
graft connect . ec2-user@build-box:~/work \
  --sync \
  --forward make --forward bazel \
  --forward 8080 --forward 5432:5432/tcp \
  --name build
```

### Path B: project / workspace `graft.yaml`

Use this when the connection is stable and shared across teammates or
shells. `graft init` generates one from flags. With a `graft.yaml` in CWD,
`graft connect` with no positional arguments reads it automatically.

Minimal project config (`graft.yaml`):

```yaml
version: v1
forward:
  - make
  - 8080
destinations:
  build:
    host: build-box
    user: ec2-user
    syncTo: ~/work
    sync: true
```

Workspace root config (parent dir, opts entire workspace into syncing):

```yaml
version: v1
workspace: true
defaults:
  syncWorkspace: true
```

See `references/config-schema.md` for the full schema. **Do not confuse this
with the daemon-internal state schema** - users hand-author the project
schema above, not the `connections[]` structure used by graft's persistent
state file.

## Command cheat sheet

| command | when to use it |
| --- | --- |
| `graft connect` | establish a connection (see paths above) |
| `graft disconnect <conn>` | tear down a connection (destructive - confirm with user first) |
| `graft run <cmd>` | run a command on the current connection. add `--to <conn>` to override CWD-based selection |
| `graft shell` | open an interactive remote shell |
| `graft sync` | trigger a sync (usually unnecessary - sync runs continuously when enabled) |
| `graft use <conn>` | pin the current shell to a connection. `graft use --clear` resumes CWD-based selection |
| `graft status` | human-readable status. add `--json` for parsing, `--watch` to follow |
| `graft doctor` | diagnose environment and connection issues - run this FIRST when something looks broken |
| `graft init` | scaffold a `graft.yaml` from flags |
| `graft connection available-commands` | list which commands graft knows how to forward on the remote |
| `graft connection set-root <conn> <local_dir> [remote_dir]` | reassign a connection's local (and optionally remote) root after the fact |
| `graft run --match <pattern> -- <cmd>` | run the same command across all connections whose names match the pattern (multi-target run) |

Note: shell activation installs `gr` as an alias for `graft`, so `gr run ...`
and `graft run ...` are equivalent. Prefer `graft` in scripts and docs;
`gr` is for interactive use.

The `forward` family deserves its own table:

| command | purpose |
| --- | --- |
| `graft forward <arg>...` | add forwards to the current connection. mixes commands and ports freely |
| `graft forward list` | show all currently-shimmed commands and active port forwards (auto-detected and explicit) |
| `graft forward remove <arg>...` | remove forwards. removing an auto-detected port emits a warning - it will start forwarding again as soon as the remote process listens |
| `graft forward which <command>` | show which connection a shimmed command resolves to (port forwards are connection-wide and rejected here) |

Port spec grammar: `[local_port:]remote_port[/protocol]`

- `8080` -> remote 8080 to local 8080 (tcp)
- `3000:8080` -> remote 8080 to local 3000
- `5432/tcp` -> explicit protocol
- `5353/udp` -> UDP forward
- `3000:8080/udp` -> full form

## Reflexes

- Prefer `graft run --to <conn> <cmd>` over `ssh <host> <cmd>`.
- Prefer `graft sync` (or just letting sync run) over `scp` / `rsync` for
  graft-managed directories.
- Prefer `graft forward 8080` over `ssh -L 8080:localhost:8080 <host>`. Often
  you don't need to declare anything: graft auto-detects and forwards remote
  listeners.
- Before assuming a connection is healthy, read `graft status --json`. Before
  assuming a port is reachable, read `graft forward list` or the
  `port_forward_statuses[]` field of `graft status --json` and check
  `conflict` / `conflict_reason` for "address already in use" failures on the
  local side.
- When anything looks broken, run `graft doctor` BEFORE guessing. It is
  faster and more accurate than poking at logs.
- Use `graft use <conn>` to pin a session instead of passing `--to` to every
  command in a sequence.
- `graft connect` with no positional args reads `graft.yaml` from CWD; with
  positional args + flags it ignores any yaml.
- Port forwards are **connection-wide**, not per-CWD - adding one once
  persists for the connection's lifetime.

## Troubleshooting ladder

Walk this in order. Stop at the first step that surfaces the problem.

1. `graft status --json` - is the connection in `CONNECTION_STATE_CONNECTED`?
   what does `state_reason` say? are sync conflicts or port conflicts
   reported?
2. `graft doctor` - runs the full check suite (shell activation, local
   daemon, SSH reachability, transport mode, remote daemon, remote dirs).
   Read `references/doctor-playbook.md` for failure modes.
3. `graft daemon logs` - recent local daemon logs.
4. `graft ping` - is the local daemon even reachable?
5. `graft daemon restart` - last resort. Confirm with the user first if
   other connections might be in use.

## What to escalate to the user before doing

Always confirm before:

- `graft disconnect <conn>` or removing a connection from daemon state
- Writing or modifying a `graft.yaml` that is tracked in git
- `graft daemon stop` / `restart` while other connections are active
- Anything that prompts for credentials

## Reference and example files

Loaded on demand:

- `references/commands.md` - full per-command flag reference
- `references/config-schema.md` - complete `graft.yaml` schema (project +
  workspace), with the daemon-state-vs-user-config distinction
- `references/status-json-shape.md` - field-by-field meaning of
  `graft status --json`
- `references/doctor-playbook.md` - one section per `graft doctor` check,
  with failure modes and fixes
- `examples/connect-flag-cheatsheet.md` - worked `graft connect`
  invocations covering the common combinations
- `examples/forward-cheatsheet.md` - port spec grammar and forward
  workflows
- `examples/project.graft.yaml`, `examples/workspace.graft.yaml` - copy-
  paste starting points
