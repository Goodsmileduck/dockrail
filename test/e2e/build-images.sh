#!/usr/bin/env bash
set -euo pipefail

# Builds the e2e test app at three tags. REPO lets the droplet tier retag for
# ghcr (e.g. ghcr.io/owner/dockrail-e2e-app); default is the local-only name.
REPO="${1:-dockrail-e2e-app}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

build() {
  local tag="$1" healthy="$2"
  docker build -q \
    --build-arg "VER=$tag" \
    --build-arg "HEALTHY=$healthy" \
    -t "${REPO}:${tag}" \
    "${DIR}/app" >/dev/null
  echo "built ${REPO}:${tag}"
}

build v1 1
build v2 1
build bad 0
