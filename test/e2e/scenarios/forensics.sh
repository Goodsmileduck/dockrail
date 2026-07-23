#!/usr/bin/env bash
# scenario_forensics: a NEW that never becomes ready must exit non-zero, leave a
# failure trail, and keep the failed container for inspection.
#
# NOTE on OLD availability: the "keep OLD serving" guarantee is a PROXY-strategy
# property (design sect. 6: "the invariant applies to the proxy strategy only").
# `recreate` is the deliberate-blip path — it stops OLD before starting NEW, so
# a failed recreate leaves OLD down. This scenario records that downtime as a
# FINDING rather than failing the PR gate; the hard assertions below are the
# guarantees dockrail actually makes for a failed recreate deploy.
scenario_forensics() {
  local ns="forensics-$$"
  local dy; dy="$(mktemp)"
  local app="http://localhost:${APP_PORT}"

  gen_deploy_yml "$dy" "$ns" "$TARGET_DIR/compose-recreate.yml" recreate v1
  "$DOCKRAIL" -c "$dy" deploy
  assert_version "$app/version" v1

  gen_deploy_yml "$dy" "$ns" "$TARGET_DIR/compose-recreate.yml" recreate bad
  if "$DOCKRAIL" -c "$dy" deploy; then
    echo "FAIL: bad deploy unexpectedly succeeded"; rm -f "$dy"; return 1
  fi
  echo "ok: bad deploy exited non-zero"

  # A failure record must be visible in history.
  "$DOCKRAIL" -c "$dy" audit | grep -q "failed@" || {
    echo "FAIL: no failed@ record in audit"; rm -f "$dy"; return 1; }
  echo "ok: audit shows a failed@ record"

  # The failed NEW container must be kept for inspection (not auto-removed).
  # Match the :bad image specifically so we assert the FAILED container is kept,
  # not merely that some "web" container lingers.
  runc "docker ps -a --filter name=web --format '{{.Image}}'" | grep -q ':bad$' || {
    echo "FAIL: failed NEW (:bad) container was not kept for inspection"; rm -f "$dy"; return 1; }
  echo "ok: failed NEW container kept for inspection"

  # OLD availability — a finding, not a gate (see NOTE above).
  if target_curl_ok "$app/health" && [ "$(target_curl "$app/version" 2>/dev/null || true)" = "v1" ]; then
    echo "note: OLD (v1) still served after failed recreate"
  else
    echo "note: FINDING — failed recreate left OLD down (downtime window); file a dockrail issue"
  fi

  runc "TAG=bad docker compose -f $TARGET_DIR/compose-recreate.yml down >/dev/null 2>&1" || true
  rm -f "$dy"
  echo "PASS scenario_forensics"
}
