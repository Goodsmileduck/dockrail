#!/usr/bin/env bash
# scenario_proxy: zero-downtime cutover through the nginx fixture.
# See KNOWN RISK in the plan/spec: the v2 (overlap) deploy is expected to fail
# at "start green" on a host-port collision, leaving v1 serving with no blip.
scenario_proxy() {
  local ns="proxy-$$"
  local dy; dy="$(mktemp)"
  local nginx="http://localhost:${E2E_PORT}"

  up_fixture "$ns"

  gen_deploy_yml "$dy" "$ns" "$TARGET_DIR/compose-proxy.yml" proxy v1
  "$DOCKRAIL" -c "$dy" deploy       # first deploy: single color, must succeed
  assert_version "$nginx/version" v1

  # No-blip probe: hammer nginx on the TARGET (~100ms cadence, ~12s window that
  # spans the cutover), emitting one 'x' per non-200/reset. The loop runs on the
  # target via runc; its stdout streams back to a runner-side file we count. A
  # fixed window (rather than a flag file) is used because the loop and the flag
  # would live on different hosts under CONN=ssh.
  local hits; hits="$(mktemp)"
  ( runc "for i in \$(seq 1 120); do curl -fsS -m 2 http://localhost:${E2E_PORT}/health >/dev/null 2>&1 || echo x; sleep 0.1; done" ) > "$hits" 2>/dev/null &
  local probe_pid=$!
  sleep 1   # warm the probe before cutover

  gen_deploy_yml "$dy" "$ns" "$TARGET_DIR/compose-proxy.yml" proxy v2
  local v2_rc=0
  "$DOCKRAIL" -c "$dy" deploy || v2_rc=$?

  wait "$probe_pid" 2>/dev/null || true
  # grep -c prints the count and exits 1 when it is 0; `|| true` swallows that
  # exit without appending a second value (a stray `echo 0` would corrupt the
  # integer test below).
  local fails; fails="$(grep -c x "$hits" 2>/dev/null || true)"

  echo "no-blip: $fails failed requests across the cutover window"
  [ "$fails" -eq 0 ] || { echo "FAIL: blip detected ($fails failures)"; down_fixture "$ns"; rm -f "$dy" "$hits"; return 1; }

  if [ "$v2_rc" -eq 0 ]; then
    assert_version "$nginx/version" v2
    echo "note: proxy overlap SUCCEEDED — the KNOWN RISK did not reproduce"
  else
    assert_version "$nginx/version" v1
    echo "note: KNOWN RISK reproduced — v2 overlap deploy failed (rc=$v2_rc), v1 still serving; file a dockrail issue"
  fi

  runc "TAG=v2 docker compose -f $TARGET_DIR/compose-proxy.yml down >/dev/null 2>&1" || true
  down_fixture "$ns"
  rm -f "$dy" "$hits"
  echo "PASS scenario_proxy"
}
