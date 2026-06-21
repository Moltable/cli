#!/bin/sh
# Smoke-test the curl-served install.sh in a clean Ubuntu container.
#
# Usage (from repo root):
#   apps/cli/scripts/test-install.sh
#
# What it does:
#   - Boots ubuntu:latest with no extra setup.
#   - Installs curl + tar (the only runtime prereqs install.sh needs).
#   - Runs the on-disk install.sh against the published `cli-v*`
#     release flow and confirms `moltable version` exits 0.
#
# Notes:
#   - This is a release-time smoke test, NOT part of `go test ./...`.
#     It needs Docker on the host and reaches the real GitHub releases
#     API, so it's wired into the post-tag CI flow (and useful as a
#     local sanity check before cutting a release).
#   - To pin a specific version: MOLTABLE_VERSION=0.1.0 ./test-install.sh

set -eu

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
INSTALL_SH="${REPO_ROOT}/install.sh"

if [ ! -f "$INSTALL_SH" ]; then
    printf 'error: install.sh not found at %s\n' "$INSTALL_SH" >&2
    exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
    printf 'error: docker is required to run this smoke test.\n' >&2
    exit 1
fi

# -e MOLTABLE_VERSION lets a caller pin a release for testing.
DOCKER_ENV=""
if [ -n "${MOLTABLE_VERSION:-}" ]; then
    DOCKER_ENV="-e MOLTABLE_VERSION=${MOLTABLE_VERSION}"
fi

printf 'Running install.sh inside ubuntu:latest…\n'

# shellcheck disable=SC2086
docker run --rm \
    $DOCKER_ENV \
    -v "${INSTALL_SH}:/install.sh:ro" \
    ubuntu:latest \
    bash -c '
        set -eu
        apt-get update -qq >/dev/null
        apt-get install -y -qq curl ca-certificates tar >/dev/null
        sh /install.sh
        moltable version
    '

printf 'install.sh smoke test passed.\n'
