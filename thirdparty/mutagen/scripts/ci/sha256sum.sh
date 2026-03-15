#!/bin/bash

# Exit immediately on failure.
set -e

# Move to the release directory.
pushd build/release > /dev/null

# Compute SHA256 digests.
sha256sum mutagen_* > SHA256SUMS

# Leave the release directory.
popd > /dev/null
