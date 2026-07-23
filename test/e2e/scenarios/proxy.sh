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

  # No-blip probe: hammer nginx on the TARGET (~100ms cadence), emitting one 'x'
  # per non-200/reset. The loop runs on the target via runc and stops when we
  # drop a sentinel on the target after the cutover attempt — so the window
  # spans exactly the deploy regardless of how long it takes (important over SSH,
  # where each dockrail command is slower). Both the loop and the sentinel live
  # on the target, so this works identically for CONN=local and CONN=ssh. The
  # seq cap (~60s) is only a runaway safety net.
  local hits stopf="/tmp/e2e-probe-stop.$$"
  hits="$(mktemp)"
  runc "rm -f $stopf"
  ( runc "for i in \$(seq 1 600); do [ -f $stopf ] && break; curl -fsS -m 2 http://localhost:${E2E_PORT}/health >/dev/null 2>&1 || echo x; sleep 0.1; done" ) > "$hits" 2>/dev/null &
  local probe_pid=$!
  sleep 1   # warm the probe before cutover

  gen_deploy_yml "$dy" "$ns" "$TARGET_DIR/compose-proxy.yml" proxy v2
  local v2_rc=0
  "$DOCKRAIL" -c "$dy" deploy || v2_rc=$?

  runc "touch $stopf"           # cutover attempt done → stop the probe
  wait "$probe_pid" 2>/dev/null || true
  runc "rm -f $stopf" || true
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
