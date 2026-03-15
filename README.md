<div align="center">

# <a href="https://graft.run"><img src="assets/logo.svg" width="36" height="36" alt="Graft logo"></a> [graft](https://graft.run)

A local-first remote development platform. Work with remote files and commands as if they were local.

[![CI](https://github.com/edaniels/graft/actions/workflows/ci.yml/badge.svg)](https://github.com/edaniels/graft/actions/workflows/ci.yml)
[![Downloads](https://img.shields.io/github/downloads/edaniels/graft/total.svg)](https://github.com/edaniels/graft/releases)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

> **Note:** This project is still in alpha development. Breaking changes may be made. Issues and contributions are welcome!

</div>

## Supported Platforms

| | Local (client) | Remote (target) |
|---|---|---|
| macOS | yes | |
| Linux | yes | yes |

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

**2. Start the daemon:**

```bash
graft daemon service install   # auto-start on login
```

**3. Connect to a remote machine:**

```bash
graft connect user@host
```

**4. Use it:**

```bash
graft run make build           # run a command remotely
graft shell                    # open a remote shell
graft sync                     # sync files to the remote
graft forward go make          # forward commands to the remote
```

## Commands

| Command | Description |
|---|---|
| `connect` | Connect to a remote machine (SSH or Docker) |
| `disconnect` | Disconnect from a remote connection |
| `run` | Run a command on the remote |
| `shell` | Open a remote shell |
| `sync` | Sync files to the remote |
| `forward` | Forward local commands to the remote |
| `status` | Show connection status |
| `doctor` | Check environment setup and diagnose issues |
| `init` | Generate a graft.yaml configuration file for future `graft connect`s |

## Coming Soon

- **Transparent SSH agent forwarding** -- use local SSH keys on the remote without manual setup (written, not yet tested)
- **LSP support** -- run language servers remotely with local editor integration (written, not yet tested)

## Architecture

See [docs/architecture.md](docs/architecture.md) for how graft works internally.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines, including our [AI usage policy](CONTRIBUTING.md#ai-usage-policy). This project uses AI tools responsibly — all code is human-reviewed before merging, and all contributions must disclose AI usage.

## Development

All build/test/lint commands use [`just`](https://github.com/casey/just):

```bash
just graft-dev    # build and install for local dev
just test         # run tests
just lint         # run all linters
```

See the `justfile` for the full list of recipes.
