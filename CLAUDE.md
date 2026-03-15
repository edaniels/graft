# CLAUDE.md

## TDD

Use red/green test driven development.

## Project Overview

Graft is a local-first remote development platform that enables working with remote files and commands as if they were local. `graft` is the CLI client and daemon server (run `graft daemon` to start the server).

## Build Commands

```bash
just build-all      # Build graft (installs + cross-compiles for linux/darwin)
just graft          # Build and install graft (also builds cross-compilation binaries)
just graft-dev      # Build and install graft for local dev (no embedded binaries, faster)
just lint           # Run all linters (protos + just + go)
just go-lint        # Go linting only (vet, gofumpt, golangci-lint)
just protos-lint    # Protobuf linting (buf format + buf lint)
just protos         # Lint and generate protobuf code (outputs to gen/proto/)
```

## Architecture

### Two-Tier Daemon Model
- Local daemon (`graft daemon`) listens on Unix socket (~/.local/state/graft/local/graft.sock)
- Remote daemon (`graft daemon --as-remote`) runs on target machines (SSH/Docker)
- Communication between daemons uses gRPC with bidirectional streaming for command I/O

### Key Packages (pkg/)
- `server.go` - Main server with ConnectionManager and SessionManager
- `connection_manager.go` - Manages Connection lifecycle
- `connector_ssh.go`, `connector_docker.go` - Protocol-specific connectors
- `session_manager.go` - Tracks local sessions by PID
- `server_grpc_*.go` - gRPC service implementations (command, connection, session, administrative)
- `running_command.go` - Abstracts local/remote/docker command execution

### Code Style
- **Formatting**: Never use emdashes. Use regular dashes instead.
- **Errors**: Use the project's errors package over the stdlib package.
- **Linting**: After every comittable unit of work completed, run `just lint`.
- **Tests**: Every test must exercise **our code**, not stdlib or third-party behavior. If a function can't be called in a test (e.g. it depends on `os.Executable()`), refactor it to be testable.
- **Tests**: Never use `time.Sleep` in tests - use channels to wait for conditions. Use `go.viam.com/test` for assertions, never `t.Fatal`/`t.Fatalf`/`t.Error` directly:
  ```go
  import "go.viam.com/test"

  test.That(t, err, test.ShouldBeNil)
  test.That(t, result, test.ShouldEqual, expected)
  test.That(t, slice, test.ShouldBeEmpty)
  test.That(t, flag, test.ShouldBeTrue)
  ```

### Git Commits
- Do not add Co-Authored-By lines - all code belongs to the repository owner

## Dependencies

- Mutagen fork in `thirdparty/` for file synchronization
- gRPC/protobuf for all IPC
