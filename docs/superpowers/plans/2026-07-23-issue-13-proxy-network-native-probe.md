# Issue 13 — Proxy Network-Native Readiness Probe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the zero-gap `proxy` cutover complete on a single host by probing NEW over the docker network at its container IP instead of a host-published port, and stop proxy-path color services from publishing the shared host port.

**Architecture:** The readiness `Prober` interface gains a `host` target. `recreate` keeps passing `"localhost"` (unchanged); `proxy` resolves the freshly-started color's container IP via `docker inspect` and probes `<ip>:<port>`. nginx still routes to the color by container name at the same container port. A preflight guard catches a shared published color port early with an actionable error. The e2e fixture drops its deliberate port collision and now asserts the overlap deploy succeeds.

**Tech Stack:** Go (stdlib only), bash e2e harness, Docker Compose v2.

## Global Constraints

- Go standard style — run `gofmt -w` and `go vet ./...`; every commit must `go build ./...` and `go test ./...` clean.
- No new third-party dependencies (stdlib `encoding/json` only for the preflight parse).
- Keep the tool generic — no dogfood-project specifics in code.
- **Commit policy:** project rule is *never commit/push without explicit approval*. The commit step in each task assumes the operator has approved committing for this session — confirm before the first commit.
- Container port stays identical across colors (`readiness.port`); do not introduce per-color ports.

---

### Task 1: Prober interface accepts a target host

**Files:**
- Modify: `strategy/readiness/readiness.go` (interface)
- Modify: `strategy/readiness/http.go:30`, `strategy/readiness/tcp.go:32`, `strategy/readiness/vllm.go:39`
- Modify: `engine/engine.go:260` (recreate caller), `engine/bluegreen.go:106,127` (proxy callers — temporary `"localhost"`, replaced in Task 2), `fleet/apply/exec.go:151` (fleet multi-host probe caller — `"localhost"`, behavior-preserving)
- Test: `strategy/readiness/http_test.go`, `strategy/readiness/tcp_test.go`, `strategy/readiness/vllm_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `Prober.Probe(ctx context.Context, conn connection.Connection, host string) error` — `host` is an address with no port; each prober composes `host` + its own port/path. Task 2 relies on this signature.

- [ ] **Step 1: Update the http probe test to thread a non-localhost host**

In `strategy/readiness/http_test.go`, replace `TestHTTPProbeSuccess` and fix the timeout test's call site:

```go
func TestHTTPProbeSuccess(t *testing.T) {
	f := connection.NewFake()
	h := &HTTP{Path: "/health", Port: 8010, Timeout: 5 * time.Second}
	if err := h.Probe(context.Background(), f, "10.0.0.5"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Commands[0], "curl") || !strings.Contains(f.Commands[0], "10.0.0.5:8010/health") {
		t.Errorf("unexpected probe command: %v", f.Commands)
	}
}
```

In `TestHTTPProbeTimesOut`, change the call to `h.Probe(context.Background(), f, "localhost")` (the `curl` stub is host-agnostic, so keep asserting the timeout).

- [ ] **Step 2: Update tcp + vllm tests the same way**

In `strategy/readiness/tcp_test.go`, `TestTCPProbeSucceeds`: call `p.Probe(context.Background(), f, "10.0.0.5")` and assert `strings.Contains(..., "/dev/tcp/10.0.0.5/8010")`. In `TestTCPProbeTimesOut`: call `p.Probe(context.Background(), f, "localhost")` (its stub is `/dev/tcp/localhost/8010`, so keep `"localhost"`).

In `strategy/readiness/vllm_test.go`, all three tests: add `, "10.0.0.5"` to each `p.Probe(context.Background(), f)` call. The stubs match on `/health` and `/v1/models` substrings (host-agnostic), so no stub changes; the existing `:8000/health` / `:8000/v1/models` assertions still hold.

- [ ] **Step 3: Run the readiness tests to verify they fail to compile**

Run: `go test ./strategy/readiness/ 2>&1 | head -20`
Expected: FAIL — compile error, `Probe` called with 3 args but defined with 2.

- [ ] **Step 4: Change the interface and all three probers**

`strategy/readiness/readiness.go` — interface:

```go
type Prober interface {
	Probe(ctx context.Context, conn connection.Connection, host string) error
}
```

`strategy/readiness/http.go` — signature and command:

```go
func (h *HTTP) Probe(ctx context.Context, conn connection.Connection, host string) error {
	cmd := fmt.Sprintf("curl -fsS -m 5 http://%s:%d%s >/dev/null", host, h.Port, h.Path)
```

`strategy/readiness/tcp.go`:

```go
func (p *TCP) Probe(ctx context.Context, conn connection.Connection, host string) error {
	cmd := fmt.Sprintf("timeout 5 bash -c '</dev/tcp/%s/%d' 2>/dev/null", host, p.Port)
```

`strategy/readiness/vllm.go`:

```go
func (p *VLLM) Probe(ctx context.Context, conn connection.Connection, host string) error {
	health := fmt.Sprintf("curl -fsS -m 5 http://%s:%d/health >/dev/null", host, p.Port)
	models := fmt.Sprintf("curl -fsS -m 5 http://%s:%d/v1/models", host, p.Port)
```

- [ ] **Step 5: Update the three callers so the module still compiles**

`engine/engine.go:260` (recreate — correct final value):

```go
	if err := prober.Probe(ctx, e.Conn, "localhost"); err != nil {
```

`engine/bluegreen.go` at both probe sites (lines ~106 and ~127) — temporary, replaced in Task 2:

```go
	if err := prober.Probe(ctx, e.Conn, "localhost"); err != nil {
```

`fleet/apply/exec.go:151` (multi-host fleet probe — behavior-preserving, not part of the issue #13 proxy path):

```go
	if err := prober.Probe(ctx, x.conn, "localhost"); err != nil {
```

- [ ] **Step 6: Run all tests and vet**

Run: `gofmt -w strategy/readiness/*.go engine/*.go && go vet ./... && go test ./...`
Expected: PASS. (Engine bluegreen tests still pass — they assert command *order*, not the probe host; the fake returns success for `curl localhost...`.)

- [ ] **Step 7: Commit**

```bash
git add strategy/readiness/ engine/engine.go engine/bluegreen.go
git commit -m "refactor(readiness): Probe takes a target host, callers pass localhost"
```

---

### Task 2: Proxy cutover probes NEW at its container IP

**Files:**
- Modify: `engine/bluegreen.go` (add `strings` import, `containerIP` + `probeGreen` helpers, wire both probe sites)
- Test: `engine/bluegreen_test.go`, `engine/engine_test.go` (add green-slot + inspect stubs)

**Interfaces:**
- Consumes: `Prober.Probe(ctx, conn, host)` from Task 1; `Engine.composePS(ctx, name) (string, error)` (existing, `engine/engine.go:312`).
- Produces: `Engine.containerIP(ctx, cid string) (string, error)` — first IPv4 of a container; `Engine.probeGreen(ctx, prober readiness.Prober, greenSvc string) error` — resolve green's container, get its IP, probe it.

- [ ] **Step 1: Add green-slot stubs + IP assertion to the zero-gap test**

In `engine/bluegreen_test.go`, `TestProxyCutoverFreeSlotIsZeroGap`, after the existing stubs add:

```go
	f.Stub("ps -q web-green", "cid-green\n", nil)
	f.Stub("NetworkSettings.Networks", "10.1.2.3\n", nil)
```

and after the existing order assertion add:

```go
	if !strings.Contains(all, "10.1.2.3:8000/health") {
		t.Fatalf("green must be probed at its container IP:\n%s", all)
	}
```

- [ ] **Step 2: Add the same green-slot stubs to the other probing cutover tests**

Add these two stubs to `TestProxyCutoverStopOldFirst` and `TestProxyCutoverAutoRollback` (both reach the probe):

```go
	f.Stub("ps -q web-green", "cid-green\n", nil)
	f.Stub("NetworkSettings.Networks", "10.1.2.3\n", nil)
```

In `engine/engine_test.go`, `TestDeployProxyStrategyRoutesCutover`, add the same two stubs alongside the existing `f.Stub("ps -q web-blue", ...)` line. (`TestProxyCutoverFailBranch` aborts before start — leave it untouched.)

- [ ] **Step 3: Run engine tests to verify they fail**

Run: `go test ./engine/ -run 'ProxyCutover|ProxyStrategy' 2>&1 | head -30`
Expected: FAIL — `TestProxyCutoverFreeSlotIsZeroGap` fails its new `10.1.2.3:8000/health` assertion (probe still uses `localhost`).

- [ ] **Step 4: Add the helpers and wire the probe sites**

In `engine/bluegreen.go`, add `"strings"` to the import block, then add:

```go
// containerIP returns the first IPv4 across the container's networks. Single-
// network is the norm for proxy-path color services; multi-network resolves to
// the first address (documented limitation).
func (e *Engine) containerIP(ctx context.Context, cid string) (string, error) {
	out, err := e.Conn.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' %s", cid))
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", cid, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("no IP for container %s", cid)
	}
	return fields[0], nil
}

// probeGreen resolves the freshly-started color's container, looks up its IP on
// the docker network, and probes it there — the proxy path never host-publishes
// the color, so localhost would not reach it.
func (e *Engine) probeGreen(ctx context.Context, prober readiness.Prober, greenSvc string) error {
	cid, err := e.composePS(ctx, greenSvc)
	if err != nil {
		return fmt.Errorf("resolve %s container: %w", greenSvc, err)
	}
	if cid == "" {
		return fmt.Errorf("%s not running after start", greenSvc)
	}
	ip, err := e.containerIP(ctx, cid)
	if err != nil {
		return err
	}
	return prober.Probe(ctx, e.Conn, ip)
}
```

Then replace the temporary probe call at **both** sites in `proxyCutover` — the stop-old-first branch and the zero-gap branch — changing:

```go
	if err := prober.Probe(ctx, e.Conn, "localhost"); err != nil {
```

to:

```go
	if err := e.probeGreen(ctx, prober, greenSvc); err != nil {
```

(Leave each branch's surrounding rollback/teardown untouched — only the condition changes.)

- [ ] **Step 5: Run engine tests to verify they pass**

Run: `gofmt -w engine/bluegreen.go && go test ./engine/ 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 6: Full build + test + vet**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add engine/bluegreen.go engine/bluegreen_test.go engine/engine_test.go
git commit -m "fix(proxy): probe NEW color at its container IP over the docker network (#13)"
```

---

### Task 3: Preflight guard for a shared published color port

**Files:**
- Modify: `engine/preflight.go`
- Test: `engine/preflight_test.go` (create)

**Interfaces:**
- Consumes: `config.Config`, `connection.Connection`.
- Produces: `proxyPortCollision(ctx context.Context, conn connection.Connection, cfg *config.Config) []error`, appended into `Preflight`.

- [ ] **Step 1: Write the failing preflight tests**

Create `engine/preflight_test.go`:

```go
package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func proxyCfg() *config.Config {
	return &config.Config{
		Compose: "docker-compose.yml",
		Services: map[string]config.Service{
			"web": {Cutover: config.Cutover{Strategy: "proxy", Proxy: "nginx"}},
		},
	}
}

func TestPreflightFlagsSharedColorPort(t *testing.T) {
	f := connection.NewFake()
	f.Stub("config --format json",
		`{"services":{"web-blue":{"ports":[{"published":"8080","target":8080}]},`+
			`"web-green":{"ports":[{"published":"8080","target":8080}]}}}`, nil)
	errs := Preflight(context.Background(), f, proxyCfg())
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "8080") || !strings.Contains(joined, "web-green") {
		t.Fatalf("want a shared-port collision error mentioning 8080, got: %s", joined)
	}
}

func TestPreflightAllowsUnpublishedColors(t *testing.T) {
	f := connection.NewFake()
	f.Stub("config --format json",
		`{"services":{"web-blue":{"ports":[]},"web-green":{"ports":[]}}}`, nil)
	for _, e := range Preflight(context.Background(), f, proxyCfg()) {
		if strings.Contains(e.Error(), "host port") {
			t.Fatalf("unpublished colors must not trip the collision guard: %v", e)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./engine/ -run Preflight 2>&1 | head -20`
Expected: FAIL — `TestPreflightFlagsSharedColorPort` gets no collision error (guard not implemented).

- [ ] **Step 3: Implement the guard**

In `engine/preflight.go`, add `"encoding/json"` to imports, append the collision result inside `Preflight` just before `return errs`:

```go
	errs = append(errs, proxyPortCollision(ctx, conn, cfg)...)
```

and add:

```go
// proxyPortCollision fails fast when a proxy-cutover service's two color
// containers publish the same host port — a guaranteed start-time bind
// collision (issue #13). Best-effort: if compose config cannot be rendered or
// parsed, it stays silent rather than blocking the deploy.
func proxyPortCollision(ctx context.Context, conn connection.Connection, cfg *config.Config) []error {
	anyProxy := false
	for _, s := range cfg.Services {
		if s.Cutover.Strategy == "proxy" {
			anyProxy = true
			break
		}
	}
	if !anyProxy {
		return nil
	}
	out, err := conn.Run(ctx, fmt.Sprintf("docker compose -f %s config --format json", cfg.Compose))
	if err != nil {
		return nil
	}
	var parsed struct {
		Services map[string]struct {
			Ports []struct {
				Published string `json:"published"`
			} `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil
	}
	published := func(svc string) map[string]bool {
		set := map[string]bool{}
		for _, p := range parsed.Services[svc].Ports {
			if p.Published != "" {
				set[p.Published] = true
			}
		}
		return set
	}
	var errs []error
	for name, s := range cfg.Services {
		if s.Cutover.Strategy != "proxy" {
			continue
		}
		green := published(name + "-green")
		for port := range published(name + "-blue") {
			if green[port] {
				errs = append(errs, fmt.Errorf(
					"%s-blue and %s-green both publish host port %s; proxy cutover reaches colors by container name, not a shared host port — remove the `ports:` mapping for host port %s from the color services",
					name, name, port, port))
			}
		}
	}
	return errs
}
```

- [ ] **Step 4: Run to verify pass**

Run: `gofmt -w engine/preflight.go && go test ./engine/ -run Preflight 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 5: Full build + test + vet**

Run: `go vet ./... && go test ./...`
Expected: PASS. (The existing `TestDeployProxyStrategyRoutesCutover` leaves `config --format json` unstubbed → fake returns `""` → `json.Unmarshal` errors → guard stays silent, so that test is unaffected.)

- [ ] **Step 6: Commit**

```bash
git add engine/preflight.go engine/preflight_test.go
git commit -m "feat(preflight): reject a shared published color port for proxy cutover (#13)"
```

---

### Task 4: Drop the e2e collision, assert overlap succeeds, update docs

**Files:**
- Modify: `test/e2e/compose-proxy.yml` (remove `ports:` from both colors)
- Modify: `test/e2e/scenarios/proxy.sh` (require v2 overlap success)
- Modify: `docs/specs/2026-07-23-ci-e2e-deploy-test-design.md` (resolve the KNOWN RISK note)
- Modify: `docs/specs/2026-07-05-dockrail-design.md` (note the no-host-publish contract)

**Interfaces:**
- Consumes: the fixed proxy cutover from Tasks 1–3.
- Produces: nothing code-facing; an e2e scenario that now asserts a successful overlap.

- [ ] **Step 1: Remove the deliberate port collision from the fixture**

Replace `test/e2e/compose-proxy.yml` with:

```yaml
networks:
  default:
    name: dockrail-e2e-net
    external: true
services:
  # Proxy-path color services do NOT publish host ports: nginx reaches them by
  # container name over the shared bridge, and readiness probes them at their
  # container IP (issue #13). Both colors listen on the same container port.
  web-blue:
    image: "${IMAGE:-dockrail-e2e-app}:${TAG:-v1}"
  web-green:
    image: "${IMAGE:-dockrail-e2e-app}:${TAG:-v1}"
```

- [ ] **Step 2: Rewrite the proxy scenario to require overlap success**

In `test/e2e/scenarios/proxy.sh`, update the header comment and the v2 block. Replace the two-line comment at the top of the function's intent (the `# See KNOWN RISK ...` lines) with:

```bash
# scenario_proxy: zero-downtime cutover through the nginx fixture. The v2 deploy
# brings green up alongside blue, probes it over the docker network, flips nginx,
# then stops blue — no blip, and v2 must land (issue #13 resolved).
```

Replace the block from `gen_deploy_yml "$dy" "$ns" ... proxy v2` through the `if [ "$v2_rc" -eq 0 ]; then ... fi` with:

```bash
  gen_deploy_yml "$dy" "$ns" "$TARGET_DIR/compose-proxy.yml" proxy v2
  local v2_rc=0
  "$DOCKRAIL" -c "$dy" deploy || v2_rc=$?

  runc "touch $stopf"           # cutover attempt done → stop the probe
  wait "$probe_pid" 2>/dev/null || true
  runc "rm -f $stopf" || true
  local fails; fails="$(grep -c x "$hits" 2>/dev/null || true)"

  echo "no-blip: $fails failed requests across the cutover window"
  [ "$fails" -eq 0 ] || { echo "FAIL: blip detected ($fails failures)"; down_fixture "$ns"; rm -f "$dy" "$hits"; return 1; }

  if [ "$v2_rc" -ne 0 ]; then
    echo "FAIL: v2 proxy overlap deploy failed (rc=$v2_rc)"
    runc "TAG=v2 docker compose -f $TARGET_DIR/compose-proxy.yml down >/dev/null 2>&1" || true
    down_fixture "$ns"; rm -f "$dy" "$hits"; return 1
  fi
  assert_version "$nginx/version" v2
```

(The `runc "touch $stopf"` / `wait` / cleanup lines that previously lived *after* the `if` block are now folded into this block — delete the old duplicated copies so the probe is stopped exactly once.)

- [ ] **Step 3: Run the proxy scenario locally to verify it passes**

Run: `go build -o /tmp/dockrail . && DOCKRAIL=/tmp/dockrail CONN=local test/e2e/run.sh proxy 2>&1 | tail -30`
Expected: `no-blip: 0 failed requests ...`, `ok: http://localhost:18080/version == v2`, `PASS scenario_proxy`.

(Requires a local docker daemon. If the harness needs the app images, `test/e2e/build-images.sh dockrail-e2e-app` first, per `test/e2e/run.sh`.)

- [ ] **Step 4: Resolve the KNOWN RISK note in the e2e design doc**

In `docs/specs/2026-07-23-ci-e2e-deploy-test-design.md`, find the KNOWN RISK / port-collision passage for the proxy scenario and replace its "expected to fail" framing with a resolution note, e.g.:

```markdown
> **Resolved (issue #13):** proxy-path color services no longer publish a host
> port; nginx routes to them by container name and readiness probes them at
> their container IP, so the v2 overlap deploy completes. The scenario asserts
> v2 lands with zero blip.
```

- [ ] **Step 5: Record the contract in the main design doc**

In `docs/specs/2026-07-05-dockrail-design.md`, near the proxy/cutover (D5/D11/D12) discussion, add a sentence:

```markdown
Proxy-path color services must not host-publish the shared service port: nginx
reaches them by container name over the shared bridge and readiness probes them
at their container IP (issue #13). Both colors listen on the same container port.
```

- [ ] **Step 6: Commit**

```bash
git add test/e2e/compose-proxy.yml test/e2e/scenarios/proxy.sh docs/specs/2026-07-23-ci-e2e-deploy-test-design.md docs/specs/2026-07-05-dockrail-design.md
git commit -m "test(e2e): proxy overlap must land now that colors don't share a host port (#13)"
```

---

## Self-Review

**Spec coverage:**
- Prober `host` param + http/vllm/tcp rewrite → Task 1. ✓
- Engine container-IP resolution + both proxy probe sites → Task 2. ✓
- `flipUpstream` unchanged → not modified in any task. ✓
- recreate keeps `localhost` → Task 1 Step 5. ✓
- Documented contract → Task 4 Steps 4–5. ✓
- Preflight guard → Task 3. ✓
- e2e fixture + scenario → Task 4 Steps 1–3. ✓
- Tests (readiness, bluegreen, preflight) → Tasks 1–3. ✓

**Placeholder scan:** none — every code and command step is concrete.

**Type consistency:** `Probe(ctx, conn, host string)` used identically across readiness.go/http.go/tcp.go/vllm.go and both callers; `containerIP`/`probeGreen`/`proxyPortCollision` signatures match their call sites. Fake stub substrings (`ps -q web-green`, `NetworkSettings.Networks`, `config --format json`) match the exact commands the code issues.

**Note on `published` type:** parsed as a JSON string, matching `docker compose config --format json` (v2) long-form output (`"published": "8080"`). If a future compose emits it as a number, `json.Unmarshal` errors and the guard falls back to silent (safe) — the collision would then surface as the raw bind error, same as today.
