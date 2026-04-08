# `graft forward` cheat sheet

## Port spec grammar

`[local_port:]remote_port[/protocol]`

| spec | meaning |
| --- | --- |
| `8080` | remote 8080 to local 8080 (tcp) |
| `3000:8080` | remote 8080 to local 3000 |
| `5432/tcp` | explicit protocol |
| `5353/udp` | UDP forward |
| `3000:8080/udp` | full form: local:remote/protocol |

## Adding forwards to an existing connection

The current-directory connection is used unless `--to` is passed.

```bash
graft forward 8080                       # add a tcp forward
graft forward 5432:5432/tcp              # explicit protocol
graft forward 5353/udp                   # udp
graft forward make bazel                 # add command shims
graft forward 8080 9090 make             # mix of ports and commands in one call
graft forward --to build 8080            # target a specific connection
```

## Listing what's currently forwarded

```bash
graft forward list
```

Shows shimmed commands (per session) and active port forwards
(per connection). Includes both **auto-detected** and **explicit** forwards.

For a structured view that includes conflict reasons, use:

```bash
graft status --json
```

and inspect each connection's `port_forward_statuses[]`. See
`references/status-json-shape.md` for the field meanings.

## Removing forwards

```bash
graft forward remove 8080                # remove an explicit port forward
graft forward remove make                # remove a command shim
graft forward remove 8080 make           # remove both at once
```

If you remove a port that was **auto-detected** (graft was forwarding it
because the remote process was listening), graft prints a warning:

```
warning: port 8080 is auto-detected, not explicitly forwarded;
it will stop being forwarded when the remote process stops listening on it
```

This is the intended behavior - auto-detected forwards come and go with the
remote process. To stop auto-detection entirely you have to stop the remote
listener.

## Which connection owns a shimmed command

```bash
graft forward which make
```

Resolves which connection a command shim points at. Port forwards are
connection-wide, not command-shaped, so this command rejects port specs
with: "port forwards are connection-wide; use 'graft forward list' to see
them".

## Auto-detection vs explicit forwards

graft watches `/proc/net/tcp{,6}` and `/proc/net/udp{,6}` on the remote
(Linux only) and forwards anything that starts listening. Most of the time
you do not need to declare forwards at all - if you start a dev server on
the remote that binds `:8080`, graft will start forwarding it within a
second.

Declare an **explicit** forward when:

- the remote process listens before graft sees it and you want a
  guaranteed-immediate forward
- you want to remap the port (`3000:8080` is local 3000 -> remote 8080)
- you want the forward to persist even when the remote process is not
  running
- the remote OS does not expose `/proc/net/*` (auto-detection is Linux-only)

## Diagnosing port conflicts

If `graft status --json` shows `conflict: true` and a `conflict_reason` for
a forward, the local port is already bound by something else. Two fixes:

1. Stop the local process holding the port.
2. Remap to a free local port: `graft forward 9000:8080` (then access via
   `localhost:9000`).
