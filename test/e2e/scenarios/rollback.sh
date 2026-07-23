#!/usr/bin/env bash
# scenario_rollback: after v2, `rollback` must restore v1.
scenario_rollback() {
  local ns="rollback-$$"
  local dy; dy="$(mktemp)"
  local app="http://localhost:${APP_PORT}"

  gen_deploy_yml "$dy" "$ns" "$TARGET_DIR/compose-recreate.yml" recreate v1
  "$DOCKRAIL" -c "$dy" deploy
  gen_deploy_yml "$dy" "$ns" "$TARGET_DIR/compose-recreate.yml" recreate v2
  "$DOCKRAIL" -c "$dy" deploy
  assert_version "$app/version" v2

  # rollback re-points to the previously running tag (v1).
  "$DOCKRAIL" -c "$dy" rollback
  assert_version "$app/version" v1

  runc "TAG=v1 docker compose -f $TARGET_DIR/compose-recreate.yml down >/dev/null 2>&1" || true
  rm -f "$dy"
  echo "PASS scenario_rollback"
}
