#!/usr/bin/env bash
set -euo pipefail
E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$E2E_DIR/lib.sh"

: "${DOCKRAIL:?set DOCKRAIL to the dockrail binary path}"

E2E_REGISTRY="dockrail-e2e-registry"

# reset_state tears down every container/network the harness may have left
# behind — so a fresh run starts clean and a scenario that aborts mid-way does
# not poison the next run (the compose project is shared across scenarios).
reset_state() {
  for f in compose-proxy.yml compose-recreate.yml; do
    runc "docker compose -f $E2E_DIR/$f down >/dev/null 2>&1" || true
  done
  runc "docker ps -aq --filter name=e2e-web | xargs -r docker rm -f >/dev/null 2>&1" || true
  runc "docker rm -f $E2E_NGINX >/dev/null 2>&1" || true
}

cleanup() {
  reset_state
  down_network
  if [ "${E2E_SKIP_BUILD:-0}" != "1" ]; then
    docker rm -f "$E2E_REGISTRY" >/dev/null 2>&1 || true
  fi
}

# Local tier: dockrail always runs `docker compose pull`, so local-only images
# can't be deployed. Stand up a throwaway registry, push the images to it, and
# point IMAGE at it — this also exercises dockrail's real pull path. The droplet
# tier sets E2E_SKIP_BUILD=1 and IMAGE to a ghcr repo it already pushed.
if [ "${E2E_SKIP_BUILD:-0}" != "1" ]; then
  docker rm -f "$E2E_REGISTRY" >/dev/null 2>&1 || true
  docker run -d --name "$E2E_REGISTRY" -p 127.0.0.1:5000:5000 registry:2 >/dev/null
  # Wait for the registry to accept pushes.
  for _ in $(seq 1 20); do
    curl -fsS http://127.0.0.1:5000/v2/ >/dev/null 2>&1 && break
    sleep 0.5
  done
  export IMAGE="127.0.0.1:5000/dockrail-e2e-app"
  "$E2E_DIR/build-images.sh" "$IMAGE"
  for t in v1 v2 bad; do docker push "$IMAGE:$t" >/dev/null; done
fi

up_network
trap cleanup EXIT
reset_state   # clean any leftovers from a prior aborted run

rc=0
for name in "$@"; do
  echo "=== scenario: $name ==="
  reset_state   # each scenario starts from a clean slate
  # shellcheck disable=SC1090
  source "$E2E_DIR/scenarios/$name.sh"
  # Run the scenario in a `set -e` subshell so a failed step (bad deploy, failed
  # assert_*) aborts it. We disable run.sh's own -e around the call and capture
  # the exit code: putting the subshell directly in an `if`/`||` would make it a
  # context where `set -e` is ignored, letting a failed step reach `echo PASS`.
  set +e
  ( set -e; "scenario_$name" )
  sc=$?
  set -e
  if [ "$sc" -ne 0 ]; then
    echo "FAIL scenario_$name"; rc=1
  fi
done
exit "$rc"
