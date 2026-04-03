#!/bin/sh
# release.sh - Create GitHub release with prebuilt binaries
# Usage: ./scripts/release.sh <version>
# Example: ./scripts/release.sh 0.1.0

set -e

# Configuration
REPO="edaniels/graft"
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

# Validate version argument
if [ -z "$1" ]; then
    err "Usage: $0 <version> (e.g., 0.1.0)"
fi

VERSION="$1"
TAG="v${VERSION}"

# Validate semver format (basic check)
if ! echo "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.]+)?$'; then
    err "Invalid version format: $VERSION (expected semver like 0.1.0 or 0.1.0-beta.1)"
fi

# Check for required tools
command -v gh >/dev/null 2>&1 || err "gh CLI is required but not installed"
command -v just >/dev/null 2>&1 || err "just is required but not installed"
command -v shasum >/dev/null 2>&1 || command -v sha256sum >/dev/null 2>&1 || err "shasum or sha256sum is required"

# Check gh authentication
if ! gh auth status >/dev/null 2>&1; then
    err "gh CLI is not authenticated. Run 'gh auth login' first."
fi

# Check for uncommitted changes (allow dirty working directory for dev releases)
IS_DEV=false
case "$VERSION" in *-dev*) IS_DEV=true;; esac

if [ -n "$(git status --porcelain)" ] && [ "$IS_DEV" = false ]; then
    err "Working directory is dirty. Commit or stash changes before releasing."
fi

# Check if release/tag already exists
if [ "$IS_DEV" = true ]; then
    # For dev releases, clean up existing release and tag
    if gh release view "$TAG" --repo "$REPO" >/dev/null 2>&1; then
        warn "Deleting existing release ${TAG}..."
        gh release delete "$TAG" --repo "$REPO" --yes
    fi
    if git rev-parse "$TAG" >/dev/null 2>&1; then
        git tag -d "$TAG"
    fi
    if git ls-remote --tags origin "$TAG" 2>/dev/null | grep -q "$TAG"; then
        git push origin --delete "$TAG"
    fi
else
    if gh release view "$TAG" --repo "$REPO" >/dev/null 2>&1; then
        err "Release $TAG already exists. Delete it first with: gh release delete $TAG --repo $REPO --yes"
    fi
    if git rev-parse "$TAG" >/dev/null 2>&1; then
        err "Tag $TAG already exists locally. Delete it first with: git tag -d $TAG"
    fi
    if git ls-remote --tags origin "$TAG" 2>/dev/null | grep -q "$TAG"; then
        err "Tag $TAG already exists on remote. Delete it first with: git push origin --delete $TAG"
    fi
fi

say "Creating and pushing tag ${TAG}..."
git tag "$TAG"
if ! git push origin "$TAG"; then
    git tag -d "$TAG"
    err "Failed to push tag to remote"
fi

# Cleanup tag if subsequent steps fail
cleanup_tag() {
    warn "Cleaning up tag ${TAG}..."
    git tag -d "$TAG" 2>/dev/null || true
    git push origin --delete "$TAG" 2>/dev/null || true
}
trap cleanup_tag EXIT

say "Building all platform binaries with embedded deployment support..."
if ! BUILD_VERSION="$TAG" just build-all-embedded; then
    err "Build failed"
fi

# Verify binaries exist and have correct version
for platform in $PLATFORMS; do
    binary="${BIN_DIR}/graft-${platform}"
    if [ ! -f "$binary" ]; then
        err "Binary not found: $binary"
    fi
done

say "Generating checksums..."
CHECKSUMS_FILE="${BIN_DIR}/checksums.txt"
rm -f "$CHECKSUMS_FILE"

# Generate SHA256 checksums
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

say "Generating version.txt..."
VERSION_FILE="${BIN_DIR}/version.txt"
echo "$TAG" > "$VERSION_FILE"

say "Generating release notes..."
NOTES_FILE="${BIN_DIR}/release-notes.txt"

git fetch --tags --quiet
PREV_TAG="$(git tag --list 'v[0-9]*' --sort=-version:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.]+)?$' | grep -v "^${TAG}$" | head -n 1)" || true

if [ -n "$PREV_TAG" ]; then
    say "Changelog: ${PREV_TAG}..${TAG}"
    git log --pretty=format:"- %s (%h)" "${PREV_TAG}..${TAG}" > "$NOTES_FILE"
else
    say "First release - using recent commits"
    git log --pretty=format:"- %s (%h)" -20 > "$NOTES_FILE"
fi

say "Creating GitHub release ${TAG}..."

say "Uploading release assets..."
PRERELEASE_FLAG=""
if [ "$IS_DEV" = true ]; then
    PRERELEASE_FLAG="--prerelease"
fi

if ! gh release create "$TAG" \
    --repo "$REPO" \
    --title "Graft ${TAG}" \
    --generate-notes \
    $PRERELEASE_FLAG \
    "${BIN_DIR}/graft-linux-amd64" \
    "${BIN_DIR}/graft-linux-arm64" \
    "${BIN_DIR}/graft-darwin-arm64" \
    "${CHECKSUMS_FILE}" \
    "${VERSION_FILE}" \
    "${NOTES_FILE}"; then
    err "Failed to create release"
fi

# Release succeeded - disarm the cleanup trap.
trap - EXIT

say "Release ${TAG} created successfully!"
echo ""
echo "View release: https://github.com/${REPO}/releases/tag/${TAG}"
echo ""
echo "Users can install with:"
echo "  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/scripts/install.sh | sh"
