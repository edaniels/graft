# graft.yaml schema reference

This is the schema for the **user-authored `graft.yaml`** that lives in a
project or workspace directory and is read by `graft connect` (and written by
`graft init`). For the daemon-internal state schema, see "Daemon state vs
user config" at the bottom of this file.

The Go definitions live in `cmd/graft/helpers.go:108-130`.

## Project config

A project config defines one or more destinations for a single project
directory.

```yaml
version: v1
forward:
  - make
  - bazel
  - 8080
  - 5432:5432/tcp
destinations:
  build:
    host: build-box
    user: ec2-user
    syncTo: ~/work
    sync: true
    prefix: false
```

### Top-level fields (`ProjectConfig`)

| field | type | required | meaning |
| --- | --- | --- | --- |
| `version` | string | recommended | schema version. Currently `v1` |
| `forward` | list of strings | no | commands to forward and/or port specs to forward. graft auto-partitions by spec shape - anything matching a port spec becomes a port forward, the rest become command shims |
| `destinations` | map of name -> `ProjectDestinationConfig` | yes | named destinations for this project. Currently exactly one destination is supported per project (`cmd/graft/connect.go:227-229`) |

### Per-destination fields (`ProjectDestinationConfig`)

| field | type | meaning |
| --- | --- | --- |
| `host` | string | hostname or SSH config alias. Required |
| `user` | string | SSH username. Optional if encoded in SSH config |
| `syncTo` | string | remote directory to sync to. Required if `sync: true` |
| `sync` | bool | enable bidirectional file sync to `syncTo` |
| `prefix` | bool | prefix forwarded commands with the connection name (e.g. `make` -> `build-make`) to avoid collisions when multiple connections forward the same command |

### Port spec grammar (in the `forward` list)

`[local_port:]remote_port[/protocol]`

- `8080` - remote 8080 to local 8080 (tcp)
- `3000:8080` - remote 8080 to local 3000
- `5432/tcp` - explicit protocol
- `5353/udp` - UDP forward
- `3000:8080/udp` - full form

## Workspace config

A workspace config sits at the root of a directory tree containing multiple
projects. When `graft connect` runs from inside a project under a workspace,
it walks up looking for the workspace root (`cmd/graft/helpers.go:134-161`)
and, if `defaults.syncWorkspace` is `true`, syncs the entire workspace
instead of just the project subdirectory.

```yaml
version: v1
workspace: true
defaults:
  syncWorkspace: true
```

### Top-level fields (`WorkspaceConfig`)

| field | type | meaning |
| --- | --- | --- |
| `version` | string | schema version. Currently `v1` |
| `workspace` | bool | must be `true` for graft to recognize this as a workspace root |
| `defaults` | `WorkspaceConfigDefaults` | defaults applied to projects under this workspace |

### `WorkspaceConfigDefaults`

| field | type | meaning |
| --- | --- | --- |
| `syncWorkspace` | bool | when true, projects under this workspace sync the entire workspace tree as one unit instead of just the project subdirectory |

## Layout

```
~/work/                  <- workspace root: graft.yaml with workspace: true
  infra/
    projectA/            <- project: graft.yaml with destinations
    projectB/            <- project: graft.yaml with destinations
```

A workspace root config does not contain destinations. Each project under it
has its own `destinations:` map. The workspace config only opts the
descendants into whole-workspace sync semantics.

## Generating these from flags

`graft init` writes both shapes:

- `graft init . user@host:~/work --name build --sync --forward make` writes
  the project shape above.
- `graft init --workspace` writes the workspace shape above with
  `syncWorkspace: true`.

`graft init` refuses to overwrite an existing `graft.yaml` unless `--force`
is passed.

## Daemon state vs user config

There is a **second** YAML schema in graft, in `pkg/config.go:18-136`, that
defines `RootConfig` and `ConnectionConfig`:

```yaml
connections:
  - name: build
    destination: ssh://ec2-user@build-box
    localRoot: /Users/eric/work
    remoteRoot: /home/ec2-user/work
    forward: [make, bazel]
    prefixForward: []
    synchronizations:
      - fromLocal: /Users/eric/work
        toRemote: /home/ec2-user/work
    background: false
    ports: ["8080", "5432:5432/tcp"]
```

**This is not what users hand-author.** It is the daemon's persistent state
file (typically under `$GRAFT_STATE_HOME/graft/local/`) that records every
connection the daemon currently knows about. The daemon owns it, reloads it
on startup, and rewrites it when connections change.

If a user asks "what does my graft.yaml need", they mean the
project/workspace schema above, not this one. Only mention the daemon state
schema if the user explicitly asks about graft's internal state files.
