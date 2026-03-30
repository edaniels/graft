# Graft for VS Code / Cursor

[Graft](https://graft.run) integration for VS Code and Cursor. Shows the active connection in the status bar, lets you switch connections, and open remote terminals.

## Setup

Install [Graft](https://graft.run), start the daemon (`graft daemon`), and open a synced workspace folder. The extension picks up the connection automatically.

## Commands

| Command                            | Description                             |
| ---------------------------------- | --------------------------------------- |
| Graft: Select Connection           | Pin a connection to the current session |
| Graft: Open Terminal to Connection | Open a remote shell                     |

## Settings

| Setting                | Description                                                  |
| ---------------------- | ------------------------------------------------------------ |
| `graft.executablePath` | Path to the graft binary. Resolved via login shell if empty. |
