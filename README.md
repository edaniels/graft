<div align="center">

# <a href="https://graft.run"><img src="assets/logo.svg" width="36" height="36" alt="Graft logo"></a> [graft](https://graft.run)

A local-first remote development platform. Work with remote files and commands as if they were local.

[![CI](https://github.com/edaniels/graft/actions/workflows/ci.yml/badge.svg)](https://github.com/edaniels/graft/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

> **Note:** This project is still in alpha development. Breaking changes may be made. Issues and contributions are welcome!

</div>

## Supported Platforms

|       | Local (client) | Remote (target) |
| ----- | -------------- | --------------- |
| macOS | yes            |                 |
| Linux | yes            | yes             |

## Install

```bash
curl --proto '=https' --tlsv1.2 -sSf https://graft.run/install.sh | sh
```

Options: `--install-dir <dir>`, `--version <tag>`

## Quick Start

**1. Activate shell integration** (add to your shell rc file):

```bash
# bash: ~/.bashrc, zsh: ~/.zshrc
eval "$(graft activate zsh)"
```

This lets graft track your working directory so commands like `run`, `shell`, `sync`, and `forward` can automatically detect which connection to use.

**2. Start the daemon:**

```bash
graft daemon service install   # auto-start on login
```

The daemon runs in the background and manages your remote connections.

**3. Connect to a remote machine:**

```bash
graft connect . user@host:~/project --sync
```

This connects your current directory (`.`) to `~/project` on the remote host, with `--sync` to enable bidirectional file synchronization. You can also use `graft init` to save connection settings to a `graft.yaml` file for repeated use.

**4. Use it from within the connected directory:**

```bash
graft run make build           # run a command remotely
graft shell                    # open a remote shell
graft sync                     # sync files to the remote
graft forward go make          # forward commands to the remote
```

All of these commands detect the connection from your current directory. You can also specify a connection explicitly with `--to <connection>`, or pin a connection to your shell session with `graft use <connection>`.

## Use with Claude Code

If you use [Claude Code](https://claude.com/claude-code), install the graft plugin so Claude knows how to use graft for remote development. It covers running commands, syncing files, forwarding ports, and diagnosing connection issues:

```
/plugin marketplace add edaniels/graft
/plugin install graft@graft
```

The plugin auto-triggers when you're working in a graft-managed directory, so you can keep using Claude Code normally; it will reach for `graft run`, `graft sync`, and `graft status` instead of raw `ssh`/`scp` where appropriate.

## Commands

| Command      | Description                                                          |
| ------------ | -------------------------------------------------------------------- |
| `connect`    | Connect to a remote machine (SSH or Docker)                          |
| `disconnect` | Disconnect from a remote connection                                  |
| `run`        | Run a command on the remote                                          |
| `shell`      | Open a remote shell                                                  |
| `sync`       | Sync files to the remote                                             |
| `forward`    | Forward local commands to the remote                                 |
| `use`        | Pin a connection to the current shell session                        |
| `status`     | Show connection status                                               |
| `doctor`     | Check environment setup and diagnose issues                          |
| `lsp`        | Proxy a language server running on the remote ([details](#remote-language-servers-lsp)) |
| `init`       | Generate a graft.yaml configuration file for future `graft connect`s |

## Connection selection

When you run a command like `graft run` or `graft shell`, graft needs to know which connection to use. It follows this hierarchy:

1. **Explicit** - `--to <connection>` on the command line
2. **Session pin** - set with `graft use <connection>`, applies to the current shell
3. **CWD-based** - automatically detected from your working directory based on each connection's local root

`graft use` is useful when you have multiple connections and want to lock your shell to a specific one:

```bash
graft use labos          # pin this shell to the "labos" connection
graft shell              # opens a shell on labos, regardless of cwd
graft use --clear        # resume CWD-based auto-selection
```

## Syncing .git (read-only remote git)

By default graft does not sync `.git`, so git commands on the remote fail with
`fatal: not a git repository`. Opt in to a one-way replica of your local
`.git` with:

```bash
graft sync --git             # or: graft connect --sync --sync-git
```

or `syncGit: true` on a synchronization in the daemon config (and
`syncGit: true` on a destination in `graft.yaml`).

The replica makes the remote's git **read-only**: `git status`, `log`,
`diff`, `blame`, and `rev-parse` all work, but anything the remote writes to
`.git` is reverted on the next flush. In particular:

- Remote `git commit`/`branch`/`tag`/`fetch` appear to succeed, then
  silently evaporate seconds later. Commit locally instead.
- **Never run `git stash`, `git checkout <branch>`, or `git reset --hard` on
  the remote.** Their working-tree changes flow back to your local machine
  through the two-way file sync while the git metadata reverts; `git stash`
  in particular reverts your edits on both sides and then loses the stash.
- Git's transient files (`*.lock`, temp objects, `gc.pid`) are never
  replicated, so a local `index.lock` can't wedge remote git.

The initial sync transfers your entire `.git` (which can be large), and a
local `git gc`/repack re-transfers the rewritten packfiles.

## Syncing gitignored files (e.g. generated code)

By default graft derives its sync ignores from your `.gitignore`, so anything
git ignores is also skipped by sync. That is usually what you want, but not
for generated files you deliberately keep out of git yet still need on both
ends, like generated protobufs for example, that a build on the remote produces
and you want mirrored back locally.

Use `--include-ignored` (repeatable) to sync a gitignore-style pattern even
though `.gitignore` excludes it:

```bash
graft sync --include-ignored '**/*_pb2.py' --include-ignored '**/*.pb.go'
# or: graft connect --sync --sync-include-ignored '**/*_pb2.py'
```

or `syncInclude` on a synchronization in the daemon config (and on a
destination in `graft.yaml`):

```yaml
destinations:
  myconn:
    host: myhost
    user: ubuntu
    syncTo: ~/mydir
    sync: true
    syncInclude:
      - "**/*_pb2.py"
      - "**/*.pb.go"
```

One caveat, inherited from how git and the sync engine both work: you **cannot
re-include a file whose parent directory is ignored outright**. Ignore a
directory's *contents* (`gen/**`), not the directory itself (`gen/`), or the
scan prunes the directory before the re-include is ever consulted. graft warns
you (right in the `graft sync` output, and in the daemon log for config-driven
syncs) when an include pattern is shadowed this way, so it never fails
silently.

## Remote language servers (LSP)

Point your editor's LSP client at graft instead of the language server binary:

```jsonc
"command": ["graft", "lsp", "rust-analyzer"]
```

`graft lsp <server>` is a stdio LSP proxy. It starts `<server>` on the
connection matching your working directory (falling back to a local `<server>`
if the remote does not have one) and rewrites `file://` URIs between the local
and remote sides using the connection's path remappings, so the server sees
remote paths while your editor sees local ones.

### Files that only exist on the remote (`graft://` URIs)

Definitions often land in files outside the synced tree that only exist on the
remote: the cargo registry, rust std sources, a Go module cache. The proxy
rewrites those to `graft://<connection>/<remote-path>` URIs and serves their
content read-only through the LSP 3.18 `workspace/textDocumentContent`
request, so goto-definition opens the exact bytes the server analyzed, with no
local copy of the toolchain required. Hover and further navigation keep
working from inside those buffers; editing and saving them does not (they are
read-only views).

This requires an LSP client that supports `workspace/textDocumentContent`
(proposed in LSP 3.18). Tested with Sublime Text's LSP package (>= 2.13.0).

### Sublime Text setup

In `Preferences > Package Settings > LSP > Settings`:

```jsonc
{
  "clients": {
    "rust-remote": {
      "enabled": true,
      "selector": "source.rust",
      "command": ["graft", "lsp", "rust-analyzer"],
      // Attach to graft:// buffers so goto-def and hover keep working from
      // inside remote-only sources (the default is ["file"] only).
      "schemes": ["file", "graft"],
      // Syntax highlighting for those read-only buffers.
      "syntax_map": {
        "graft": "Packages/Rust/Rust.sublime-syntax"
      }
    }
  }
}
```

Editors title these buffers with the last segment of the URI, so graft marks
the file name with the connection it came from, keeping the extension intact
for editors that infer the language from it: goto-definition into the cargo
registry opens a tab named `context@myconn.rs` rather than a bare
`context.rs` that looks local. The marker is stripped before any path reaches
the language server or the remote filesystem.

## Projects and workspaces

Instead of passing flags to `graft connect` every time, you can save connection settings in a `graft.yaml` file.

### Project config

A project config lives in a project directory and defines how to connect:

```bash
graft init . ubuntu@myhost:~/mydir --name myconn --sync --forward make
```

This creates a `graft.yaml`:

```yaml
version: v1
forward:
  - make
destinations:
  myconn:
    host: myhost
    user: ubuntu
    syncTo: ~/mydir
    sync: true
```

Then `graft connect` with no arguments from that directory reads the config automatically.

### Workspace config

A workspace groups multiple projects under a shared root. Create one with:

```bash
cd ~/work
graft init --workspace
```

This creates a `graft.yaml` with `workspace: true`. When you run `graft connect` from a project directory inside the workspace, graft walks up the directory tree looking for the workspace root. If found and `syncWorkspace` is enabled, the entire workspace directory is synced rather than just the project subdirectory.

```
~/work/                  <- workspace root (graft.yaml with workspace: true)
  infra/
    projectA/            <- project (graft.yaml with destinations)
    projectB/            <- project (graft.yaml with destinations)
```

### Background connections

Connections created with `--background` are excluded from CWD-based auto-selection. This is useful for auxiliary connections (e.g. a shared build server) that you only want to use explicitly via `--to` or `graft use`:

```bash
graft connect . user@build-server --background --name build
graft use build          # explicitly switch to it
```

## Coming Soon

- **Transparent SSH agent forwarding** -- use local SSH keys on the remote without manual setup (written, not yet tested)

## Architecture

See [docs/architecture.md](docs/architecture.md) for how graft works internally.

## Security

See [docs/architecture.md#security-model](docs/architecture.md#security-model).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines, including our [AI usage policy](CONTRIBUTING.md#ai-usage-policy). This project uses AI tools responsibly - all code is human-reviewed before merging, and all contributions must disclose AI usage.

## Development

All build/test/lint commands use [`just`](https://github.com/casey/just):

```justfile
just graft-dev    # build and install for local dev
just test         # run tests
just lint         # run all linters
```

See the `justfile` for the full list of recipes.
