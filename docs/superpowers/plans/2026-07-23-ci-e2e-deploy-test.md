# CI End-to-End Deploy Test Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run `dockrail`'s four deploy scenarios (recreate, rollback, failed-deploy forensics, proxy zero-downtime cutover) end-to-end against real Docker — cheaply on every PR via local-exec, and against a real DigitalOcean droplet on manual dispatch — then tear everything down.

**Architecture:** One transport-parametrized bash harness under `test/e2e/` runs identical scenario assertions with `CONN=local` (runner's own Docker) or `CONN=ssh` (a droplet over real SSH). A tiny `busybox httpd` app image tagged `:v1`/`:v2`/`:bad` makes cutovers a provable version swap. An nginx fixture container provides the "existing nginx" dockrail's proxy strategy drives. Two GitHub workflows: `e2e.yml` (local tier every PR + droplet tier on dispatch) and `e2e-reap.yml` (scheduled droplet-leak backstop). No change to the shipped `dockrail` binary.

**Tech Stack:** Bash, Docker + docker compose v2, nginx (`busybox`/`nginx:alpine`), `doctl` (DigitalOcean CLI), GitHub Actions, ghcr.io.

## Global Constraints

- **Test/CI only.** No edits to `dockrail` Go source. All new files live under `test/e2e/` and `.github/workflows/`.
- **Public repo hygiene.** Use `example.com` and `ghcr.io/${{ github.repository_owner }}/…` placeholders. Never hardcode a real host, IP, or owner literal in committed files.
- **dockrail invariants that shape the fixtures** (verified against source):
  - `http` readiness runs `curl -fsS -m 5 http://localhost:<readiness.port><path>` **on the connection host** (`strategy/readiness/http.go:31`). The probed container must publish `<readiness.port>:<readiness.port>` on the host.
  - Proxy cutover writes `upstream <svc> { server <svc>-<color>:<readiness.port>; }` to `$HOME/.dockrail/<project>/nginx/<svc>.conf` and reloads via `docker exec <cutover.proxy> nginx -s reload` (`engine/nginx.go`). So `readiness.port` is **both** the host-published port (readiness curl) and the container port (nginx upstream) — they are the same number.
  - Proxy uses compose services `<svc>-blue` / `<svc>-green` (`engine/bluegreen.go:66`); recreate uses a plain `<svc>` service (`engine/engine.go:246`). First proxy deploy has `active=""` → brings up `<svc>-blue` and flips the upstream to it.
  - The image tag varies via the `TAG` env var dockrail prepends to `docker compose` (`TAG=<tag> docker compose -f <compose> …`). Compose reads `${TAG}`.
  - `deploy.yml` interpolation is `${vars.name}` only — **no env interpolation** (`config/vars.go`). The harness therefore generates each `deploy.yml` by heredoc; it does not rely on interpolation for host/tag.
  - `target.host` empty ⇒ local exec; `user@host` ⇒ SSH (`config/config.go:37`).
- **KNOWN RISK — proxy scenario (Task 6):** In the zero-gap path blue and green run simultaneously, but both must publish the same `readiness.port` on the host, which Docker forbids. The v1 initial deploy (one color) succeeds; the v2 deploy is expected to **fail at "start green" with a port-allocation error**, leaving v1 serving (no blip). Task 6 asserts exactly this reproducer and, if confirmed, the implementer files a dockrail issue — it is **not** a fixture bug to "fix." See `docs/specs/2026-07-23-ci-e2e-deploy-test-design.md`.
- Match existing repo style. Bash scripts start with `#!/usr/bin/env bash` and `set -euo pipefail`. Do NOT `git commit`/`push` beyond the per-task commits this plan specifies.
- Commit message trailer for every commit: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

---

### Task 1: Test app image + build script

**Files:**
- Create: `test/e2e/app/Dockerfile`
- Create: `test/e2e/build-images.sh`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - Docker images `dockrail-e2e-app:v1`, `:v2`, `:bad` (built locally).
  - `test/e2e/build-images.sh [REPO]` — builds the three tags. `REPO` defaults to `dockrail-e2e-app`; when set (e.g. `ghcr.io/owner/dockrail-e2e-app`) it tags for that repo so Task 9 can push. Exposes `GET /health` → `200` (`:v1`/`:v2`), `GET /version` → the tag string, and `:bad` has **no** `/health` (readiness never passes).

- [ ] **Step 1: Write the failing test**

Create `test/e2e/build-images.sh`:

```bash
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
```

Make it executable and try to run it — it must fail because the Dockerfile does not exist yet:

Run: `chmod +x test/e2e/build-images.sh && test/e2e/build-images.sh`
Expected: FAIL — `docker build` errors, no such file `test/e2e/app/Dockerfile`.

- [ ] **Step 2: Confirm the failure**

Run: `test/e2e/build-images.sh; echo "exit=$?"`
Expected: non-zero exit; error mentions the missing `app` build context / Dockerfile.

- [ ] **Step 3: Write minimal implementation**

Create `test/e2e/app/Dockerfile`:

```dockerfile
FROM busybox:1.36
ARG VER=dev
ARG HEALTHY=1
# /www/version always reflects the build tag so a cutover is a provable swap.
# /www/health exists only when HEALTHY=1; the :bad image omits it, so
# `curl -fsS /health` returns non-2xx and dockrail readiness never passes.
RUN mkdir -p /www \
 && printf '%s' "$VER" > /www/version \
 && if [ "$HEALTHY" = "1" ]; then printf 'ok' > /www/health; fi
EXPOSE 8080
CMD ["httpd", "-f", "-v", "-p", "8080", "-h", "/www"]
```

- [ ] **Step 4: Run to verify it passes**

Run:
```bash
test/e2e/build-images.sh
docker run -d --name e2e_probe -p 18099:8080 dockrail-e2e-app:v1
sleep 1
curl -fsS http://localhost:18099/health && echo
test "$(curl -fsS http://localhost:18099/version)" = "v1" && echo "version ok"
docker rm -f e2e_probe
# :bad must NOT serve /health
docker run -d --name e2e_bad -p 18099:8080 dockrail-e2e-app:bad
sleep 1
curl -fsS http://localhost:18099/health && echo "UNEXPECTED: bad served health" || echo "bad has no health (correct)"
docker rm -f e2e_bad
```
Expected: `ok`, `version ok`, `bad has no health (correct)`.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/app/Dockerfile test/e2e/build-images.sh
git commit -m "test(e2e): busybox app image at v1/v2/bad tags

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Shared harness library + fixtures

**Files:**
- Create: `test/e2e/fixture/nginx.conf`
- Create: `test/e2e/fixture/compose-fixture.yml`
- Create: `test/e2e/compose-recreate.yml`
- Create: `test/e2e/compose-proxy.yml`
- Create: `test/e2e/lib.sh`

**Interfaces:**
- Consumes: images from Task 1.
- Produces (sourced from `lib.sh`):
  - `E2E_NET=dockrail-e2e-net`, `E2E_NGINX=dockrail-e2e-nginx`, `E2E_PORT=18080` (nginx host port), `APP_PORT=8080` (app container+readiness port).
  - `runc "<cmd>"` — run a shell command on the target: locally when `CONN=local`, else `ssh $SSH_TARGET "<cmd>"`. Central place transport is chosen.
  - `up_network` / `down_network` — create/remove the shared external docker network (idempotent).
  - `up_fixture <project>` — create `$HOME/.dockrail/<project>/nginx/web.conf` seeded with `upstream web { server web-blue:8080; }`, then start the nginx fixture container attached to `$E2E_NET`, publishing `$E2E_PORT:80`, mounting the upstream dir at `/etc/nginx/dockrail`.
  - `down_fixture <project>` — stop+remove nginx and delete the project's `.dockrail` dir.
  - `gen_deploy_yml <file> <project> <compose> <strategy> <tag>` — write a `deploy.yml`. Adds `target: { host: "$SSH_TARGET" }` only when `CONN=ssh`; adds `cutover.proxy: dockrail-e2e-nginx` only when `strategy=proxy`.
  - `assert_version <url> <want>` / `assert_no_health <url>` — curl assertions that exit non-zero on mismatch.

- [ ] **Step 1: Write the failing test**

Create `test/e2e/lib.sh`:

```bash
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
  runc "mkdir -p \$HOME/.dockrail/$project/nginx && \
        printf 'upstream web { server web-blue:%s; }\n' $APP_PORT > \$HOME/.dockrail/$project/nginx/web.conf"
  runc "docker rm -f $E2E_NGINX >/dev/null 2>&1 || true"
  runc "docker run -d --name $E2E_NGINX --network $E2E_NET -p $E2E_PORT:80 \
          -v \$HOME/.dockrail/$project/nginx:/etc/nginx/dockrail:ro \
          -v $E2E_DIR/fixture/nginx.conf:/etc/nginx/nginx.conf:ro \
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

assert_version() {
  local url="$1" want="$2" got
  got="$(curl -fsS -m 5 "$url")" || { echo "FAIL: $url unreachable"; return 1; }
  [ "$got" = "$want" ] || { echo "FAIL: $url version=$got want=$want"; return 1; }
  echo "ok: $url == $want"
}

assert_no_health() {
  local url="$1"
  if curl -fsS -m 5 "$url" >/dev/null 2>&1; then
    echo "FAIL: $url unexpectedly served /health"; return 1
  fi
  echo "ok: $url has no /health"
}
```

Write a throwaway smoke that exercises the fixture (this is the failing test — it fails until the compose/nginx files exist):

Run:
```bash
cat > /tmp/e2e_smoke.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
source test/e2e/lib.sh
up_network
up_fixture smoke
TAG=v1 docker compose -f test/e2e/compose-proxy.yml up -d --no-deps web-blue
sleep 2
assert_version "http://localhost:${E2E_PORT}/version" v1
TAG=v1 docker compose -f test/e2e/compose-proxy.yml down
down_fixture smoke
down_network
EOF
bash /tmp/e2e_smoke.sh
```
Expected: FAIL — `test/e2e/compose-proxy.yml` and `test/e2e/fixture/nginx.conf` do not exist yet.

- [ ] **Step 2: Confirm the failure**

Run: `bash /tmp/e2e_smoke.sh; echo "exit=$?"`
Expected: non-zero; error names the missing compose or nginx.conf file.

- [ ] **Step 3: Write minimal implementation**

Create `test/e2e/fixture/nginx.conf`:

```nginx
events {}
http {
    # dockrail writes upstream fragments here; the seed file defines `web`.
    include /etc/nginx/dockrail/*.conf;
    server {
        listen 80;
        location /health  { proxy_pass http://web; }
        location /version { proxy_pass http://web; }
        location /        { proxy_pass http://web; }
    }
}
```

Create `test/e2e/fixture/compose-fixture.yml` (documents the fixture network; the nginx container itself is started imperatively by `up_fixture` so it can bind-mount the per-project upstream dir):

```yaml
# The shared network is created by lib.sh `up_network`; this file documents it
# and lets `docker compose -f compose-fixture.yml config` validate the name.
networks:
  default:
    name: dockrail-e2e-net
    external: true
```

Create `test/e2e/compose-recreate.yml`:

```yaml
networks:
  default:
    name: dockrail-e2e-net
    external: true
services:
  web:
    image: "${IMAGE:-dockrail-e2e-app}:${TAG:-v1}"
    ports:
      - "8080:8080"   # readiness.port published for `curl localhost:8080`
```

Create `test/e2e/compose-proxy.yml`:

```yaml
networks:
  default:
    name: dockrail-e2e-net
    external: true
services:
  web-blue:
    image: "${IMAGE:-dockrail-e2e-app}:${TAG:-v1}"
    ports:
      - "8080:8080"
  web-green:
    image: "${IMAGE:-dockrail-e2e-app}:${TAG:-v1}"
    ports:
      - "8080:8080"   # same host port as blue → the KNOWN RISK collision
```

- [ ] **Step 4: Run to verify it passes**

Run: `bash /tmp/e2e_smoke.sh && echo "SMOKE OK"`
Expected: `ok: http://localhost:18080/version == v1`, then `SMOKE OK`. (nginx proxies :18080 → upstream `web` → `web-blue:8080`.)

- [ ] **Step 5: Commit**

```bash
git add test/e2e/lib.sh test/e2e/fixture/ test/e2e/compose-recreate.yml test/e2e/compose-proxy.yml
git commit -m "test(e2e): shared harness lib, nginx fixture, compose files

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Recreate scenario + run.sh dispatcher

**Files:**
- Create: `test/e2e/run.sh`
- Create: `test/e2e/scenarios/recreate.sh`

**Interfaces:**
- Consumes: `lib.sh` (Task 2), `DOCKRAIL` env (path to a built binary), images (Task 1).
- Produces:
  - `test/e2e/run.sh <scenario...>` — sources `lib.sh`, ensures network + images, runs each named scenario in `test/e2e/scenarios/<name>.sh`, reports pass/fail, always cleans up.
  - `scenario_recreate()` — deploy v1 (recreate), assert `/version`=v1 on the published app port; deploy v2, assert exit 0 and `/version`=v2.

- [ ] **Step 1: Write the failing test**

Create `test/e2e/scenarios/recreate.sh`:

```bash
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
```

Create `test/e2e/run.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail
E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$E2E_DIR/lib.sh"

: "${DOCKRAIL:?set DOCKRAIL to the dockrail binary path}"

# Build images locally unless the caller pre-pushed them (droplet tier sets
# E2E_SKIP_BUILD=1 and IMAGE to a registry path).
if [ "${E2E_SKIP_BUILD:-0}" != "1" ]; then
  "$E2E_DIR/build-images.sh"
fi

up_network
trap 'down_network' EXIT

rc=0
for name in "$@"; do
  echo "=== scenario: $name ==="
  # shellcheck disable=SC1090
  source "$E2E_DIR/scenarios/$name.sh"
  if ! "scenario_$name"; then
    echo "FAIL scenario_$name"; rc=1
  fi
done
exit "$rc"
```

Run: `chmod +x test/e2e/run.sh && go build -o /tmp/dockrail . && DOCKRAIL=/tmp/dockrail test/e2e/run.sh recreate`
Expected: FAIL — the scenario file/function is discovered, but on a clean checkout there is no prior state; the FIRST run should actually pass once implemented. To see it fail first, temporarily rename the compose reference: run with a bogus binary `DOCKRAIL=/bin/false test/e2e/run.sh recreate` and expect `FAIL scenario_recreate`.

- [ ] **Step 2: Confirm the failure**

Run: `DOCKRAIL=/bin/false test/e2e/run.sh recreate; echo "exit=$?"`
Expected: `FAIL scenario_recreate`, `exit=1` (dockrail invocation fails).

- [ ] **Step 3: Write minimal implementation**

No new code — Steps 1's files are the implementation. If the scenario referenced helpers not yet in `lib.sh`, add them now. (All helpers used — `gen_deploy_yml`, `assert_version`, `E2E_DIR`, `APP_PORT` — exist from Task 2.)

- [ ] **Step 4: Run to verify it passes**

Run: `DOCKRAIL=/tmp/dockrail test/e2e/run.sh recreate`
Expected: `ok: …/version == v1`, `ok: …/version == v2`, `PASS scenario_recreate`, exit 0.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/run.sh test/e2e/scenarios/recreate.sh
git commit -m "test(e2e): run.sh dispatcher + recreate scenario

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Rollback scenario

**Files:**
- Create: `test/e2e/scenarios/rollback.sh`

**Interfaces:**
- Consumes: `lib.sh`, `run.sh` dispatcher, `DOCKRAIL`.
- Produces: `scenario_rollback()` — deploy v1 then v2 (recreate), run `dockrail rollback`, assert `/version` returns to v1.

- [ ] **Step 1: Write the failing test**

Create `test/e2e/scenarios/rollback.sh`:

```bash
#!/usr/bin/env bash
# scenario_rollback: after v2, `rollback` must restore v1.
scenario_rollback() {
  local ns="rollback-$$"
  local dy; dy="$(mktemp)"
  local app="http://localhost:${APP_PORT}"

  gen_deploy_yml "$dy" "$ns" "$E2E_DIR/compose-recreate.yml" recreate v1
  "$DOCKRAIL" -c "$dy" deploy
  gen_deploy_yml "$dy" "$ns" "$E2E_DIR/compose-recreate.yml" recreate v2
  "$DOCKRAIL" -c "$dy" deploy
  assert_version "$app/version" v2

  # rollback re-points to the previously running tag (v1).
  "$DOCKRAIL" -c "$dy" rollback
  assert_version "$app/version" v1

  TAG=v1 docker compose -f "$E2E_DIR/compose-recreate.yml" down >/dev/null 2>&1 || true
  rm -f "$dy"
  echo "PASS scenario_rollback"
}
```

Run: `DOCKRAIL=/bin/false test/e2e/run.sh rollback; echo "exit=$?"`
Expected: `FAIL scenario_rollback`, `exit=1`.

- [ ] **Step 2: Confirm the failure**

Run: same command; confirm `exit=1` and the FAIL line.

- [ ] **Step 3: Write minimal implementation**

The scenario file IS the implementation; it uses only existing helpers. No further code.

- [ ] **Step 4: Run to verify it passes**

Run: `DOCKRAIL=/tmp/dockrail test/e2e/run.sh rollback`
Expected: `…== v2`, `…== v1`, `PASS scenario_rollback`, exit 0.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/scenarios/rollback.sh
git commit -m "test(e2e): rollback scenario restores previous tag

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Failed-deploy forensics scenario

**Files:**
- Create: `test/e2e/scenarios/forensics.sh`

**Interfaces:**
- Consumes: `lib.sh`, `run.sh`, `DOCKRAIL`, the `:bad` image (Task 1).
- Produces: `scenario_forensics()` — with v1 live, deploy `:bad` (never ready); assert the command exits **non-zero**, v1 still serves `/version`=v1, and a failure record exists in the deploy history (`dockrail audit`).

- [ ] **Step 1: Write the failing test**

Create `test/e2e/scenarios/forensics.sh`:

```bash
#!/usr/bin/env bash
# scenario_forensics: a NEW that never becomes ready must not take down OLD,
# must exit non-zero, and must leave a failure trail.
scenario_forensics() {
  local ns="forensics-$$"
  local dy; dy="$(mktemp)"
  local app="http://localhost:${APP_PORT}"

  gen_deploy_yml "$dy" "$ns" "$E2E_DIR/compose-recreate.yml" recreate v1
  "$DOCKRAIL" -c "$dy" deploy
  assert_version "$app/version" v1

  gen_deploy_yml "$dy" "$ns" "$E2E_DIR/compose-recreate.yml" recreate bad
  if "$DOCKRAIL" -c "$dy" deploy; then
    echo "FAIL: bad deploy unexpectedly succeeded"; rm -f "$dy"; return 1
  fi
  echo "ok: bad deploy exited non-zero"

  # OLD must still serve. (recreate stops OLD before starting NEW, so this
  # asserts dockrail's documented recreate failure behavior; if v1 is down
  # here that is a real finding to record.)
  assert_version "$app/version" v1

  # A failure record must be visible in history.
  "$DOCKRAIL" -c "$dy" audit | grep -q "failed@" || {
    echo "FAIL: no failed@ record in audit"; rm -f "$dy"; return 1; }
  echo "ok: audit shows a failed@ record"

  TAG=v1 docker compose -f "$E2E_DIR/compose-recreate.yml" down >/dev/null 2>&1 || true
  rm -f "$dy"
  echo "PASS scenario_forensics"
}
```

Run: `DOCKRAIL=/bin/false test/e2e/run.sh forensics; echo "exit=$?"`
Expected: `FAIL scenario_forensics`, `exit=1`.

- [ ] **Step 2: Confirm the failure**

Run: same command; confirm the FAIL line and `exit=1`.

- [ ] **Step 3: Write minimal implementation**

Scenario file is the implementation. Note for the implementer: `recreate` stops OLD before starting NEW (`engine/engine.go:252-257`), so the "OLD still serves" assertion tests dockrail's real recreate-failure path. If it fails, record it as a finding rather than weakening the assertion.

- [ ] **Step 4: Run to verify it passes**

Run: `DOCKRAIL=/tmp/dockrail test/e2e/run.sh forensics`
Expected: `bad deploy exited non-zero`, `audit shows a failed@ record`, `PASS scenario_forensics`. If the "OLD still serves" line fails, capture the output and open a dockrail issue (recreate leaves a downtime window on failed NEW) — then proceed.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/scenarios/forensics.sh
git commit -m "test(e2e): failed-deploy forensics scenario

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Proxy zero-downtime scenario (with no-blip probe)

**Files:**
- Create: `test/e2e/scenarios/proxy.sh`

**Interfaces:**
- Consumes: `lib.sh`, `run.sh`, `DOCKRAIL`, nginx fixture, `compose-proxy.yml`.
- Produces: `scenario_proxy()` — bring up the nginx fixture; deploy v1 via proxy (one color, succeeds), confirm `/version`=v1 through nginx; start a background probe against **nginx** counting failures; deploy v2 via proxy; assert **zero** probe failures across the window (no blip). Then assert the outcome per the KNOWN RISK: if the v2 deploy failed at start-green (port collision), record the reproducer and assert v1 still serves; if it unexpectedly succeeded, assert `/version`=v2.

- [ ] **Step 1: Write the failing test**

Create `test/e2e/scenarios/proxy.sh`:

```bash
#!/usr/bin/env bash
# scenario_proxy: zero-downtime cutover through the nginx fixture.
# See KNOWN RISK in the plan/spec: the v2 (overlap) deploy is expected to fail
# at "start green" on a host-port collision, leaving v1 serving with no blip.
scenario_proxy() {
  local ns="proxy-$$"
  local dy; dy="$(mktemp)"
  local nginx="http://localhost:${E2E_PORT}"

  up_fixture "$ns"

  gen_deploy_yml "$dy" "$ns" "$E2E_DIR/compose-proxy.yml" proxy v1
  "$DOCKRAIL" -c "$dy" deploy       # first deploy: single color, must succeed
  assert_version "$nginx/version" v1

  # No-blip probe: hammer nginx every 100ms, count non-200s + connection resets.
  local fails=0 probe_flag; probe_flag="$(mktemp)"
  ( while [ -f "$probe_flag" ]; do
      curl -fsS -m 2 "$nginx/health" >/dev/null 2>&1 || echo x
      sleep 0.1
    done ) > "$probe_flag.hits" &
  local probe_pid=$!
  sleep 1   # warm the probe before cutover

  gen_deploy_yml "$dy" "$ns" "$E2E_DIR/compose-proxy.yml" proxy v2
  local v2_rc=0
  "$DOCKRAIL" -c "$dy" deploy || v2_rc=$?

  sleep 1
  rm -f "$probe_flag"; wait "$probe_pid" 2>/dev/null || true
  fails="$(grep -c x "$probe_flag.hits" 2>/dev/null || echo 0)"

  echo "no-blip: $fails failed requests across the cutover window"
  [ "$fails" -eq 0 ] || { echo "FAIL: blip detected ($fails failures)"; down_fixture "$ns"; rm -f "$dy" "$probe_flag.hits"; return 1; }

  if [ "$v2_rc" -eq 0 ]; then
    assert_version "$nginx/version" v2
    echo "note: proxy overlap SUCCEEDED — the KNOWN RISK did not reproduce"
  else
    assert_version "$nginx/version" v1
    echo "note: KNOWN RISK reproduced — v2 overlap deploy failed (rc=$v2_rc), v1 still serving; file a dockrail issue"
  fi

  TAG=v2 docker compose -f "$E2E_DIR/compose-proxy.yml" down >/dev/null 2>&1 || true
  down_fixture "$ns"
  rm -f "$dy" "$probe_flag.hits"
  echo "PASS scenario_proxy"
}
```

Run: `DOCKRAIL=/bin/false test/e2e/run.sh proxy; echo "exit=$?"`
Expected: `FAIL scenario_proxy`, `exit=1` (v1 deploy fails).

- [ ] **Step 2: Confirm the failure**

Run: same command; confirm the FAIL line and `exit=1`.

- [ ] **Step 3: Write minimal implementation**

Scenario file is the implementation. The pass criterion is **no blip** plus a correct branch on the deploy outcome — it passes whether or not the KNOWN RISK reproduces, but prints which happened. Do NOT edit dockrail to make v2 succeed; if the collision reproduces, open an issue referencing the design doc.

- [ ] **Step 4: Run to verify it passes**

Run: `DOCKRAIL=/tmp/dockrail test/e2e/run.sh proxy`
Expected: `no-blip: 0 failed requests …`, then either the SUCCEEDED note (v2 live) or the KNOWN RISK note (v1 still serving), then `PASS scenario_proxy`. Copy the `note:` line into the PR description.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/scenarios/proxy.sh
git commit -m "test(e2e): proxy zero-downtime scenario with no-blip probe

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Local-tier workflow (every PR)

**Files:**
- Create: `.github/workflows/e2e.yml`

**Interfaces:**
- Consumes: `run.sh` and all scenarios; runs on an `ubuntu-latest` runner (Docker preinstalled).
- Produces: an `e2e-local` job that builds dockrail, then runs all four scenarios via local exec. Triggers on `pull_request` and `push` to `main`. No secrets → works on fork PRs.

- [ ] **Step 1: Write the failing test**

Create `.github/workflows/e2e.yml`:

```yaml
name: e2e

on:
  pull_request:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  e2e-local:
    runs-on: ubuntu-latest
    env:
      GOTOOLCHAIN: local
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v6
        with:
          go-version: "1.26.4"
          check-latest: false
      - name: Build dockrail
        run: go build -o /tmp/dockrail .
      - name: Run e2e (local exec, all scenarios)
        env:
          DOCKRAIL: /tmp/dockrail
          CONN: local
        run: test/e2e/run.sh recreate rollback forensics proxy
```

Validate the workflow syntax locally:

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/e2e.yml')); print('yaml ok')"`
Expected: `yaml ok`. (Full failure surfaces only in CI; the local proxy of "does it work" is Step 4 running the same command the job runs.)

- [ ] **Step 2: Confirm the job command works locally**

Run: `go build -o /tmp/dockrail . && DOCKRAIL=/tmp/dockrail CONN=local test/e2e/run.sh recreate rollback forensics proxy; echo "exit=$?"`
Expected: all four print `PASS scenario_*`; `exit=0`. (This is the exact command the job runs.)

- [ ] **Step 3: Write minimal implementation**

If Step 2 surfaced a cross-scenario state leak (e.g. a leftover `web` container from a prior scenario), add a per-scenario `docker rm -f` guard at the top of the affected `scenario_*` (each scenario already tears down its own compose project; only add guards if Step 2 actually fails). Otherwise no change.

- [ ] **Step 4: Re-run to verify**

Run: `DOCKRAIL=/tmp/dockrail CONN=local test/e2e/run.sh recreate rollback forensics proxy`
Expected: four `PASS` lines, exit 0.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/e2e.yml
git commit -m "ci(e2e): local-exec e2e job on every PR

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Droplet lifecycle script + reaper workflow

**Files:**
- Create: `test/e2e/droplet.sh`
- Create: `.github/workflows/e2e-reap.yml`

**Interfaces:**
- Consumes: `doctl` (authenticated via `DIGITALOCEAN_ACCESS_TOKEN` env), an SSH pubkey.
- Produces:
  - `test/e2e/droplet.sh create` — create a Docker-preinstalled droplet named `dockrail-ci-<GITHUB_RUN_ID>`, tagged `dockrail-ci`, inject `$E2E_PUBKEY`, wait for SSH, print `IP=<addr>` to stdout.
  - `test/e2e/droplet.sh destroy` — destroy the droplet by name (idempotent).
  - `e2e-reap.yml` — hourly `schedule` job destroying any `dockrail-ci`-tagged droplet older than 30 minutes (the leak backstop).

- [ ] **Step 1: Write the failing test**

Create `test/e2e/droplet.sh`:

```bash
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
```

Run: `test/e2e/droplet.sh; echo "exit=$?"`
Expected: FAIL — usage error, `exit=2` (no subcommand). This confirms the dispatcher parses.

- [ ] **Step 2: Confirm the failure**

Run: `bash test/e2e/droplet.sh bogus; echo "exit=$?"`
Expected: `usage: droplet.sh create|destroy`, `exit=2`.

- [ ] **Step 3: Write minimal implementation**

The script above is the implementation. Create the reaper workflow `.github/workflows/e2e-reap.yml`:

```yaml
name: e2e-reap

on:
  schedule:
    - cron: "17 * * * *"   # hourly; destroys leaked dockrail-ci droplets
  workflow_dispatch:

permissions:
  contents: read

jobs:
  reap:
    runs-on: ubuntu-latest
    steps:
      - name: Install doctl
        uses: digitalocean/action-doctl@v2
        with:
          token: ${{ secrets.DIGITALOCEAN_ACCESS_TOKEN }}
      - name: Destroy dockrail-ci droplets older than 30m
        run: |
          set -euo pipefail
          now=$(date -u +%s)
          doctl compute droplet list --tag-name dockrail-ci \
            --format ID,Name,Created --no-header | while read -r id name created; do
            age=$(( now - $(date -u -d "$created" +%s) ))
            if [ "$age" -gt 1800 ]; then
              echo "reaping $name (age ${age}s)"
              doctl compute droplet delete "$id" --force
            fi
          done
```

- [ ] **Step 4: Verify syntax + dispatcher**

Run:
```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/e2e-reap.yml')); print('yaml ok')"
bash test/e2e/droplet.sh 2>&1 | grep -q usage && echo "dispatcher ok"
```
Expected: `yaml ok`, `dispatcher ok`. (Live `create`/`destroy` are exercised by Task 9 in CI, where the DO token exists.)

- [ ] **Step 5: Commit**

```bash
git add test/e2e/droplet.sh .github/workflows/e2e-reap.yml
git commit -m "ci(e2e): droplet lifecycle script + leak reaper

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Droplet-tier job (manual dispatch, real SSH + ghcr)

**Files:**
- Modify: `.github/workflows/e2e.yml` (add `workflow_dispatch` trigger + an `e2e-droplet` job)

**Interfaces:**
- Consumes: `droplet.sh` (Task 8), `run.sh` with `CONN=ssh`, `build-images.sh` with a ghcr repo arg, repo secret `DIGITALOCEAN_ACCESS_TOKEN`, built-in `GITHUB_TOKEN` for ghcr.
- Produces: an `e2e-droplet` job that builds+pushes v1/v2/bad to `ghcr.io/<owner>/dockrail-e2e-app`, creates a droplet, runs all four scenarios over SSH, and destroys the droplet in an `always()` step.

- [ ] **Step 1: Write the failing test**

Edit `.github/workflows/e2e.yml`: add `workflow_dispatch:` under `on:`, and append the job below. Add a top-level `concurrency` block so two dispatches can't race a droplet name.

```yaml
concurrency:
  group: e2e-droplet
  cancel-in-progress: false
```

```yaml
  e2e-droplet:
    if: github.event_name == 'workflow_dispatch'
    runs-on: ubuntu-latest
    timeout-minutes: 25          # hard cap so a hang cannot bill forever
    permissions:
      contents: read
      packages: write            # push images to ghcr
    env:
      GOTOOLCHAIN: local
      IMAGE: ghcr.io/${{ github.repository_owner }}/dockrail-e2e-app
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v6
        with: { go-version: "1.26.4", check-latest: false }
      - name: Build dockrail
        run: go build -o /tmp/dockrail .

      - name: Log in to ghcr
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Build + push app images
        run: |
          test/e2e/build-images.sh "$IMAGE"
          for t in v1 v2 bad; do docker push "$IMAGE:$t"; done

      - name: Install doctl
        uses: digitalocean/action-doctl@v2
        with:
          token: ${{ secrets.DIGITALOCEAN_ACCESS_TOKEN }}
      - name: Generate ephemeral SSH key
        run: |
          ssh-keygen -t ed25519 -N '' -f /tmp/e2e_key
          echo "E2E_PUBKEY=$(cat /tmp/e2e_key.pub)" >> "$GITHUB_ENV"
      - name: Create droplet
        id: droplet
        env:
          DIGITALOCEAN_ACCESS_TOKEN: ${{ secrets.DIGITALOCEAN_ACCESS_TOKEN }}
        run: test/e2e/droplet.sh create | tee -a "$GITHUB_OUTPUT"

      - name: Run e2e over SSH
        env:
          DOCKRAIL: /tmp/dockrail
          CONN: ssh
          SSH_TARGET: root@${{ steps.droplet.outputs.IP }}
          E2E_SKIP_BUILD: "1"       # images already pushed; droplet pulls them
        run: |
          eval "$(ssh-agent -s)"; ssh-add /tmp/e2e_key
          # ghcr pull auth on the droplet:
          ssh -o StrictHostKeyChecking=accept-new root@${{ steps.droplet.outputs.IP }} \
            "echo ${{ secrets.GITHUB_TOKEN }} | docker login ghcr.io -u ${{ github.actor }} --password-stdin"
          test/e2e/run.sh recreate rollback forensics proxy

      - name: Destroy droplet
        if: always()
        env:
          DIGITALOCEAN_ACCESS_TOKEN: ${{ secrets.DIGITALOCEAN_ACCESS_TOKEN }}
        run: test/e2e/droplet.sh destroy
```

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/e2e.yml')); print('yaml ok')"`
Expected: FAIL first if indentation is off; iterate until `yaml ok`.

- [ ] **Step 2: Confirm the failure surfaced (if any) then pass syntax**

Run: same yaml load command.
Expected: `yaml ok`, and `grep -c 'e2e-droplet' .github/workflows/e2e.yml` ≥ 1.

- [ ] **Step 3: Write minimal implementation**

The YAML above is the implementation. Ensure the local job still parses and the `on:` block now has both `pull_request`, `push`, and `workflow_dispatch`.

- [ ] **Step 4: Verify the local tier still runs**

Run: `DOCKRAIL=/tmp/dockrail CONN=local test/e2e/run.sh recreate rollback forensics proxy`
Expected: four `PASS` lines, exit 0 (adding the droplet job must not disturb local-tier behavior).

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/e2e.yml
git commit -m "ci(e2e): droplet tier over real SSH on manual dispatch

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 6: Live droplet-tier smoke (manual, after merge)**

After the branch is merged and `DIGITALOCEAN_ACCESS_TOKEN` is set as a repo secret, trigger the workflow from the Actions tab (`Run workflow`). Watch that the droplet is created, all four scenarios run over SSH, the `note:` line from the proxy scenario is captured, and the droplet is destroyed. Confirm via `doctl compute droplet list --tag-name dockrail-ci` that none remain.

---

## Self-Review

**Spec coverage:**
- Two tiers over one shared harness → Tasks 3–9 (`run.sh` param by `CONN`); local job Task 7, droplet job Task 9. ✓
- Test app + images (`/health`, `/version`, `:bad`) → Task 1. ✓
- Proxy fixture (nginx include dir, shared network, `docker exec … reload`) → Task 2. ✓
- deploy.yml/compose fixtures (recreate + proxy) → Task 2. ✓
- Scenario harness, own project namespace per scenario → Tasks 3–6 (each `ns=<name>-$$`). ✓
- Four scenarios: recreate (T3), rollback (T4), forensics (T5), proxy zero-downtime + no-blip (T6). ✓
- Droplet lifecycle + wait-for-SSH + reaper backstop + concurrency + timeout → Tasks 8–9. ✓
- ghcr build+push + registry-auth pull path on droplet → Task 9. ✓
- Fork PRs / no secrets: local tier has none; droplet tier is `workflow_dispatch`-gated → Tasks 7, 9. ✓
- KNOWN RISK (readiness can't probe NEW during overlap) surfaced, not papered over → Global Constraints + Task 6. ✓

**Placeholder scan:** No "TBD"/"add error handling"/"similar to Task N". Every scenario, compose, nginx, and workflow file is given in full. The one deliberate branch (proxy success vs KNOWN-RISK reproduction) is fully coded in Task 6, not left open.

**Type/interface consistency:** `E2E_NET`/`E2E_NGINX`/`E2E_PORT`/`APP_PORT`/`E2E_DIR`, `runc`, `up_network`/`down_network`, `up_fixture`/`down_fixture`, `gen_deploy_yml`, `assert_version`/`assert_no_health` defined once in Task 2 `lib.sh` and used unchanged in Tasks 3–6. `run.sh` reads `DOCKRAIL`, `CONN`, `E2E_SKIP_BUILD`, `IMAGE` — all set by the jobs in Tasks 7/9. `droplet.sh create|destroy` and its `IP=` output consumed by Task 9's `steps.droplet.outputs.IP`. Compose service names (`web`, `web-blue`, `web-green`) match the strategy each `gen_deploy_yml` call selects.
