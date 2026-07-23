# Issue 13 — proxy zero-gap: network-native readiness probe

Resolves [#13](https://github.com/Goodsmileduck/dockrail/issues/13): the zero-gap
`proxy` cutover cannot complete on a single host because bringing up the standby
color collides on the host-published readiness port.

## Problem

`readiness.port` is conflated into two roles:

- the **host-published** port the probe curls — `curl http://localhost:<port>` on
  the target (`strategy/readiness/http.go`), and
- the **container** port in the nginx upstream —
  `server <svc>-<color>:<port>` (`engine/nginx.go` `flipUpstream`).

In the zero-gap path blue and green run simultaneously. Two containers cannot
publish the same host port, so `docker compose up -d web-green` fails at start —
before any probe runs — with:

```
Bind for 0.0.0.0:<port> failed: port is already allocated
```

The collision is therefore **at container start**, not in the probe. The probe is
only implicated because it *forced* a host publish (it curls `localhost`). nginx
already reaches the colors by **container name over the shared bridge**
(`server web-green:<port>`), so host-publishing was never needed for the proxy
path — it existed only to satisfy the probe's `localhost` assumption.

This is consistent with the locked decisions: D6 (bridge networking, no
`network_mode: host`) and D5 (drive the existing nginx, colors share its network).
Each container has its own IP, so both colors listen on the **same container
port** with no conflict — the containerized best practice (k8s keeps
`containerPort` identical across pods; Kamal keeps one target port and flips the
proxy). Requiring *different* container ports per color is the bare-metal /
host-network pattern this design deliberately rejects.

## Fix

Probe NEW over the docker network at its container IP, and stop host-publishing
the shared port on proxy-path color services.

### 1. Prober interface gains a target host

`strategy/readiness/readiness.go`:

```go
Probe(ctx context.Context, conn connection.Connection, host string) error
```

`host` is the address to reach the service (IP or hostname, **no port** — each
prober owns its own port and path). The three probers replace their hardcoded
`localhost`:

| Prober | Before | After |
|--------|--------|-------|
| http   | `curl -fsS -m 5 http://localhost:%d%s` | `curl -fsS -m 5 http://%s:%d%s` (host, port, path) |
| vllm   | `http://localhost:%d/health`, `/v1/models` | `http://%s:%d/...` |
| tcp    | `</dev/tcp/localhost/%d>` | `</dev/tcp/%s/%d>` |

Callers:

- **recreate** (`engine/engine.go` ~L260) passes `"localhost"` — behavior is
  byte-for-byte identical to today (single active container, host-published).
- **proxy** (`engine/bluegreen.go` L106 and L127) passes the **container IP** of
  the freshly started NEW color.

### 2. Engine resolves NEW's container IP

New helper in `engine/bluegreen.go`:

```go
// containerIP returns the first IPv4 across the container's networks. Single-
// network is the norm for proxy-path color services; multi-network resolves to
// the first address (documented limitation).
func (e *Engine) containerIP(ctx context.Context, cid string) (string, error) {
    out, err := e.Conn.Run(ctx,
        "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "+cid)
    // trim, take first field; error if empty
}
```

In `proxyCutover`, immediately after `up -d --no-deps <green>` and before the
probe, in **both** the zero-gap and stop-old-first branches:

```go
cid, err := e.composePS(ctx, greenSvc)      // hard error — not best-effort slotID
if err != nil || cid == "" {
    return "", fmt.Errorf("resolve %s container: %w", greenSvc, err)
}
ip, err := e.containerIP(ctx, cid)
if err != nil { return "", err }
if err := prober.Probe(ctx, e.Conn, ip); err != nil { /* existing teardown */ }
```

`flipUpstream` is **unchanged** — still `server <svc>-<color>:<Readiness.Port>` by
name at the container port. So the probe hits `<ip>:port` and nginx hits
`<name>:port` — one container port end to end.

Constraint (documented): host-to-container-IP routing works on a standard Linux
docker host with a user-defined bridge (the single Linux target per D1/D2). A
custom `DOCKER-USER` iptables policy that drops host→bridge traffic would break
it — an edge case outside v1.

### 3. Contract: proxy-path color services must not host-publish the shared port

This is what actually removes the collision; the probe change is its consequence.
dockrail does not yet own compose generation (the D12 `<svc>-next` generator is
unimplemented — `bluegreen.go` expects the compose file to define
`<svc>-blue`/`<svc>-green`), so this is a **documented contract** enforced by a
preflight guard (section 4).

Documentation updates:

- Spec `docs/specs/2026-07-05-dockrail-design.md` — note that proxy-path color
  services reach nginx by container name and must not publish the shared host
  port.
- `docs/specs/2026-07-23-ci-e2e-deploy-test-design.md` — the "KNOWN RISK"
  port-collision note is resolved by this change; update it.

### 4. Preflight guard

Extend `engine/Preflight` with a check that fails fast with an actionable message
instead of the cryptic bind error. In `engine/preflight.go`:

```go
// For each service with cutover.strategy == "proxy", run `docker compose config`
// and collect the host ports published by <svc>-blue and <svc>-green. Any host
// port published by BOTH colors is a guaranteed start-time collision.
func proxyPortCollision(ctx, conn, cfg) []error
```

- Runs `TAG=<tag> docker compose -f <compose> config --format json` on the target
  via `conn` (a placeholder TAG is fine — published ports do not depend on the
  image tag).
- Parses `.services["<svc>-<color>"].ports[].published` for both colors.
- If the intersection of blue's and green's published host ports is non-empty,
  errors:

  ```
  <svc>-blue and <svc>-green both publish host port <p>; proxy cutover reaches
  colors by container name, not a shared host port. Remove the `ports:` mapping
  for host port <p> from the color services.
  ```

Called from `Preflight` alongside the existing command checks; its `[]error`
entries append to the returned slice.

### 5. e2e fixture + scenario

- `test/e2e/compose-proxy.yml` — remove the `ports:` mapping from `web-blue` and
  `web-green` (nginx reaches them by name on `dockrail-e2e-net`). Drops the
  deliberate collision.
- `test/e2e/scenarios/proxy.sh` — the v2 overlap deploy must now **succeed**:
  assert `v2_rc == 0`, `version == v2`, and zero blip. Remove the "KNOWN RISK
  reproduced" tolerance branch and update the header comment.

## Testing

- **Readiness unit tests** (`strategy/readiness/*_test.go`): update to the new
  `Probe(ctx, conn, host)` signature; assert the emitted command carries the
  passed host (e.g. probing `http://10.0.0.5:8080/health`, not `localhost`).
- **Engine bluegreen test** (`engine/bluegreen_test.go`, fake Connection that
  records issued commands): the fake gains a canned `docker inspect ... Networks`
  response returning an IP; assert that inspect is issued for the green container
  and that the probe command targets that IP. Assert recreate still probes
  `localhost`.
- **Preflight test** (`engine/preflight_test.go`): fake `docker compose config`
  JSON with both colors publishing `8080` → expect a collision error; with the
  `ports` removed → no error.

## Out of scope

- The D12 `<svc>-next` generated compose override (dockrail owning two-slot
  generation and thus the ports directly) — tracked separately; this fix works
  with the current "compose defines both colors" model.
- Multi-network color-service disambiguation beyond first-IPv4.
