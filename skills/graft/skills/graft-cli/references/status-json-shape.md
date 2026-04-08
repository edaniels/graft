# `graft status --json` shape reference

`graft status --json` emits a `ListConnectionsResponse` from the
`graft.v1.GraftService` proto, marshaled by `protojson` with proto field
names (not camelCase). The relevant proto definitions live in
`proto/graft/v1/graft.proto` and the marshal code is in
`pkg/local_client_commands.go:151-174`.

## Top-level shape

```json
{
  "connections": {
    "build": { ... ConnectionStatus ... },
    "lab": { ... ConnectionStatus ... }
  }
}
```

A map keyed by connection name. The map may be empty if no connections are
configured.

## `ConnectionStatus`

```json
{
  "state": "CONNECTION_STATE_CONNECTED",
  "state_reason": "",
  "current": true,
  "sync_statuses": [ ... ],
  "safe_destination": "ssh://ec2-user@build-box",
  "port_forward_statuses": [ ... ]
}
```

| field | meaning |
| --- | --- |
| `state` | one of `CONNECTION_STATE_UNKNOWN`, `CONNECTION_STATE_INITIALIZING`, `CONNECTION_STATE_CONNECTED`, `CONNECTION_STATE_FAILED`, `CONNECTION_STATE_CLOSED`, `CONNECTION_STATE_RECONNECTING`. Anything other than `CONNECTED` is not usable yet |
| `state_reason` | optional human-readable reason. Populated when `state` is `FAILED` or `RECONNECTING`. Read this first when diagnosing connection issues |
| `current` | true if this is the connection that would be selected for the current shell session (per the explicit/pin/CWD hierarchy) |
| `sync_statuses` | array of `SyncStatus`, one per active sync intent. Empty if `--sync` was not used |
| `safe_destination` | sanitized destination URI (credentials stripped). Safe to display |
| `port_forward_statuses` | array of `PortForwardStatus`, one per active forward (auto-detected and explicit) |

## `SyncStatus`

```json
{
  "from_local": "/Users/eric/work",
  "to_remote": "/home/ec2-user/work",
  "paused": false,
  "status": "watching",
  "last_error": "",
  "conflicts": [],
  "problems": [],
  "staging_progress": null
}
```

| field | meaning |
| --- | --- |
| `from_local` | local path being synced |
| `to_remote` | remote path being synced |
| `paused` | whether the sync is currently paused |
| `status` | mutagen-style status string (`watching`, `staging`, `transitioning`, etc.) |
| `last_error` | most recent error message; empty when healthy |
| `conflicts` | array of `SyncConflict { path, local_changes[], remote_changes[] }`. Non-empty means files diverged on both sides and need manual resolution |
| `problems` | array of `SyncProblem { path, error }`. Non-conflict errors (permission denied, missing parent, etc.) |
| `staging_progress` | optional `SyncStagingProgress { received_files, expected_files, total_received_size, total_expected_size, current_path, ... }`, populated mid-transfer |

A connection is **healthy for sync** when `paused` is false, `last_error` is
empty, and both `conflicts` and `problems` are empty.

## `PortForwardStatus`

```json
{
  "remote_port": 8080,
  "local_port": 8080,
  "protocol": "tcp",
  "conflict": false,
  "conflict_reason": "",
  "explicit": false
}
```

| field | meaning |
| --- | --- |
| `remote_port` | port on the remote machine |
| `local_port` | port on the local machine. May differ from `remote_port` if the user mapped them |
| `protocol` | `"tcp"` or `"udp"` |
| `conflict` | true if graft could not bind the local port (e.g. another process is already listening on it). The forward is inactive while this is true |
| `conflict_reason` | when `conflict` is true, the reason graft could not bind. Read this to diagnose "address already in use" errors |
| `explicit` | true if the user requested this forward (via `--forward`, `graft forward`, or `ports:` in `graft.yaml`); false if graft auto-detected the remote listener |

A port is **reachable** when `conflict` is false. If `conflict` is true,
either kill the local process holding the port or remap with `graft forward
<other_local>:<remote_port>`.

## Distinguishing auto-detected vs explicit forwards

`explicit: false` forwards come and go with the remote process - graft
detects them via `/proc/net/tcp{,6}` and `/proc/net/udp{,6}` polling on
Linux remotes. Removing one with `graft forward remove` only takes effect
until the remote process listens again.

`explicit: true` forwards persist even when the remote process is not
listening, and only go away when explicitly removed.
