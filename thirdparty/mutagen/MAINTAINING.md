# Maintaining the Mutagen Fork

## Upstream Source

This is a git subtree of [mutagen-io/mutagen](https://github.com/mutagen-io/mutagen), forked from commit [`cc411d5c`](https://github.com/mutagen-io/mutagen/commit/cc411d5c583a9d90749faa3dd8fd663505144181) (PR #498: "all: prepare for v0.18.0-rc1 release", April 30, 2024).

## Changes from Upstream

### SSPL Code Removal

All SSPL-licensed code and metadata has been removed:

- `sspl/` directory (license + all SSPL source)
- `pkg/mutagen/licenses_sspl.go`
- `pkg/synchronization/compression/zstandard_sspl.go`
- `pkg/synchronization/hashing/xxh128_sspl.go`
- `pkg/filesystem/watching/watch_recursive_linux_sspl_fanotify.go`
- `cmd/mutagen/sync/create_sspl.go`
- Build constraint in `pkg/filesystem/watching/watch_recursive_unsupported.go` simplified to remove `mutagensspl` tags

### Linux File Watcher Replacement

`pkg/filesystem/watching/watch_recursive_linux.go` - new file replacing the SSPL fanotify watcher. Uses `github.com/rjeczalik/notify` instead. Provides `LinuxRecursiveWatcher` with channel-based event delivery.

### macOS fsevents Dependency Swap

`pkg/filesystem/watching/watch_recursive_darwin_cgo.go` - import changed from `github.com/mutagen-io/fsevents` to `github.com/fsnotify/fsevents` (upstream's fork replaced with the community package). Corresponding license entry removed from `pkg/mutagen/licenses.go`.

### Logger slog Bridge

`pkg/logging/logger.go` - added `NewLoggerOnSlogger()` constructor and modified `Sublogger`, `log`, and `logf` to route through Go's `log/slog` when a slogger is configured. Allows graft to capture mutagen log output via structured logging.

### Session Manager Without Persistence

`pkg/synchronization/manager.go` - added `NewManagerWithoutPersistence()` which creates a Manager that skips loading sessions from disk. Graft manages session lifecycle externally.

### go.mod Stripped

`go.mod` contains only the module declaration and Go version. All dependencies are resolved through the root `go.mod` via a `replace` directive:

```
replace github.com/mutagen-io/mutagen => ./thirdparty/mutagen
```

### Docs/Metadata Removed

Deleted `BUILDING.md`, `CONTRIBUTING.md`, `DCO`, `README.md`, `SECURITY.md`. `LICENSE` kept.

## Pulling Upstream Updates

Since this is a git subtree, our local changes are tracked in git history. When pulling upstream updates, git will merge them with our modifications - you only need to manually intervene if there are merge conflicts.

```bash
# Add the upstream remote (one-time)
git remote add mutagen https://github.com/mutagen-io/mutagen.git

# Fetch upstream
git fetch mutagen

# Pull the subtree update (replace TARGET_REF with the desired tag, branch, or commit)
git subtree pull --prefix=thirdparty/mutagen mutagen TARGET_REF --squash
```

### After Pulling

If the merge completes cleanly, verify things still work:

1. **Run `go mod tidy`** from the repo root
2. **Run `just lint`** and fix any issues
3. **Build and test** - `just graft-dev` and run the test suite

### Resolving Conflicts

If there are merge conflicts, they'll most likely be in files we've modified. Watch for:

- **SSPL code re-added** - upstream may add new SSPL files or re-add the `sspl/` directory. Delete them and remove any `mutagensspl` build tags.
- **go.mod** - upstream will have full dependency lists; strip it back to just the module declaration and Go version.
- **`pkg/logging/logger.go`** - if upstream modifies the logger, ensure `NewLoggerOnSlogger` and the slog routing in `log`/`logf`/`Sublogger` still apply.
- **`pkg/synchronization/manager.go`** - ensure `NewManagerWithoutPersistence` still compiles against any changes to `Manager` fields.
- **`pkg/filesystem/watching/`** - if upstream changes the watcher interfaces, our `watch_recursive_linux.go` and the fsevents import swap in `watch_recursive_darwin_cgo.go` may need updating.
- **Docs** - upstream may re-add `BUILDING.md`, `CONTRIBUTING.md`, `DCO`, `README.md`, `SECURITY.md`. Delete them.
