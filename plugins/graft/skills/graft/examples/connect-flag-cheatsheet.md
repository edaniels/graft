# `graft connect` flag cheat sheet

Worked examples for the ad-hoc `graft connect` path. Each example assumes you
are running it from the local directory you want to associate with the
connection.

## SSH, no sync, no forwards

Smallest possible connection. Useful for `graft run` / `graft shell` only.

```bash
graft connect . user@host
```

## SSH with bidirectional sync

Most common case. Sync the current directory to `~/work` on the remote, in
both directions, with `.gitignore` respected.

```bash
graft connect . user@build-box:~/work --sync
```

## SSH with named connection

Names default to the host. Pass `--name` when you have multiple connections
to the same host or want a friendlier name for `graft use` and `--to`.

```bash
graft connect . user@build-box:~/work --sync --name build
```

## SSH with command forwarding

Shim `make` and `bazel` so running them from this directory transparently
runs them on the remote.

```bash
graft connect . user@build-box:~/work \
  --sync \
  --forward make --forward bazel \
  --name build
```

## SSH with port forwarding

Forward remote port 8080 to local 8080 (graft auto-detects port specs in
`--forward`):

```bash
graft connect . user@build-box:~/work --forward 8080
```

Map remote 8080 to local 3000:

```bash
graft connect . user@build-box:~/work --forward 3000:8080
```

UDP forward:

```bash
graft connect . user@build-box:~/work --forward 5353/udp
```

## SSH with mixed command and port forwards in one flag

`--forward` is partitioned automatically. You can mix freely:

```bash
graft connect . user@build-box:~/work \
  --sync \
  --forward make --forward bazel \
  --forward 8080 --forward 5432:5432/tcp \
  --name build
```

## SSH with prefixed command forwarding

When two connections both forward `python3`, you'll get a collision unless
one of them prefixes. Use `--forward-prefix` to prefix shimmed commands with
the connection name (`python3` -> `build-python3`):

```bash
graft connect . user@build-box:~/work --forward python3 --forward-prefix --name build
```

## Background SSH connection

A background connection is excluded from CWD-based auto-selection. Use it
for an auxiliary remote (e.g. a shared build box) you only want to use
explicitly via `--to` or `graft use`:

```bash
graft connect . user@build-box --background --name build
graft use build              # explicitly switch to it
graft run -- make            # runs on build
```

## Docker container

```bash
graft connect . docker://ubuntu:24.04
```

With an OS hint:

```bash
graft connect . docker://my-image:latest --os ubuntu
```

## Reading from `graft.yaml` instead

If a `graft.yaml` exists in the current directory, the simplest invocation
just is:

```bash
graft connect
```

This reads the project (and any parent workspace) config and connects
without needing any flags. See `examples/project.graft.yaml` and
`examples/workspace.graft.yaml` for what to put in those files.
