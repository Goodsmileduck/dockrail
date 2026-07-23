#!/usr/bin/env bash
set -euo pipefail

# Requires: doctl on PATH + DIGITALOCEAN_ACCESS_TOKEN in env.
# create also requires E2E_PUBKEY (an SSH public key string) and GITHUB_RUN_ID.
NAME="dockrail-ci-${GITHUB_RUN_ID:-local}"
REGION="${DO_REGION:-fra1}"
SIZE="${DO_SIZE:-s-1vcpu-1gb}"
IMAGE="${DO_IMAGE:-docker-20-04}"   # Marketplace image with Docker preinstalled

cmd_create() {
  local keyfp
  keyfp="$(doctl compute ssh-key import "$NAME" --public-key "$E2E_PUBKEY" \
             --output json | python3 -c 'import json,sys;print(json.load(sys.stdin)[0]["fingerprint"])')"
  doctl compute droplet create "$NAME" \
    --region "$REGION" --size "$SIZE" --image "$IMAGE" \
    --ssh-keys "$keyfp" --tag-name dockrail-ci --wait >/dev/null
  local ip
  ip="$(doctl compute droplet get "$NAME" --format PublicIPv4 --no-header)"
  # Wait for sshd to accept the key (droplet "active" != ssh-ready).
  for _ in $(seq 1 30); do
    if ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 \
         "root@$ip" 'cloud-init status --wait >/dev/null 2>&1 || true; docker --version' >/dev/null 2>&1; then
      echo "IP=$ip"; return 0
    fi
    sleep 5
  done
  echo "ERROR: droplet $NAME never became ssh-ready" >&2; return 1
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
