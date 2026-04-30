# https://github.com/casey/just/blob/master/README.md

set unstable := true

default:
    @just -l

GRAFT_PKG := 'github.com/edaniels/graft/cmd/graft'
BIN_DIR := justfile_directory() / 'bin'
EMBED_DIR := justfile_directory() / 'pkg' / 'embedded' / 'binaries'
DAEMON_NAME := 'graft'
BUILD_VERSION := env('BUILD_VERSION', '')
BUILD_UPDATE_URL := env('BUILD_UPDATE_URL', '')
[private]
_LD_BUILD_VERSION := if BUILD_VERSION != "" { " -X github.com/edaniels/graft/pkg.buildVersion=" + BUILD_VERSION } else { "" }
[private]
_LD_BUILD_UPDATE_URL := if BUILD_UPDATE_URL != "" { " -X github.com/edaniels/graft/pkg.defaultUpdateURL=" + BUILD_UPDATE_URL } else { "" }
LD_FLAGS := '-ldflags "-w -s' + _LD_BUILD_VERSION + _LD_BUILD_UPDATE_URL + '"'
GO_ENV_FLAGS := "GOBIN=$HOME/go/bin"
GO_BUILD_FLAGS := '-trimpath ' + if env('RACE', 'false') == "true" { "-race" } else { "" }
PLATFORMS := "linux-amd64 linux-arm64 darwin-arm64"
LINT_GOOS_LIST := "linux darwin"

# Build a single platform binary (no embedding)
build-graft os arch:
    CGO_ENABLED={{ if os == "darwin" { "1" } else { "0" } }} {{ GO_ENV_FLAGS }} GOOS={{ os }} GOARCH={{ arch }} go build {{ GO_BUILD_FLAGS }} {{ LD_FLAGS }} -o {{ BIN_DIR }}/{{ DAEMON_NAME }}-{{ os }}-{{ arch }} {{ GRAFT_PKG }}

# Build a single platform binary with embedded binaries (requires prepare-embedded first)
build-graft-embedded os arch:
    CGO_ENABLED={{ if os == "darwin" { "1" } else { "0" } }} {{ GO_ENV_FLAGS }} GOOS={{ os }} GOARCH={{ arch }} go build {{ GO_BUILD_FLAGS }} -tags embed_binaries {{ LD_FLAGS }} -o {{ BIN_DIR }}/{{ DAEMON_NAME }}-{{ os }}-{{ arch }} {{ GRAFT_PKG }}

# Build and install graft for current host with embedded binaries for remote deployment
graft: prepare-embedded
    {{ GO_ENV_FLAGS }} go install {{ GO_BUILD_FLAGS }} -tags embed_binaries {{ GRAFT_PKG }}

# Build and install graft for current host with no embedded binaries (good for faster local development).

# This will build other arch/os binaries on demand.
graft-dev:
    {{ GO_ENV_FLAGS }} go install {{ GO_BUILD_FLAGS }} {{ GRAFT_PKG }}

# Build all deployment target binaries and compress for embedding. Darwin can
# only be cross-compiled with cgo from a darwin host, so non-darwin hosts skip

# the darwin daemon (the resulting graft binary can only push linux daemons).
[linux]
prepare-embedded:
    just --justfile {{ justfile() }} build-graft linux amd64
    just --justfile {{ justfile() }} build-graft linux arm64
    just --justfile {{ justfile() }} compress-binaries

[macos]
prepare-embedded:
    just --justfile {{ justfile() }} build-graft linux amd64
    just --justfile {{ justfile() }} build-graft linux arm64
    just --justfile {{ justfile() }} build-graft darwin arm64
    just --justfile {{ justfile() }} compress-binaries

# Compress binaries for embedding (requires binaries to exist in bin/)
compress-binaries:
    @mkdir -p {{ EMBED_DIR }}
    @if command -v parallel >/dev/null 2>&1; then \
        parallel --will-cite 'src="{{ BIN_DIR }}/{{ DAEMON_NAME }}-{}"; dst="{{ EMBED_DIR }}/{{ DAEMON_NAME }}-{}.zst"; if [ -f "$src" ]; then echo "Compressing $src -> $dst"; zstd -19 -f "$src" -o "$dst"; else echo "Warning: $src not found, skipping"; fi' ::: {{ PLATFORMS }}; \
    else \
        for platform in {{ PLATFORMS }}; do \
            src="{{ BIN_DIR }}/{{ DAEMON_NAME }}-$platform"; \
            dst="{{ EMBED_DIR }}/{{ DAEMON_NAME }}-$platform.zst"; \
            if [ -f "$src" ]; then \
                echo "Compressing $src -> $dst"; \
                zstd -19 -f "$src" -o "$dst"; \
            else \
                echo "Warning: $src not found, skipping"; \
            fi \
        done; \
    fi

# Release: build all platforms with embedded binaries
build-all-embedded: prepare-embedded
    just --justfile {{ justfile() }} build-graft-embedded linux amd64
    just --justfile {{ justfile() }} build-graft-embedded linux arm64
    just --justfile {{ justfile() }} build-graft-embedded darwin arm64

# Build all platform binaries (no embedding)
build-all:
    just --justfile {{ justfile() }} build-graft linux amd64
    just --justfile {{ justfile() }} build-graft linux arm64
    just --justfile {{ justfile() }} build-graft darwin arm64

protos-lint:
    buf format -w
    buf lint

protos: protos-lint
    rm -rf gen/proto/*
    buf generate

test *ARGS:
    go test -race {{ ARGS }} ./...

# Run tests inside a Linux Docker container (for local macOS development)
test-linux *ARGS:
    docker run --rm \
        -v {{ justfile_directory() }}:/src \
        -v graft-go-mod:/go/pkg/mod \
        -v graft-go-cache:/root/.cache/go-build \
        -w /src \
        golang:1.26 go test -race {{ ARGS }} ./...

go-lint:
    @for goos in {{ LINT_GOOS_LIST }}; do \
        echo "go fix (GOOS=$goos)..."; \
        CGO_ENABLED=0 GOOS=$goos go fix ./... || exit 1; \
    done
    go tool gofumpt -l -w $(go list -f '{{{{.Dir}}' ./... | grep -v /gen/)
    [ -f ./custom-gcl ] && [ ./custom-gcl -nt .custom-gcl.yml ] || go tool golangci-lint custom
    @for goos in {{ LINT_GOOS_LIST }}; do \
        echo "golangci-lint (GOOS=$goos)..."; \
        CGO_ENABLED=0 GOOS=$goos ./custom-gcl run -v --fix ./... || exit 1; \
    done

just-lint:
    just --fmt

actions-lint:
    actionlint

editors-lint:
    cd editors/vscode && npm install --silent && npm run compile

lint: protos-lint just-lint actions-lint go-lint editors-lint

ci-lint:
    just lint
    @if ! git diff --exit-code; then \
        echo ""; \
        echo "Linting produced changes. Run 'just lint' locally and commit the results."; \
        exit 1; \
    fi

[linux]
ci-build:
    just build-graft linux amd64
    just build-graft linux arm64

[macos]
ci-build:
    just build-graft linux amd64
    just build-graft linux arm64
    just build-graft darwin arm64

update-deps:
    go get -u ./...
    go mod tidy

# Release to GitHub
release version:
    ./scripts/release.sh "{{ version }}"

# Publish VS Code/Cursor extension (bump: patch, minor, or major)
release-extension bump="patch":
    cd editors/vscode && npm install && npm run compile
    cd editors/vscode && npm version {{ bump }} --no-git-tag-version
    cd editors/vscode && npx @vscode/vsce publish -p "$VSCE_PAT"
    cd editors/vscode && npx ovsx publish -p "$OVSX_PAT"
