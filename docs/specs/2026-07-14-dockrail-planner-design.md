# dockrail v2 — Planner (sub-spec 3 of the fleet reconciler)

**Date:** 2026-07-14
**Status:** Draft for review
**Scope:** Sub-spec 3 of the v2 fleet design
([`2026-07-14-dockrail-v2-fleet-design.md`](2026-07-14-dockrail-v2-fleet-design.md),
section 8). Builds on sub-spec 1 (Observer + fleet config) and sub-spec 2
(Scheduler), both merged. The Planner is the reconciler spine: it diffs desired
vs actual and emits an ordered action list. Read-only — it produces a plan;
execution is sub-spec 4 (`apply`).

---

## 1. Purpose

Given the desired `fleet.Config` and the observed `FleetState`, compute the
ordered set of actions that converges actual → desired while preserving the
fleet-wide invariant **OLD serves until NEW is proven**. The output is a printed
plan (`dockrail fleet plan`) now, and the input to `apply` later. Pure function,
deterministic, table-testable.

## 2. Identity — labels, not names (best practice)

Reconcilers identify managed objects by self-describing metadata read off
reality, not by parsing names (Kubernetes uses labels+ownerRefs, Kamal uses
container labels, Compose sets `com.docker.compose.*`). dockrail v2 does the
same. Every dockrail-managed container carries labels; the Planner diffs on
them.

**Label schema** (string consts in `fleet/observe`, shared by Observer which
reads them, Planner which interprets them, and `apply`/launch which stamps them):

| Label | On | Value |
|-------|-----|-------|
| `dockrail.managed` | every managed container | `"true"` |
| `dockrail.backend` | backend replicas | backend name |
| `dockrail.replica` | backend replicas | 0-based replica index |
| `dockrail.gpu` | backend replicas | GPU index the replica occupies |
| `dockrail.service` | routed-service containers | service name |

The image **tag** is read from the observed `Container.Image` (no label needed).
The **host** is known from which host the container was observed on.

## 3. Observer extension (folded into this sub-spec)

Sub-spec 1's Observer reads only `name` + `image`. Extend it to surface labels:

- `observe.Container` gains `Labels map[string]string`.
- `psQuery` template extracts exactly the dockrail keys, so there is no fragile
  all-labels comma parsing:
  `docker ps --format '{{.Names}}\t{{.Image}}\t{{.Label "dockrail.managed"}}\t{{.Label "dockrail.backend"}}\t{{.Label "dockrail.replica"}}\t{{.Label "dockrail.gpu"}}\t{{.Label "dockrail.service"}}'`
  (still single-quoted per the sub-spec-1 shell-escaping fix). Absent labels
  render empty. `parseContainers` fills `Labels` from the extra columns (only
  non-empty dockrail keys).
- `fleet status` still lists **all** running containers (labels just add
  optional columns); the Planner filters to `Labels["dockrail.managed"] == "true"`.

## 4. Reconciliation model

For each backend, the Scheduler assigns desired replicas host:gpu slots; the
Planner matches observed managed replicas (by `dockrail.backend` +
`dockrail.replica`) and classifies each desired/observed pair:

- **Satisfied** — desired replica has a container with the right tag on the right
  GPU → no action.
- **Update** — right replica present, wrong image tag → `UpdateReplica` (in
  place, same GPU).
- **Missing** — desired replica with no matching container → `PlaceReplica` on
  currently-free capacity.
- **Extra** — an observed managed replica beyond the desired count, or whose
  backend is no longer desired → `RemoveReplica`.

**Leave healthy replicas put (decided).** A running replica with the correct tag
is never moved for better packing. Moves happen only from *desired-state*
changes: a changed pin or a scale event is expressed as `RemoveReplica` (old) +
`PlaceReplica` (new), ordered across phases — not as the Scheduler
second-guessing a healthy placement. Live rebalancing is out of v1 scope (as in
k8s, a separate concern).

**Placing missing replicas — `schedule.PlanDelta`.** Because healthy replicas
stay put, the Planner must place only the *missing* auto replicas, while (a) not
re-placing kept replicas and (b) respecting anti-affinity against them. This is
a small, bounded extension to sub-spec 2's Scheduler:

```go
// PlanDelta places only the replicas not already covered by `kept`, seeding the
// ledger and anti-affinity from kept placements. Plan(cfg, state) == PlanDelta(cfg, state, nil).
func PlanDelta(cfg *fleet.Config, state observe.FleetState, kept Placements) (Placements, error)
```

`kept` is the set of satisfied/update replicas (they keep their GPUs); their
VRAM is already reflected in `observe.GPUState.FreeMiB` (they are running), and
`PlanDelta` marks their GPUs occupied-by-backend for anti-affinity. Missing
**pinned** replicas go to their pin (validated as today). The existing `Plan`
delegates to `PlanDelta(..., nil)` unchanged.

**Capacity / the NEW-alongside-OLD invariant.** Missing and moved replicas are
placed against **observed-free VRAM**, which already excludes every running
replica — so a new replica lands on genuinely free capacity, never on the GPU
its predecessor still occupies. The invariant falls out of using observed-free
as the ledger. (The single-GPU degrade case — no free GPU for the new replica —
surfaces as a `ScheduleError`; the `stop-old-first` accept-downtime path stays a
sub-spec-4 execution policy, not a Planner concern.)

## 5. Services & rewire

- Each `services` entry is matched by its `dockrail.service` container. Missing
  or wrong-tag → `DeployService` / `UpdateService` (single-host, mirrors v1).
- For each `uses` binding, the Planner emits `Rewire{service, backend,
  endpoints}` carrying the desired backend endpoints derived from the backend's
  placements (host:port per replica; port from the backend/service config). In
  this sub-spec `Rewire` is **emitted and printed only** — the Wiring driver
  that executes it is sub-spec 5.

## 6. Output — phases

`Plan{ Phases []Phase }`, `Phase{ Name string; Actions []Action }`. Exactly
three phases encode the serving invariant:

1. **converge** — `PlaceReplica` / `UpdateReplica` + `DeployService` /
   `UpdateService`. New capacity is created and proven. (A move driven by a pin
   change contributes its *new* replica here as a `PlaceReplica`.)
2. **rewire** — `Rewire` actions flip traffic to the new endpoints.
3. **drain** — `RemoveReplica` — old capacity released last, so serving capacity
   never dips below desired mid-transition. (A move contributes its *old*
   replica here as a `RemoveReplica`.)

A move is thus not a distinct action: it decomposes into `PlaceReplica`
(converge) + `RemoveReplica` (drain), consistent with "leave healthy replicas
put" — the only moves are desired-state-change-driven.

`Action` is a tagged struct (`Kind` enum + fields: `Backend`, `Replica`,
`Service`, `Host`, `GPU`, `Tag`, `OldTag`, `Endpoints`) with a `String()` for
human-readable plan output. A tagged struct (not an interface) keeps the plan
trivially printable and table-testable; `apply` (sub-spec 4) dispatches on
`Kind`.

`ActionKind` values: `place-replica`, `update-replica`, `remove-replica`,
`deploy-service`, `update-service`, `rewire`.

Deterministic ordering within a phase: backends by name, replicas ascending,
services by name.

## 7. `dockrail fleet plan` command

Loads `fleet.yml`, observes the fleet (Observer), runs `plan.Compute`, prints the
phased plan (text, plus `--json` emitting the `Plan` struct). Read-only — the
fleet-scale `--dry-run`. Shares the `fleet` command group and `-f/--fleet` flag
from sub-spec 1. No `apply` yet.

Example text output:

```
Phase 1 — converge
  place  llama-70b/2   gpu-b:0   vllm-v0.9.2
  update chat-api      gpu-a     ${TAG} (was v1.4.0)
Phase 2 — rewire
  rewire chat-api → llama-70b   [gpu-a:0, gpu-a:1, gpu-b:0]
Phase 3 — drain
  remove embed-small/1  gpu-a:3
```

An empty plan (fully converged) prints "already converged; no actions."

## 8. Interfaces

New package `fleet/plan`:

```go
func Compute(cfg *fleet.Config, observed observe.FleetState) (Plan, error)

type Plan struct { Phases []Phase }
type Phase struct { Name string; Actions []Action }
type Action struct {
	Kind      ActionKind
	Backend   string
	Replica   int
	Service   string
	Host      string
	GPU       int
	Tag       string
	OldTag    string
	Endpoints []string
}
func (a Action) String() string
type ActionKind string
```

Consumes: `fleet.Config`, `observe.FleetState`/`Container` (+ new `Labels`),
`schedule.PlanDelta`/`Placements`/`Assignment`. Produces the above for `apply`
(sub-spec 4) and the `plan` command.

## 9. Testing

- **Observer labels** — `parseContainers` fills `Labels` from the template
  columns; managed filter works; `fleet status` still lists unlabeled
  containers.
- **`schedule.PlanDelta`** — kept replicas not re-placed; anti-affinity honoured
  against kept; missing placed on free capacity; `Plan == PlanDelta(nil)`
  regression (existing scheduler tests stay green).
- **`plan.Compute`** pure table tests — no-op (all satisfied) → empty plan;
  scale-up → place; tag change → update; pin change → remove+place across
  phases; scale-down → remove; Err-host skipped; service deploy/update; rewire
  endpoints correct; phase ordering (converge < rewire < drain); determinism.

## 10. Build order (for the implementation plan)

1. Label schema consts + Observer `Labels` extension (Observer test updates;
   `fleet status` unaffected).
2. `schedule.PlanDelta` (kept-aware placement); `Plan` delegates to it.
3. `fleet/plan` types + backend diff (satisfied/update/missing/extra) via the
   Scheduler.
4. Service + rewire actions + three-phase assembly + ordering + `String()`.
5. `dockrail fleet plan` command (text + `--json`).

## 11. Open items / deferred

- **Replica renumbering.** Replica indices are 0..N-1 by construction. On
  scale-down the highest indices are the "extra" ones removed (stable). If a
  middle replica's container dies, the Planner sees it as missing index-K and
  re-places it — correct.
- **`Err`-host with managed replicas.** An unreachable host's replicas cannot be
  observed; the Planner treats a desired replica there as missing and would try
  to place elsewhere. For v1, a host in `Err` is reported and its desired
  replicas are left unplanned with a warning rather than blindly recreated
  elsewhere (avoids double-running when the host is merely unreachable, not
  dead). This is the concrete resolution of the sub-spec-1 deferred
  "`Err` stage-context" item, scoped to what the Planner needs.
- **Endpoint port derivation** for `Rewire` — from a `port` on the backend (the
  vLLM serving port); confirm the config field in the plan (may add
  `backends.<n>.port` if not already expressible).
- Live rebalancing of healthy replicas — deferred (section 4).
