# dockrail v2 — apply (sub-spec 4 of the fleet reconciler)

**Date:** 2026-07-14
**Status:** Draft for review
**Scope:** Sub-spec 4 of the v2 fleet design
([`2026-07-14-dockrail-v2-fleet-design.md`](2026-07-14-dockrail-v2-fleet-design.md),
section 8). Builds on sub-specs 1–3 (Observer, Scheduler, Planner), all merged.
`apply` executes the Planner's phased plan — the step that turns `fleet plan`
(dry-run) into real, health-gated deploys across the fleet. The `Rewire`
actions are executed through a **Wiring interface** whose real drivers land in
sub-spec 5.

---

## 1. Purpose

`dockrail fleet apply` observes the fleet, computes the plan (`plan.Compute`),
and executes its three phases — **converge → rewire → drain** — preserving the
fleet-wide invariant **OLD serves until NEW is proven**. It is the first command
that mutates host state at fleet scale.

## 2. Launch model — compose, per-replica overrides (decided)

Backends and services are **compose services** (decision D7 preserved: the tool
never replaces compose). `apply` launches a backend replica by generating a
**per-replica compose override** layered over the user's compose file and
running `docker compose`. This is v1's two-slot override mechanism (D12,
`<svc>-blue/green`) generalized to `<backend>-<replica>` fanned across hosts,
with explicit per-replica GPU pinning (not `--scale`, rejected in v1 sect. 13).

**Config additions:**

- Top-level `compose: <file>` (the compose file, shipped to each host — as v1).
- Each **backend** and **service** gains `service: <compose-service>` naming the
  service to template from. `image_tag` still overrides that service's tag;
  `model`/`placement`/`readiness` are unchanged.

```yaml
compose: docker-compose.fleet.yml     # one file, shipped per host

backends:
  llama-70b:
    service: vllm                     # templates from the `vllm` compose service
    image_tag: vllm-v0.9.2
    model: /models/llama70b/best
    replicas: 3
    placement: { vram_min: "20GiB", gpu: auto, pool: [gpu-a, gpu-b] }
    readiness: { type: vllm, timeout: 300s }

services:
  chat-api:
    service: chat-api                 # compose service of the same name
    host: gpu-a
    image_tag: "${TAG}"
    uses: [ { backend: llama-70b, wiring: { strategy: nginx-upstream } } ]
    readiness: { type: http, path: /health, port: 8080 }
```

Multiple backends may share one compose service (e.g. all vLLM backends →
`service: vllm`), differentiated by `image_tag` + `model` + the per-replica GPU
override.

## 3. The per-replica override

For a `PlaceReplica{backend, replica, host, gpu, tag}`, `apply` writes an
override (on the host, alongside the base compose) that, for `<backend>`'s
compose service, sets:

- `container_name: <backend>-<replica>` (the identity the Planner matches);
- `deploy.resources.reservations.devices` / `device_ids: ['<gpu>']` (the GPU);
- `labels:` `dockrail.managed=true`, `dockrail.backend=<backend>`,
  `dockrail.replica=<replica>`, `dockrail.gpu=<gpu>` (the labels the Observer
  reads — this is the **label-stamping** the Planner depends on);
- the image tag (`TAG=<tag>` env, as v1) and the `model` mount.

Then `docker compose -f <base> -f <override> up -d --no-deps <service>` launches
just that replica. Services stamp `dockrail.service=<name>` similarly.

This override generation is the natural extension of `engine/bluegreen.go`'s
existing override writer; sub-spec 4 generalizes it from two colors to
`<backend>-<replica>` and adds the label block.

## 4. Executor — per-action, reusing the v1 engine

`apply` does not call `Engine.Deploy` (a whole single-service state machine)
directly; it reuses the engine's **lower-level helpers** — override generation,
readiness probing (`strategy/readiness`), the deploy lock, and history — driving
one action at a time. Each host gets a `connection.Connection`; actions on the
same host are issued through it.

| Action | Execution |
|--------|-----------|
| `PlaceReplica` | write override → `docker compose … up -d <service>` → probe `backend.readiness` (long vllm timeout) → on failure keep the failed container for forensics, abort the phase |
| `UpdateReplica` | recreate that one replica with the new tag (rolling: the backend's other replicas keep serving — a per-replica blip, not a fleet outage) |
| `RemoveReplica` | `docker compose … rm -sf <backend>-<replica>` (drain phase, last) |
| `DeployService` / `UpdateService` | the existing v1 engine deploy for that service (its own recreate/proxy cutover) |
| `Rewire` | `Wiring.Apply(service, backend, endpoints)` — interface here, drivers in sub-spec 5 |

**Health-gating:** the converge phase proves every `PlaceReplica`/`UpdateReplica`
ready (via the backend's readiness strategy) before the phase completes; the
rewire phase runs only after converge succeeds; drain runs last. This is the
fleet-scale expression of the v1 invariant.

## 5. Wiring interface (drivers = sub-spec 5)

```go
type Wiring interface {
	// Apply points `service` at `backend`'s current healthy endpoints.
	Apply(ctx, service string, backend string, endpoints []Endpoint) error
}
```

Sub-spec 4 defines the interface + a **no-op/logging default** (a `Rewire`
action logs "would wire service → [endpoints]" and succeeds), so `apply` is
end-to-end runnable for backends/services now; sub-spec 5 supplies the
`nginx-upstream` and `env-list` drivers and the `host:port` endpoint derivation
the Planner currently leaves as host-only.

## 6. Orchestration & failure policy

- **Phases in order:** converge → rewire → drain. A phase completes only when all
  its actions succeed (converge/rewire are gated; drain is best-effort but
  reported).
- **`--on-failure=hold` (default):** stop at the failed action. Completed actions
  stay (each was health-gated). The failed `PlaceReplica`'s NEW container is kept
  for forensics; OLD replicas keep serving. Exit non-zero, report
  converged/pending/failed. Re-running `apply` recomputes the diff and resumes —
  **idempotent by construction**.
- **`--on-failure=rollback`:** reverse the already-applied actions of this run in
  reverse phase order (remove placed replicas, restore updated ones, re-add
  drained ones where still possible). Honest caveat (as the v2 design states):
  reversing a real model load costs minutes and can itself fail; `hold` +
  idempotent re-run is the recommended path.

## 7. Locking, scoping, safety

- **Fleet lock:** acquire a fleet-scoped lock (v1 `engine/lock.go`, promoted from
  per-project to per-fleet) before executing; release after. `--lock-wait[=15m]`
  polls (for racing CI pipelines). A second concurrent `apply` fails fast with
  the holder's identity.
- **`--scope <backend|service>`:** observe the whole fleet (placement/wiring need
  it) but execute only the actions for the scoped entry — the observe-whole /
  act-scoped mechanism that enables the central-fleet-repo and per-repo GitOps
  flows (design sect. 6).
- **`Err`-host refusal:** `apply` will not execute an action targeting a host in
  `Err` (unreachable); it reports the Planner's warning and skips (or, if a
  scoped action requires that host, fails fast). This is the execution-side
  resolution of the `Err`-host safety the Planner flags read-only.
- **History:** each executed action appends to the per-host `history.jsonl`
  (reusing v1's store), so `status`/`audit` and rollback anchors work fleet-wide.

## 8. Interfaces

```go
// package apply
func Apply(ctx context.Context, cfg *fleet.Config, obs observe.Observer,
	factory ConnFactory, opts Options) (Result, error)

type Options struct {
	OnFailure string        // "hold" (default) | "rollback"
	Scope     string        // "" = whole fleet; else a backend/service name
	LockWait  time.Duration // 0 = fail fast
	DryRun    bool          // print plan only (delegates to fleet plan)
}
type Result struct {
	Applied []plan.Action
	Failed  *plan.Action
	Pending []plan.Action
	Warnings []string
}
```

Consumes: `fleet.Config` (+ `compose`/`service`), `plan.Compute`,
`strategy/readiness`, `engine` helpers (override gen, lock, history), the
`Wiring` interface.

## 9. `dockrail fleet apply` command

`fleet apply [--on-failure hold|rollback] [--scope X] [--lock-wait 15m]
[--dry-run]` — loads `fleet.yml`, acquires the fleet lock, observes, plans,
executes, reports the `Result` (text + `--json`). `--dry-run` prints the plan
(equivalent to `fleet plan`) without mutating.

## 10. Testing

- **Override generation** — table tests: `(backend, replica, host, gpu, tag)` →
  expected override YAML (container_name, device_ids, labels, tag). Pure.
- **Executor** — against a fake `Connection` that records issued commands: assert
  the compose-up / rm command sequence per action, the readiness probe order,
  and the failed-replica-kept-for-forensics behavior.
- **Orchestration** — a fake executor + a canned `Plan`: assert phase order
  (converge before rewire before drain), `--on-failure=hold` leaves converged
  actions and reports pending, `--on-failure=rollback` reverses in order, lock
  acquired/released, `--scope` filters, `Err`-host action refused.
- **Wiring no-op** — a `Rewire` action logs and succeeds.
- **Idempotence** — applying a converged fleet (empty plan) is a no-op.

## 11. Build order (for the implementation plan)

1. Config `compose` + backend/service `service` fields + validation; per-replica
   compose-override generation (with `dockrail.*` labels).
2. Per-action executors (`PlaceReplica`/`UpdateReplica`/`RemoveReplica`) via
   compose + readiness gating, against a fake Connection.
3. Service deploy/update via the v1 engine + the `Wiring` interface (no-op
   default).
4. Phase orchestration + `--on-failure=hold|rollback` + `Result`.
5. Fleet lock (promote v1 lock to fleet scope) + `--scope` + `Err`-host refusal +
   history append.
6. `dockrail fleet apply` command (text + `--json`, `--dry-run`).

## 12. Deferred / open items

- **Rewire endpoint `host:port`** and the real `nginx-upstream`/`env-list`
  drivers — sub-spec 5.
- **Pin-change moves** for already-running replicas — still deferred (Planner
  treats a present, right-tagged replica as satisfied regardless of GPU).
- **`stop-old-first` single-GPU degrade** — the v1 `on_no_free_gpu` policy
  applies per replica when a move has no free GPU; carried from v1, surfaced
  when a `PlaceReplica` can't find capacity.
- **Compose file shipping** — how the base compose file reaches each host
  (committed alongside `fleet.yml` in the central fleet repo, or rsync'd); confirm
  the mechanism (likely: assume present on host, as v1 assumes the compose file
  is on the host).
