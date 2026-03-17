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

#### Alternative Installation methods:

<details>
<summary>Nix Flakes</summary>

```nix
# In your flake inputs add graft:
inputs = {
    # ...
    graft.url = "github:edaniels/graft";
    # ...
}

# In your configuration add graft to your systemPackages:
# ${system} should be defined as your current platform. Ex: "x86_64-linux" or "aarch64-darwin".
environment.systemPackages = [
    # ...
    graft.packages.${system}.default
    # ...
]
```

</details>

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
- **LSP support** -- run language servers remotely with local editor integration (written, not yet tested)

## Architecture

See [docs/architecture.md](docs/architecture.md) for how graft works internally.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines, including our [AI usage policy](CONTRIBUTING.md#ai-usage-policy). This project uses AI tools responsibly - all code is human-reviewed before merging, and all contributions must disclose AI usage.

## Development

All build/test/lint commands use [`just`](https://github.com/casey/just):

```bash
just graft-dev    # build and install for local dev
just test         # run tests
just lint         # run all linters
```

See the `justfile` for the full list of recipes.
