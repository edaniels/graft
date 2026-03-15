#!/bin/sh
# install.sh - Install graft binary from GitHub Releases
#
# Usage:
#   curl -fsSL https://graft.run/install.sh | sh
#
# Options:
#   --install-dir <dir>  Install to specified directory (default: ~/.local/bin)
#   --version <version>  Install specific version (default: latest)
#   --help               Show this help message
#
# Environment variables:
#   GRAFT_INSTALL_DIR    Alternative to --install-dir flag

set -e

# Configuration
REPO="edaniels/graft"
BINARY_NAME="graft"
DEFAULT_INSTALL_DIR="${HOME}/.local/bin"

# Colors (disabled if not a tty)
if [ -t 1 ]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    NC='\033[0m'
else
    RED=''
    GREEN=''
    YELLOW=''
    NC=''
fi

say() {
    printf "${GREEN}==>${NC} %s\n" "$1"
}

say_verbose() {
    printf "    %s\n" "$1"
}

err() {
    printf "${RED}error:${NC} %s\n" "$1" >&2
    exit 1
}

need_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        err "need '$1' (command not found)"
    fi
}

# Detect OS
detect_os() {
    os="$(uname -s)"
    case "$os" in
        Linux) echo "linux" ;;
        Darwin) echo "darwin" ;;
        *) err "Unsupported operating system: $os" ;;
    esac
}

# Detect architecture
detect_arch() {
    arch="$(uname -m)"
    case "$arch" in
        x86_64|amd64) echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) err "Unsupported architecture: $arch" ;;
    esac
}

# Verify SHA256 checksum
verify_checksum() {
    file="$1"
    expected="$2"

    if command -v sha256sum >/dev/null 2>&1; then
        actual="$(sha256sum "$file" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
        actual="$(shasum -a 256 "$file" | awk '{print $1}')"
    else
        printf "${YELLOW}warning:${NC} %s\n" "Neither sha256sum nor shasum found, skipping checksum verification" >&2
        return 0
    fi

    if [ "$actual" != "$expected" ]; then
        err "Checksum verification failed for $file
  Expected: $expected
  Actual:   $actual"
    fi
}

# Download file using curl or wget (with optional auth header)
github_download() {
    url="$1"
    output="$2"
    auth_header=""

    if [ -n "$GITHUB_TOKEN" ]; then
        auth_header="Authorization: token $GITHUB_TOKEN"
    fi

    if command -v curl >/dev/null 2>&1; then
        if [ -n "$auth_header" ]; then
            if ! curl -fsSL -H "$auth_header" -H "Accept: application/octet-stream" -o "$output" "$url"; then
                err "Failed to download: $url"
            fi
        else
            if ! curl -fsSL -H "Accept: application/octet-stream" -o "$output" "$url"; then
                err "Failed to download: $url"
            fi
        fi
    elif command -v wget >/dev/null 2>&1; then
        if [ -n "$auth_header" ]; then
            if ! wget -q --header="$auth_header" --header="Accept: application/octet-stream" -O "$output" "$url"; then
                err "Failed to download: $url"
            fi
        else
            if ! wget -q --header="Accept: application/octet-stream" -O "$output" "$url"; then
                err "Failed to download: $url"
            fi
        fi
    else
        err "need 'curl' or 'wget' (neither found)"
    fi
}

# Download JSON from GitHub API
github_download_json() {
    url="$1"
    auth_header=""

    if [ -n "$GITHUB_TOKEN" ]; then
        auth_header="Authorization: token $GITHUB_TOKEN"
    fi

    if command -v curl >/dev/null 2>&1; then
        if [ -n "$auth_header" ]; then
            curl -fsSL -H "$auth_header" -H "Accept: application/vnd.github+json" "$url"
        else
            curl -fsSL -H "Accept: application/vnd.github+json" "$url"
        fi
    elif command -v wget >/dev/null 2>&1; then
        if [ -n "$auth_header" ]; then
            wget -q --header="$auth_header" --header="Accept: application/vnd.github+json" -O - "$url"
        else
            wget -q --header="Accept: application/vnd.github+json" -O - "$url"
        fi
    else
        err "need 'curl' or 'wget' (neither found)"
    fi
}

# Get the latest release tag from GitHub
github_get_latest_version() {
    api_url="https://api.github.com/repos/${REPO}/releases/latest"
    response="$(github_download_json "$api_url" 2>/dev/null)" || err "Failed to fetch latest release."

    echo "$response" | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/'
}

# Get asset download URL from GitHub release
github_get_asset_url() {
    tag="$1"
    asset_name="$2"

    api_url="https://api.github.com/repos/${REPO}/releases/tags/${tag}"
    response="$(github_download_json "$api_url" 2>/dev/null)" || err "Failed to fetch release ${tag}"

    asset_id="$(echo "$response" | grep -B5 "\"name\"[[:space:]]*:[[:space:]]*\"${asset_name}\"" | grep '"id"' | head -1 | sed 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/')"

    if [ -z "$asset_id" ]; then
        err "Asset not found: $asset_name"
    fi

    echo "https://api.github.com/repos/${REPO}/releases/assets/${asset_id}"
}

show_help() {
    cat << EOF
Install graft - Local-first remote development platform

Usage: install.sh [OPTIONS]

Options:
    --install-dir <dir>  Install to specified directory (default: ~/.local/bin)
    --version <version>  Install specific version tag (default: latest)
    --help               Show this help message

Environment variables:
    GRAFT_INSTALL_DIR    Alternative to --install-dir flag

Examples:
    # Install latest version
    curl -fsSL https://graft.run/install.sh | sh

    # Install specific version to custom directory
    curl -fsSL https://graft.run/install.sh | sh -s -- --version v0.1.0 --install-dir /usr/local/bin
EOF
}

main() {
    # Parse arguments
    INSTALL_DIR="${GRAFT_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"
    VERSION=""

    while [ $# -gt 0 ]; do
        case "$1" in
            --install-dir)
                if [ -z "${2:-}" ]; then
                    err "--install-dir requires a directory argument"
                fi
                INSTALL_DIR="$2"
                shift 2
                ;;
            --version)
                if [ -z "${2:-}" ]; then
                    err "--version requires a version argument"
                fi
                VERSION="$2"
                shift 2
                ;;
            --help|-h)
                show_help
                exit 0
                ;;
            *)
                err "Unknown option: $1"
                ;;
        esac
    done

    # Use token if available (for higher rate limits).
    GITHUB_TOKEN="${GRAFT_GITHUB_TOKEN:-${GITHUB_TOKEN:-}}"

    # Detect platform
    OS="$(detect_os)"
    ARCH="$(detect_arch)"
    PLATFORM="${OS}-${ARCH}"

    say "Detected platform: ${PLATFORM}"

    # Check for supported platform
    case "$PLATFORM" in
        linux-amd64|linux-arm64|darwin-arm64)
            ;;
        darwin-amd64)
            err "macOS Intel (darwin-amd64) is not supported. Only Apple Silicon (darwin-arm64) is available."
            ;;
        *)
            err "Unsupported platform: $PLATFORM"
            ;;
    esac

    # Get version
    if [ -z "$VERSION" ]; then
        say "Fetching latest version..."
        VERSION="$(github_get_latest_version)"
        if [ -z "$VERSION" ]; then
            err "Could not determine latest version"
        fi
    fi

    say "Installing graft ${VERSION}..."

    # Create temp directory
    TMP_DIR="$(mktemp -d)"
    trap 'rm -rf "$TMP_DIR"' EXIT

    BINARY_NAME_PLATFORM="graft-${PLATFORM}"

    # Download checksums and binary
    say "Downloading checksums..."
    CHECKSUMS_URL="$(github_get_asset_url "$VERSION" "checksums.txt")"
    github_download "$CHECKSUMS_URL" "$TMP_DIR/checksums.txt"

    say "Downloading ${BINARY_NAME_PLATFORM}..."
    BINARY_URL="$(github_get_asset_url "$VERSION" "${BINARY_NAME_PLATFORM}")"
    github_download "$BINARY_URL" "$TMP_DIR/${BINARY_NAME_PLATFORM}"

    # Get expected checksum for our binary
    EXPECTED_CHECKSUM="$(grep "${BINARY_NAME_PLATFORM}$" "$TMP_DIR/checksums.txt" | awk '{print $1}')"

    if [ -z "$EXPECTED_CHECKSUM" ]; then
        err "Could not find checksum for ${BINARY_NAME_PLATFORM}"
    fi

    # Verify checksum
    say "Verifying checksum..."
    verify_checksum "$TMP_DIR/${BINARY_NAME_PLATFORM}" "$EXPECTED_CHECKSUM"
    say_verbose "Checksum OK: ${EXPECTED_CHECKSUM}"

    # Install binary
    say "Installing to ${INSTALL_DIR}..."
    mkdir -p "$INSTALL_DIR"
    mv "$TMP_DIR/${BINARY_NAME_PLATFORM}" "$INSTALL_DIR/graft"
    chmod +x "$INSTALL_DIR/graft"
    ln -sf "$INSTALL_DIR/graft" "$INSTALL_DIR/gr"

    say "Installation complete!"
    echo ""

    SHELL_NAME="$(basename "${SHELL:-/bin/sh}")"

    # Check if install dir is in PATH
    case ":$PATH:" in
        *":$INSTALL_DIR:"*)
            ;;
        *)
            echo "${YELLOW}Note:${NC} ${INSTALL_DIR} is not in your PATH."
            echo ""
            echo "Add it to your shell configuration:"
            echo ""
            case "$SHELL_NAME" in
                bash)
                    echo "    echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.bashrc"
                    ;;
                zsh)
                    echo "    echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.zshrc"
                    ;;
                fish)
                    echo "    fish_add_path ~/.local/bin"
                    ;;
                *)
                    echo "    export PATH=\"\$HOME/.local/bin:\$PATH\""
                    ;;
            esac
            echo ""
            ;;
    esac

    echo "To enable shell integration, add to your shell config:"
    echo ""
    case "$SHELL_NAME" in
        bash)
            echo "    echo 'eval \"\$(graft activate bash)\"' >> ~/.bashrc"
            echo ""
            echo "Then: source ~/.bashrc"
            ;;
        zsh)
            echo "    echo 'eval \"\$(graft activate zsh)\"' >> ~/.zshrc"
            echo ""
            echo "Then: source ~/.zshrc"
            ;;
        fish)
            echo "    echo 'graft activate fish | source' >> ~/.config/fish/config.fish"
            echo ""
            echo "Then: source ~/.config/fish/config.fish"
            ;;
        *)
            echo "    eval \"\$(graft activate \$SHELL)\""
            ;;
    esac
    echo ""

    if [ "$OS" = "darwin" ]; then
        echo "To have the daemon start automatically on login:"
        echo ""
        echo "    graft daemon service install"
        echo ""
    fi

    echo "Run 'graft --help' (or 'gr --help') to get started."
}

main "$@"
