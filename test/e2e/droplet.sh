#!/usr/bin/env bash
set -euo pipefail

# Requires: doctl on PATH + DIGITALOCEAN_ACCESS_TOKEN in env.
# create also requires E2E_PUBKEY (an SSH public key string) and GITHUB_RUN_ID.
NAME="dockrail-ci-${GITHUB_RUN_ID:-local}"
REGION="${DO_REGION:-fra1}"
SIZE="${DO_SIZE:-s-1vcpu-1gb}"
IMAGE="${DO_IMAGE:-docker-20-04}"   # Marketplace image with Docker preinstalled

# The ssh-ready probe below runs before the workflow loads its ssh-agent, so it
# must point at the private key itself. SSH_KEY is the matching private key for
# E2E_PUBKEY; without it ssh has no identity and every probe fails.
SSH_KEY="${SSH_KEY:-}"
ssh_probe() {
  local opts=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=5)
  [ -n "$SSH_KEY" ] && opts+=(-i "$SSH_KEY" -o IdentitiesOnly=yes)
  ssh "${opts[@]}" "$@"
}

cmd_create() {
  local keyfp keyfile
  # doctl dropped --public-key (string) in favour of --public-key-file, so land
  # the key in a temp file the import can read.
  keyfile="$(mktemp)"
  printf '%s\n' "$E2E_PUBKEY" > "$keyfile"
  keyfp="$(doctl compute ssh-key import "$NAME" --public-key-file "$keyfile" \
             --output json | python3 -c 'import json,sys;print(json.load(sys.stdin)[0]["fingerprint"])')"
  rm -f "$keyfile"
  doctl compute droplet create "$NAME" \
    --region "$REGION" --size "$SIZE" --image "$IMAGE" \
    --ssh-keys "$keyfp" --tag-name dockrail-ci --wait >/dev/null
  local ip
  ip="$(doctl compute droplet get "$NAME" --format PublicIPv4 --no-header)"
  # Wait for sshd to accept the key (droplet "active" != ssh-ready).
  local ready=""
  for _ in $(seq 1 30); do
    if ssh_probe \
         "root@$ip" 'cloud-init status --wait >/dev/null 2>&1 || true; docker --version' >/dev/null 2>&1; then
      ready=1; break
    fi
    sleep 5
  done
  [ -n "$ready" ] || { echo "ERROR: droplet $NAME never became ssh-ready" >&2; return 1; }
  # dockrail requires the docker compose v2 PLUGIN (`docker compose`, no dash).
  # The DO marketplace image may only ship the classic `docker-compose` binary,
  # which would fail dockrail preflight — install the plugin if it is absent.
  ssh_probe "root@$ip" \
    'docker compose version >/dev/null 2>&1 || { apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y docker-compose-plugin; }' \
    >/dev/null 2>&1 || { echo "ERROR: could not ensure docker compose plugin on $NAME" >&2; return 1; }
  echo "IP=$ip"; return 0
}

cmd_destroy() {
  doctl compute droplet delete "$NAME" --force >/dev/null 2>&1 || true
  doctl compute ssh-key delete "$NAME" --force >/dev/null 2>&1 || true
  echo "destroyed $NAME"
}

case "${1:-}" in
  create)  cmd_create ;;
  destroy) cmd_destroy ;;
  *) echo "usage: droplet.sh create|destroy" >&2; exit 2 ;;
esac
