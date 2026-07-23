#!/usr/bin/env bash
# Sourced by scenario scripts. Requires: DOCKRAIL (path to binary), CONN
# (local|ssh), and when CONN=ssh, SSH_TARGET (user@host).
set -euo pipefail

E2E_NET="dockrail-e2e-net"
E2E_NGINX="dockrail-e2e-nginx"
E2E_PORT="18080"   # nginx host port (external probe target)
APP_PORT="8080"    # app container port == readiness.port
E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CONN="${CONN:-local}"

# TARGET_DIR is where the compose files + fixture live ON THE DEPLOY TARGET.
# For local exec that is this repo dir; for the droplet tier the workflow syncs
# them to a droplet path and sets TARGET_DIR to it. dockrail runs
# `docker compose -f <compose>` on the target, and the nginx fixture bind-mounts
# from here, so these paths must resolve on the target, not the runner.
TARGET_DIR="${TARGET_DIR:-$E2E_DIR}"

# runc runs a command on the deploy target. Local exec mirrors what dockrail
# itself does; ssh mirrors the real transport.
runc() {
  if [ "$CONN" = "ssh" ]; then
    ssh -o StrictHostKeyChecking=accept-new "$SSH_TARGET" "$1"
  else
    bash -c "$1"
  fi
}

up_network() {
  runc "docker network inspect $E2E_NET >/dev/null 2>&1 || docker network create $E2E_NET"
}

down_network() {
  runc "docker network rm $E2E_NET >/dev/null 2>&1 || true"
}

up_fixture() {
  local project="$1"
  # Seed the upstream with a self-resolving placeholder so nginx always starts
  # cleanly even before any app container exists (returns 502 until the first
  # dockrail proxy deploy rewrites this to web-<color> and reloads). Seeding
  # `server web-blue:8080` instead would make nginx start against an
  # unresolvable host — environment-dependent and racy.
  runc "mkdir -p \$HOME/.dockrail/$project/nginx && \
        printf 'upstream web { server 127.0.0.1:%s; }\n' $APP_PORT > \$HOME/.dockrail/$project/nginx/web.conf"
  runc "docker rm -f $E2E_NGINX >/dev/null 2>&1 || true"
  runc "docker run -d --name $E2E_NGINX --network $E2E_NET -p $E2E_PORT:80 \
          -v \$HOME/.dockrail/$project/nginx:/etc/nginx/dockrail:ro \
          -v $TARGET_DIR/fixture/nginx.conf:/etc/nginx/nginx.conf:ro \
          nginx:1.27-alpine"
}

down_fixture() {
  local project="$1"
  runc "docker rm -f $E2E_NGINX >/dev/null 2>&1 || true"
  runc "rm -rf \$HOME/.dockrail/$project"
}

gen_deploy_yml() {
  local file="$1" project="$2" compose="$3" strategy="$4" tag="$5"
  {
    echo "project: $project"
    echo "compose: $compose"
    [ "$CONN" = "ssh" ] && echo "target: { host: \"$SSH_TARGET\" }"
    echo "services:"
    echo "  web:"
    echo "    image_tag: \"$tag\""
    echo "    readiness: { type: http, path: /health, port: $APP_PORT, timeout: 30s }"
    if [ "$strategy" = "proxy" ]; then
      echo "    cutover: { strategy: proxy, proxy: $E2E_NGINX }"
    else
      echo "    cutover: { strategy: recreate }"
    fi
  } > "$file"
}

# target_curl runs curl ON THE DEPLOY TARGET, so probes hit the host where the
# app/nginx ports are actually published (the droplet under CONN=ssh, the runner
# under CONN=local) — the published ports live on the target's localhost.
target_curl() { runc "curl -fsS -m 5 '$1'"; }

# target_curl_ok reports success/failure of a target-side curl without output.
target_curl_ok() { runc "curl -fsS -m 5 '$1' >/dev/null 2>&1"; }

assert_version() {
  local url="$1" want="$2" got
  got="$(target_curl "$url")" || { echo "FAIL: $url unreachable"; return 1; }
  [ "$got" = "$want" ] || { echo "FAIL: $url version=$got want=$want"; return 1; }
  echo "ok: $url == $want"
}

assert_no_health() {
  local url="$1"
  if target_curl_ok "$url"; then
    echo "FAIL: $url unexpectedly served /health"; return 1
  fi
  echo "ok: $url has no /health"
}
