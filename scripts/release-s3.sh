#!/bin/sh
# release-s3.sh - Build and publish graft binaries to an S3 bucket
# Usage: ./scripts/release-s3.sh <version> <s3-path>
# Example: ./scripts/release-s3.sh 0.3.0 s3://my-bucket/graft-releases
#
# The S3 path doubles as the update URL. Binaries built by this script
# have the HTTPS equivalent baked in via ldflags so they automatically
# check this bucket for future updates.

set -e

# Configuration
BIN_DIR="bin"
PLATFORMS="linux-amd64 linux-arm64 darwin-arm64"

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

warn() {
    printf "${YELLOW}warning:${NC} %s\n" "$1"
}

err() {
    printf "${RED}error:${NC} %s\n" "$1" >&2
    exit 1
}

# Validate arguments
if [ -z "$1" ] || [ -z "$2" ]; then
    err "Usage: $0 <version> <s3-path>
  Example: $0 0.3.0 s3://my-bucket/graft-releases"
fi

VERSION="$1"
S3_PATH="${2%/}"  # strip trailing slash
TAG="v${VERSION}"

# Validate semver format
if ! echo "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.]+)?$'; then
    err "Invalid version format: $VERSION (expected semver like 0.3.0 or 0.3.0-beta.1)"
fi

# Check for uncommitted changes
if [ -n "$(git status --porcelain)" ]; then
    err "Working directory is dirty. Commit or stash changes before releasing."
fi

# Check for required tools
command -v aws >/dev/null 2>&1 || err "aws CLI is required but not installed"
command -v just >/dev/null 2>&1 || err "just is required but not installed"
command -v shasum >/dev/null 2>&1 || command -v sha256sum >/dev/null 2>&1 || err "shasum or sha256sum is required"

# Derive the HTTPS URL from the S3 path for the build-time update URL.
# s3://bucket-name/prefix -> https://bucket-name.s3.amazonaws.com/prefix
S3_BUCKET="$(echo "$S3_PATH" | sed 's|^s3://||' | cut -d/ -f1)"
S3_PREFIX="$(echo "$S3_PATH" | sed 's|^s3://[^/]*/||')"
if [ "$S3_PREFIX" = "$S3_PATH" ]; then
    # No prefix (just s3://bucket)
    S3_PREFIX=""
fi

if [ -n "$AWS_REGION" ]; then
    HTTPS_BASE="https://${S3_BUCKET}.s3.${AWS_REGION}.amazonaws.com"
else
    HTTPS_BASE="https://${S3_BUCKET}.s3.amazonaws.com"
fi

if [ -n "$S3_PREFIX" ]; then
    HTTPS_URL="${HTTPS_BASE}/${S3_PREFIX}"
else
    HTTPS_URL="${HTTPS_BASE}"
fi

say "S3 path:    ${S3_PATH}"
say "Update URL: ${HTTPS_URL}"
say "Version:    ${TAG}"

say "Building all platform binaries with embedded deployment support..."
if ! BUILD_VERSION="$TAG" BUILD_UPDATE_URL="$HTTPS_URL" just build-all-embedded; then
    err "Build failed"
fi

# Verify binaries exist
for platform in $PLATFORMS; do
    binary="${BIN_DIR}/graft-${platform}"
    if [ ! -f "$binary" ]; then
        err "Binary not found: $binary"
    fi
done

say "Generating checksums..."
CHECKSUMS_FILE="${BIN_DIR}/checksums.txt"
rm -f "$CHECKSUMS_FILE"

(
    cd "$BIN_DIR"
    for platform in $PLATFORMS; do
        binary="graft-${platform}"
        if command -v sha256sum >/dev/null 2>&1; then
            sha256sum "$binary" >> checksums.txt
        else
            shasum -a 256 "$binary" >> checksums.txt
        fi
    done
)

say "Uploading versioned artifacts to ${S3_PATH}/${TAG}/..."
for platform in $PLATFORMS; do
    binary="${BIN_DIR}/graft-${platform}"
    aws s3 cp "$binary" "${S3_PATH}/${TAG}/graft-${platform}"
done
aws s3 cp "$CHECKSUMS_FILE" "${S3_PATH}/${TAG}/checksums.txt"

say "Updating latest pointer..."
echo "$TAG" | aws s3 cp - "${S3_PATH}/latest" --content-type "text/plain"

say "Release ${TAG} published to ${S3_PATH}!"
echo ""
echo "Update URL for users:"
echo "  export GRAFT_UPDATE_URL=\"${HTTPS_URL}\""
echo ""
echo "Or users with this build will auto-update from:"
echo "  ${HTTPS_URL}"
