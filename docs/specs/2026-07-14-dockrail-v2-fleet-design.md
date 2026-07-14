# dockrail v2 — Fleet Reconciler (Multi-host, GPU-sharded, Declarative)

**Date:** 2026-07-14
**Status:** Draft for review
**Scope:** v2 architecture direction. Extends the v1 single-host design
([`2026-07-05-dockrail-design.md`](2026-07-05-dockrail-design.md)) to a
multi-host, GPU-sharded, desired-state model. This is a **vision/architecture
spec**: it fixes the shape and decomposes the work into shippable sub-specs; it
is not itself a single implementation plan.

---

## 1. What v2 adds

v1 deploys **one service, one host, one tag** through an imperative,
health-gated state machine. v2 keeps that state machine as the execution
primitive and adds a **declarative fleet layer** on top of it, closing four
gaps:

| # | Requirement | v1 today | v2 closes it with |
|---|-------------|----------|-------------------|
| 1 | Multi-server / multi-GPU | Agentless SSH per host; GPU free-slot detection | `hosts:` block + Observer reads all hosts; planner fans actions across them |
| 2 | GPU sharding (N models / GPU) | GPU placement with VRAM headroom (20% buffer) | Per-replica `vram_min` budget + Scheduler bin-packs multiple tenants per GPU; per-GPU multi-tenant occupancy tracking |
| 3 | One service ↔ many vLLMs on different hosts | Multi-service, per-service backends | `backends` decoupled from `services`; `uses[]` + pluggable Wiring strategy; planner resolves backend → placed endpoints |
| 4 | Declarative config → zero-downtime transition | blue-green/proxy cutover + rollback per deploy | The planner: `plan(desired, observed) → ordered actions`, dependency-sorted to preserve the serving invariant across the fleet |

**Identity is preserved.** v2 stays a **single static binary, agentless,
one-shot CLI** (D1/D2). It does *not* become a daemon or control loop. The
declarative behaviour comes from a pure planning function invoked on demand
(from CI/GitOps), not from a continuous reconcile loop.

## 2. Decisions

Inherits all v1 decisions (D1–D13). New v2 decisions:

| ID | Decision |
|----|----------|
| V1 | Reconciler model = **planner over the existing engine** (one-shot, agentless). The planner is a pure function `plan(desired, observed) → []Action`; each action is executed by the unchanged v1 state machine. A continuous control-loop/daemon (k8s-style) is explicitly **not** built — if ever wanted, it wraps the same pure `plan()` and stays a future driver, not a rewrite. |
| V2 | Placement = **pin with scheduler assist**. `gpu: auto` hands placement to a bin-packing Scheduler; `gpu: [host:n, …]` pins explicitly. Both honoured in one plan. |
| V3 | Backend wiring = **pluggable Wiring strategy interface** (`nginx-upstream` \| `env-list` \| future mesh), same DNA as readiness/cutover/placement. A service never hard-codes backend URLs; the planner fills them from where replicas actually landed. |
| V4 | Fleet apply failure = **user-chosen policy** `--on-failure=hold\|rollback`, default `hold`. `hold` stops at the failed action (completed actions stay — each was health-gated; the failed action self-rolls-back via the v1 state machine; re-running `apply` resumes idempotently from actual state). `rollback` reverses applied actions in reverse dependency order. "Continue past a failed dependency" is **not** offered. |
| V5 | Multi-repo topology = **central fleet repo** blessed as default. App repos build+push their image and bump their tag (one-line PR) into the fleet repo; the fleet repo's CI runs `dockrail apply`. The enabling mechanism (**observe-whole / act-scoped** via `--scope`, plus a **fleet-wide lock**) is built regardless and leaves the door open to composition/fragment topologies later, but the docs point at the central-repo pattern. |
| V6 | `backends` are the placeable unit; `replicas` (desired count) is the scaling knob. Scaling a backend emits place/stop actions; each new replica is health-gated on the existing cutover path. |

## 3. Architecture

Two new **pure** components (Observer, Scheduler) and one new **pure** planner
sit above the v1 engine. Everything from the Connection layer down is unchanged.

```
 fleet.yml (desired) ──►  ┌─────────────────────────────────────┐
                          │  CLI:  plan · apply · status ·      │
                          │        rollback · lock (fleet-wide) │
                          └──────────────┬──────────────────────┘
                                         │
              ┌──────────────────────────┼───────────────────────────┐
              ▼                          ▼                            ▼
      ┌───────────────┐        ┌──────────────────┐         ┌──────────────┐
      │  Observer     │        │    Planner       │         │  Scheduler   │
      │ reads ACTUAL  │───────►│ diff desired vs  │◄────────│ bin-packs    │
      │ state across  │        │ actual → ordered │         │ auto GPU/host│
      │ all hosts     │        │ action list      │         │ placements   │
      └───────┬───────┘        └────────┬─────────┘         └──────────────┘
              │                         │ each action =
              │                         ▼ one existing state-machine run
              │        ┌────────────────────────────────────────┐
              └───────►│  v1 engine (per host): deploy · rollback │
                       │  · cutover · readiness · placement       │
                       │  · history · lock — UNCHANGED            │
                       └────────────────┬─────────────────────────┘
                                        ▼
                            Connection (SSH/local) per host
```

**Component responsibilities & boundaries:**

- **Observer** — for each host in the fleet, over SSH, reads running containers +
  their image tags + GPU occupancy (`nvidia-smi`, per-GPU used/free VRAM and
  which containers hold each GPU) + the per-host `history.jsonl`. Produces one
  `ObservedState` value for the whole fleet. Pure read, no mutation. This is
  v1 `status` grown fleet-wide.
- **Scheduler** — pure function. Given the desired backend replicas that request
  `gpu: auto` and the observed per-GPU VRAM headroom, bin-packs them into
  concrete `host:gpu` placements, honouring the v1 headroom rule (20% buffer)
  now applied **per tenant** so multiple models can share a GPU (#2). Explicit
  pins (`gpu: host:n`) pass through untouched and are subtracted from available
  headroom first. Emits a placement map or an actionable "nothing fits" error.
- **Planner** — pure function `plan(desired, observed, placements) → []Action`.
  Diffs desired vs actual and emits an **ordered, dependency-aware** action
  list, each action being something the v1 engine already does. `dockrail plan`
  prints it (the fleet-scale `--dry-run`); `dockrail apply` executes it.
- **v1 engine and below — UNCHANGED.** An action is one state-machine run.

The three new pieces are **pure functions**: unit-tested by feeding `desired`
and `observed` structs and asserting the emitted plan — no host, no fakes of
fakes. This matches the codebase's existing testing discipline (strategies with
fakes; engine against a fake Connection that records commands).

## 4. Config format (`fleet.yml`)

The single-target `deploy.yml` becomes a fleet document. It stays declarative:
you describe the desired end state; the planner computes the path. A v1
`deploy.yml` is the degenerate case (one host, no `auto` placement) and remains
valid.

```yaml
project: ml-platform

hosts:                                   # #1 — multiple targets, first-class
  gpu-a:
    ssh: deploy@gpu-a.example.com
    gpus: [0,1,2,3]
  gpu-b:
    ssh: deploy@gpu-b.example.com
    gpus: [0,1]

registry: { server: registry.example.com/acme/ml }

# ---- inference backends: the placeable units (#2, #3) ----
backends:
  llama-70b:
    image_tag: "vllm-v0.9.2"
    model: /models/llama70b/best
    replicas: 3                          # #3 — many vLLMs, one logical backend
    placement:
      vram_min: "20GiB"                  # per-replica budget → #2 sharding
      gpu: auto                          # scheduler bin-packs across pool...
      pool: [gpu-a, gpu-b]               # ...within these hosts
    readiness: { type: vllm, timeout: 300s }

  embed-small:
    image_tag: "vllm-v0.9.2"
    model: /models/bge/v1
    replicas: 2
    placement:
      vram_min: "6GiB"
      gpu: [gpu-a:2, gpu-a:3]            # pinned (#2 multi-tenant: shares gpu-a
                                         # with llama-70b if VRAM fits)
    readiness: { type: vllm, timeout: 180s }

# ---- routed services: consume backends (#3) ----
services:
  chat-api:
    host: gpu-a                          # where the API container runs
    image_tag: "${TAG}"
    uses:                                # #3 — service ↔ many backends
      - backend: llama-70b
        wiring: { strategy: nginx-upstream }   # planner writes upstream from
                                               # llama-70b's placed replicas
    readiness: { type: http, path: /health, port: 8080 }
    cutover:   { strategy: proxy }

  batch-worker:
    host: gpu-b
    uses:
      - backend: embed-small
        wiring: { strategy: env-list, var: EMBED_BACKENDS }  # injects URL list
    readiness: { type: http, path: /health, port: 9000 }
    cutover:   { strategy: recreate }
```

Three new top-level concepts, each mapping to a requirement:

- **`hosts`** (#1) — named targets with GPU inventory. Everything else references
  hosts by name; the Observer reads all of them.
- **`backends`** (#2, #3) — the *placeable inference unit*. `replicas` +
  `placement` is where GPU-sharding lives: `vram_min` is the per-replica budget,
  `gpu: auto` hands placement to the Scheduler (bin-packs, multi-tenant per GPU),
  `gpu: [host:n, …]` pins. A backend is a set of N identical vLLM containers the
  planner places and tracks.
- **`services` → `uses`** (#3) — a service names the backends it consumes and
  *how* they're wired in (the pluggable Wiring strategy). The service never
  hard-codes backend URLs; the planner fills them from wherever the replicas
  actually landed.

Design notes:

1. **Backend ↔ service decoupling** is what makes #3 work: `llama-70b` is placed
   once, and any number of services can `uses:` it. The planner resolves
   "backend name → concrete replica endpoints" at plan time.
2. **`replicas` as desired count** lets the Scheduler and planner do their jobs:
   scale a backend 3→5 and the planner emits "place 2 more" actions; the
   health-gated cutover applies per new replica.
3. **`x-` extension keys** remain ignored by validation (v1 convention).

## 5. The planner

### 5.1 Action model

`plan()` emits a typed, ordered list. Every action type is a thin wrapper over
the v1 engine — **no new execution primitives**:

| Action | Decomposes to (existing v1 engine) |
|--------|-------------------------------------|
| `DeployService{svc, host, tag}` | v1 deploy state machine on that host |
| `PlaceBackend{backend, replica, host, gpu}` | start container w/ gpu placement → vllm readiness |
| `MoveBackend{backend, replica, from→to}` | place new replica → prove ready → `Wiring.Add` → drain+stop old |
| `Rewire{service, backend}` | `Wiring.Switch` (nginx upstream flip / env-list update) |
| `ScaleBackend{backend, ±n}` | N × `PlaceBackend`, or drain+stop for scale-down |
| `Rollback{…}` | v1 rollback |

### 5.2 Ordering (dependency graph)

The planner topologically sorts actions so the v1 invariant — **OLD serves
until NEW is proven ready** — holds *across* the fleet:

1. Place/prove new backend replicas **before** rewiring any service to them.
2. Rewire services (atomic per-service, existing cutover strategy).
3. Drain+stop superseded replicas **last** — capacity never dips below desired
   mid-migration.

"Move a model across GPUs/hosts while serving" (#4's hard case) falls out of
this for free: `PlaceBackend(new)` → `Rewire` → `stop(old)`, health-gated at
each step. If the new placement fails readiness, the old is never touched.

### 5.3 Failure policy (V4)

`--on-failure=hold` (default): stop at the failed action; completed actions stay
(each was individually health-gated); the failed action self-rolls-back via the
v1 state machine (its OLD keeps serving); the run exits non-zero and reports
converged / pending / failed. Re-running `apply` resumes from actual state —
**idempotent by construction** (the planner recomputes the diff).

`--on-failure=rollback`: reverse the already-applied actions in reverse
dependency order back toward the pre-apply state. Honest caveat: reversing a
real model load / GPU move costs minutes and can itself fail; `hold` +
idempotent re-run is the recommended path for most pipelines.

## 6. Multi-repo workflow (V5)

The reconciler needs **one authoritative desired document**, but services live
in separate app repos with independent CI. v2 resolves this with a blessed
topology plus a general mechanism.

**Blessed default — central fleet repo (GitOps source of truth).** One repo
holds `fleet.yml`. An app repo's CI:

1. builds + pushes its image (unchanged — the tool never builds, D10);
2. bumps its tag in the fleet repo via a one-line change (e.g.
   `dockrail set-tag chat-api=$TAG` opening a PR);
3. on merge, the **fleet repo's** CI runs `dockrail apply`.

Single source of truth, fully auditable, one apply pipeline. Matches the
existing GitOps-workflow guide.

**Enabling mechanism (built regardless):**

- **Observe-whole / act-scoped** — `dockrail apply --scope chat-api` observes
  the entire fleet (required: placement and wiring cannot be computed from one
  slice) but diffs and acts only on the scoped slice.
- **Fleet-wide lock** — v1's per-project lock promoted to fleet scope, so
  concurrent applies (from different pipelines) serialise and cannot race on
  shared GPUs.

These two capabilities also enable, without further core work, the advanced
topologies documented as non-default: **scoped apply from each app repo** (repos
target a shared `fleet.yml`) and **config composition / fragments** (each repo
owns a fragment, a base owns host inventory, assembled via `include:`). The docs
recommend the central-repo pattern; the others are labelled advanced.

## 7. How the four requirements land

- **#1 Multi-server** → `hosts:` block + Observer reads all hosts + planner fans
  actions across them; execution stays per-host v1 engine.
- **#2 GPU sharding** → `backends[].placement.vram_min` per-replica budget +
  Scheduler bin-packs multiple tenants per GPU (headroom-respecting) + Observer
  tracks per-GPU multi-tenant occupancy; `gpu: auto | pinned`.
- **#3 Service ↔ many vLLMs** → `backends` decoupled from `services`;
  `services[].uses[]` + pluggable **Wiring** strategy; planner resolves backend
  name → placed replica endpoints and wires them.
- **#4 Declarative → zero-downtime** → the whole planner: `plan(desired,
  observed) → ordered actions`, dependency-sorted to preserve the serving
  invariant, with `plan` / `apply` / `--on-failure`.

## 8. Decomposition into buildable sub-specs

Too large for one implementation plan. v2 is five shippable, independently
testable sub-specs, each pure-function-heavy with a clean interface:

1. **Observer + fleet config** — parse `fleet.yml` (`hosts` / `backends` /
   `services`), observe actual state across hosts, `status` fleet-wide. No
   mutation — safe first step; a v1 `deploy.yml` must keep parsing.
2. **Scheduler** — pure bin-packer: `auto` placements + multi-tenant VRAM
   headroom. Pure function, heavy table tests (including "nothing fits" errors).
3. **Planner + `plan`** — diff → ordered actions, dependency sort, `dockrail
   plan` prints the fleet `--dry-run`. Still read-only.
4. **`apply` + `--on-failure`** — execute actions via the v1 engine, fleet lock,
   `--scope`, idempotent resume.
5. **Wiring interface** — `nginx-upstream` (fleet upstream from placed replicas)
   + `env-list` drivers, behind a strategy interface.

Recommended build order: 1 → 2 → 3 → 4 → 5 (observe before schedule before plan
before apply; Wiring can land alongside 4 since `apply` needs at least one
driver, but its interface is defined in 3 so the planner can emit `Rewire`).

## 9. Testing

- **Observer** — against a fake Connection returning canned `docker ps` /
  `nvidia-smi` output; assert the parsed `ObservedState`.
- **Scheduler** — pure table tests: desired replicas + observed headroom →
  expected placement map; include multi-tenant-per-GPU and infeasible cases.
- **Planner** — pure table tests: `(desired, observed)` → expected ordered
  action list; assert dependency ordering (place-before-rewire-before-stop) and
  idempotence (applying against already-converged state yields an empty plan).
- **apply** — against the fake Connection that records issued commands; assert
  the state-machine order per action, the fleet-lock acquisition, `--scope`
  filtering, and both `--on-failure` policies (hold leaves converged actions;
  rollback reverses in dependency order).
- **Wiring** — each driver against a fake; assert generated nginx upstream /
  injected env for a given set of placed endpoints.

## 10. Open items for reviewer

- **Observed state source of truth** — the per-host `history.jsonl` records
  intent; live `docker ps` / `nvidia-smi` records reality. When they disagree
  (out-of-band change), the planner trusts **reality** (observed) for diffing.
  Confirm this is the desired precedence.
- **Backend endpoint identity** — how a placed replica's `host:port` is derived
  (fixed port per backend + host, or discovered from the container). Affects
  Wiring URL generation. Leaning: declared `port` per backend, one replica per
  host:port, Observer confirms.
- **Shared-backend ownership** — in the central-repo model a backend used by two
  services is owned by the fleet repo; in advanced fragment mode this needs an
  explicit owner rule. Out of scope for the default, flagged for #2/#3.
- **`set-tag` helper** — whether the app-repo tag bump is a dockrail subcommand
  (`dockrail set-tag`) or left to the app repo's own tooling. Small, but shapes
  the blessed workflow.
