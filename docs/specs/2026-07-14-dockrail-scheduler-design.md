# dockrail v2 — Scheduler (sub-spec 2 of the fleet reconciler)

**Date:** 2026-07-14
**Status:** Draft for review
**Scope:** Sub-spec 2 of the v2 fleet design
([`2026-07-14-dockrail-v2-fleet-design.md`](2026-07-14-dockrail-v2-fleet-design.md),
section 8). Builds on sub-spec 1 (Observer + fleet config, merged). Pure
bin-packing Scheduler — no I/O, no host contact.

---

## 1. Purpose

The Scheduler decides where `gpu: auto` backend replicas run. Given the desired
`fleet.Config` and the observed `observe.FleetState` (per-GPU VRAM), it emits a
concrete `host:gpu` assignment for every replica, honouring per-GPU VRAM
headroom and multi-tenant sharing (requirement #2). It is a **pure function** —
deterministic output for deterministic input — so the Planner's later diff is
stable and the whole thing is table-testable without a host.

Pins (`gpu: [host:idx]`) pass through as assignments (validated against
capacity); only `auto` replicas are bin-packed.

## 2. Interface

New package `fleet/schedule`:

```go
func Plan(cfg *fleet.Config, state observe.FleetState) (Placements, error)

type Assignment struct {
	Replica int    // 0-based replica index within the backend
	Host    string
	GPU     int
}
type Placements map[string][]Assignment // keyed by backend name

// ScheduleError reports the first replica that could not be placed.
type ScheduleError struct {
	Backend     string
	Replica     int
	NeededMiB   int // vram_min * SafetyFactor
	BestFreeMiB int // most-free candidate GPU in the pool after prior placements
}
func (e *ScheduleError) Error() string
```

Determinism: iterate backends by sorted name, replicas `0..N-1`, and candidate
GPUs by (host name, index). Same inputs → identical `Placements`.

## 3. Algorithm

Build a per-GPU **available-VRAM ledger** seeded from
`observe.GPUState.FreeMiB` for every schedulable GPU, then place in this order:

1. **Unschedulable hosts excluded.** A host with `HostState.Err != ""` is
   dropped from the ledger entirely (its capacity is unproven).
2. **Pins first.** Each `gpu: [host:idx]` assignment deducts
   `vram_min * SafetyFactor` from that GPU's ledger. If the pinned GPU cannot
   cover it (or the host is unschedulable / GPU index absent), return a
   `ScheduleError` — a pin is explicit intent and must not silently fall back.
3. **Auto replicas.** For each backend with `gpu: auto`, place each replica onto
   a GPU in its `pool` (host names) that still has room, chosen by the resolved
   **policy** (section 4), deducting `vram_min * SafetyFactor` as it goes.
4. **Same-backend anti-affinity.** Two replicas of the *same* backend never land
   on the *same* GPU (each vLLM replica wants dedicated VRAM). *Different*
   backends may share a GPU — that is the multi-tenant sharding case (#2). A GPU
   already holding replica R of backend B is not a candidate for replica R' of B.

If no candidate GPU fits a replica, return a `ScheduleError` naming the backend,
replica, needed MiB, and the best-available candidate's free MiB.

`SafetyFactor` = the existing 20% KV-cache headroom rule (1.2), reused (section 5).

## 4. Packing policy (pluggable)

Among the GPUs that fit a replica, the policy picks one:

| Policy | Rule | Use |
|--------|------|-----|
| `spread` (default) | most-free-VRAM GPU first (worst-fit) | resilience + serving throughput; one GPU loss takes out fewer replicas |
| `binpack` | least-free GPU that still fits (best-fit) | consolidate; leave whole GPUs free for big future models |
| `first-fit` | first GPU in (host, index) order that fits | simplest, fully deterministic |

Ties broken by (host name, GPU index) so every policy is deterministic.

**Config surface** — fleet-level default with optional per-backend override
(the standard scheduler pattern; matches dockrail's per-service-overrides-global
convention):

- `fleet.Config` gains `Scheduler struct { Policy string }` → `scheduler.policy`,
  default `spread`, validated ∈ {`spread`,`binpack`,`first-fit`}.
- `fleet.Placement` gains optional `Policy string` → `backends.<n>.placement.policy`,
  same validation.
- Resolution per backend: `backend.Placement.Policy` → `cfg.Scheduler.Policy` →
  `"spread"`.

```yaml
scheduler:
  policy: spread            # fleet default
backends:
  llama-70b:
    placement:
      gpu: auto
      pool: [gpu-a, gpu-b]
      vram_min: "20GiB"
      policy: spread         # optional per-backend override
```

## 5. VRAM math — shared `vram` package (altitude decision)

`parseMiB` (VRAM string → MiB) and the `1.2` headroom factor already exist,
unexported, in `strategy/placement` (`vram.go`, `gpu.go`). Rather than duplicate
the KV-cache headroom constant into a second scheduler or couple
`fleet/schedule` laterally to `strategy/placement`, extract them into a small
shared package:

```go
// package vram
func ParseMiB(s string) (int, error)   // moved from strategy/placement/vram.go
const SafetyFactor = 1.2               // moved from strategy/placement/gpu.go
```

`strategy/placement` is updated to call `vram.ParseMiB` / `vram.SafetyFactor`
(behaviour-identical; its existing tests must still pass). `fleet/schedule`
imports the same package. One source of truth for VRAM math and the headroom
policy.

## 6. Consuming `FleetState` — deferred decisions, resolved only as needed

The Scheduler is the first programmatic consumer of `FleetState`. It settles the
two items deferred from sub-spec 1's final review exactly as far as it needs:

- **`HostState.Err != ""` ⇒ host unschedulable** (section 3, step 1). The
  finer `Err` *stage-context* (unreachable vs nvidia-smi-missing) remains
  deferred — the Scheduler only needs usable/not.
- **Empty-slice JSON (`null` vs `[]`)** — irrelevant here (nil slices range
  fine in Go); remains deferred as cosmetic.

## 7. Scope boundary

- **No command in this sub-spec.** `Plan` is a pure library, exercised by the
  Planner (sub-spec 3) and by table tests — mirroring how the nvidia-smi parser
  shipped without a command. A `dockrail fleet plan --placements` debug view is
  consciously deferred to the Planner sub-spec.
- **Accounting is against observed free VRAM**, decremented as placements are
  made. The reconciler nuance — that OLD replicas still occupy VRAM during a
  rolling move — is the **Planner's** concern (sub-spec 3): it pre-adjusts the
  `FleetState` capacity view it hands to `Plan`. The Scheduler stays a pure
  bin-packer over whatever capacity it is given.

## 8. Testing

Pure table tests: `(cfg, FleetState) → expected Placements`, covering:
- spread vs binpack vs first-fit distribution over the same capacity;
- multi-tenant: two different backends sharing one GPU when both fit;
- same-backend anti-affinity: two replicas forced onto different GPUs;
- pins deducted before auto; a pin that overflows its GPU → `ScheduleError`;
- pool restriction (a backend only lands on its pool hosts);
- an `Err` host excluded from candidates;
- infeasible → `ScheduleError` with correct backend/replica/needed/best-free;
- determinism: same input yields identical output across runs.
Plus `vram.ParseMiB` retains `strategy/placement`'s existing VRAM table tests
(moved with the code), and `strategy/placement` still passes green after the
swap.

## 9. Build order (for the implementation plan)

1. Extract shared `vram` package (`ParseMiB` + `SafetyFactor`); repoint
   `strategy/placement`; confirm its tests still pass.
2. Config additions: `Scheduler.Policy` + `Placement.Policy` + validation.
3. `fleet/schedule`: ledger + pins pass, then `spread`; `Assignment`/`Placements`/
   `ScheduleError` types.
4. `binpack` + `first-fit` policies + policy resolution; anti-affinity; pool +
   Err-host handling.
5. Full table-test sweep incl. determinism and infeasibility.

## 10. Open items

- Whether replica *indices* should be stable across re-plans (they are, by
  construction — replicas are 0..N-1 in order — but the Planner will need to map
  a replica to an actual container name; that mapping is the Planner's, noted for
  sub-spec 3).
- `binpack` leaving a GPU with a sub-`vram_min` sliver is expected (that's
  consolidation); no defrag pass in v2.
