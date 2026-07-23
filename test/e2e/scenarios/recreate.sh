#!/usr/bin/env bash
# scenario_recreate: v1 -> v2 via the recreate strategy.
scenario_recreate() {
  local ns="recreate-$$"
  local dy; dy="$(mktemp)"
  local app="http://localhost:${APP_PORT}"

  gen_deploy_yml "$dy" "$ns" "$E2E_DIR/compose-recreate.yml" recreate v1
  "$DOCKRAIL" -c "$dy" deploy
  assert_version "$app/version" v1

  gen_deploy_yml "$dy" "$ns" "$E2E_DIR/compose-recreate.yml" recreate v2
  "$DOCKRAIL" -c "$dy" deploy
  assert_version "$app/version" v2

  TAG=v2 docker compose -f "$E2E_DIR/compose-recreate.yml" down >/dev/null 2>&1 || true
  rm -f "$dy"
  echo "PASS scenario_recreate"
}
