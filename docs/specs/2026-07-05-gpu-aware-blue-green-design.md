# GPU-Aware Blue-Green Cutover — Design Spec

**Status:** Design draft — needs your review before a plan is written. Several decisions below are marked **[DECIDE]**; defaults are proposed but not settled.

## Problem

the dogfood project's ML services are vLLM model servers on a single shared GPU host (`gpu-host.example.com`). Two constraints collide:

1. **Zero-downtime cutover.** the dogfood project already runs blue-green behind nginx (`mlops/` is the reverse proxy + autoheal). A `recreate` (stop-old-then-start-new) causes a downtime window plus a multi-minute model-reload gap — unacceptable for a live service.
2. **One model copy fits in VRAM.** You cannot run blue and green vLLM containers for the same service simultaneously — there isn't GPU memory for two full model copies. So the naive blue-green ("start green alongside blue, then switch") is impossible for GPU services.

The `on_no_free_gpu: stop-old-first` config field already names this exact tension. The task is to make dockrail sequence cutover around VRAM availability, gated on vLLM readiness, with an nginx upstream flip — and roll back cleanly when green fails to become ready.

## Current state (what exists vs. what's stubbed)

- `config` **already declares** `cutover.strategy: proxy`, `cutover.proxy` (e.g. `nginx-upstream`), `cutover.warmup`, and `placement: {type: gpu, pool, vram_min, on_no_free_gpu: fail|stop-old-first}`.
- `engine` implements **only** `recreate`; `strategy/placement` implements **only** `none`. Both `proxy` and `gpu` hit "not implemented yet".
- This design fills those two stubs, and they are coupled: GPU placement decides *whether* a classic blue-green is even possible, and the proxy strategy performs the flip.

## Proposed approach

Two composable pieces driven off the existing config:

### 1. GPU placement probe (`placement.type: gpu`)

Before starting green, query the target's GPUs and decide if there's room for a second copy.

- **Detection:** `nvidia-smi --query-gpu=index,memory.free --format=csv,noheader,nounits` over the connection, parsed into per-GPU free MiB. A GPU in `placement.pool` with `memory.free >= vram_min` is a free slot.
- **Decision:**
  - **Free slot exists** → true blue-green: start green on the free GPU (`CUDA_VISIBLE_DEVICES`), keep blue serving, flip nginx after green is ready, then stop blue. Zero gap.
  - **No free slot** → branch on `on_no_free_gpu`:
    - `fail` → abort the deploy, touch nothing (state records the reason). Safe default for capacity-critical services.
    - `stop-old-first` → **sequenced** cutover: flip nginx to a maintenance/booting state or drain, stop blue to free VRAM, start green on the freed GPU, wait for vLLM readiness, flip nginx to green. Has a gap (the model-reload window) but is the only option on a saturated GPU.

**[DECIDE] VRAM headroom.** `vram_min` is the model's footprint. Do we require `free >= vram_min` exactly, or `free >= vram_min * safety_factor` (e.g. 1.1) to leave room for KV-cache growth? Proposed default: require `vram_min` as-is and document that it should include KV-cache budget. → **your call.**

**[DECIDE] GPU-to-container binding.** How does the chosen GPU index reach the container? Options: (a) dockrail sets `CUDA_VISIBLE_DEVICES=<idx>` as an env var the compose file references (`device_ids: ${CUDA_VISIBLE_DEVICES}`); (b) the compose file already pins devices and dockrail only *checks* capacity, not assigns. Proposed: **(a)** — dockrail exports `DOCKRAIL_GPU=<idx>`, compose maps it. Requires a small compose convention on the dogfood project's side. → **confirm this is acceptable, or we use (b) check-only.**

### 2. Proxy cutover (`cutover.strategy: proxy`, `cutover.proxy: nginx-upstream`)

The nginx flip. the dogfood project's `mlops` nginx fronts the services, so "flip" = point the upstream at the new container and reload nginx.

**[DECIDE] Flip mechanism.** This is the biggest open question — how dockrail tells nginx to switch:

- **Option A — two named containers + upstream rewrite.** blue/green are distinct compose services (`svc-blue`, `svc-green`); dockrail rewrites the nginx `upstream` block to the active one and `nginx -s reload`. Most explicit, but dockrail must own/template an nginx conf fragment.
- **Option B — stable container name, compose recreate behind nginx.** nginx points at a fixed service name; "green" replaces "blue" under the same name. This is really the sequenced (gap) path and doesn't give true simultaneous blue-green. Simplest, but no zero-gap.
- **Option C — port swap.** blue on `:8000`, green on `:8001`; nginx upstream flips port and reloads. dockrail manages the port pair.

Proposed default: **Option A** for the free-slot (zero-gap) path, degrading to the sequenced path (B-like) when `stop-old-first`. → **needs your decision; it dictates the compose/nginx contract the dogfood project must follow.**

**Warmup (`cutover.warmup`).** When true and a free slot exists, after green is *ready* but before the flip, optionally send warmup requests so the first real request isn't cold. **[DECIDE]** what a warmup request looks like for vLLM (a trivial `/v1/completions`?) or whether readiness (`/v1/models` serving) is already sufficient. Proposed: readiness is sufficient; treat `warmup` as a no-op stub for v1 and revisit. → **your call.**

## State & rollback implications

- The single project-level tag pair still works: green's tag becomes `current`, blue's becomes `previous`, exactly as `recreate` does today via `finalize`.
- **Failure of green before the flip must not take blue down.** For the free-slot path this is natural (blue never stopped). For `stop-old-first`, blue is already gone when green fails — so rollback re-pulls and restarts blue's (previous) tag. That path reuses the existing `Rollback` machinery but must be triggered automatically on green-readiness failure. **[DECIDE]** auto-rollback on failed sequenced cutover, or leave the operator to run `dockrail rollback`? Proposed: **auto-rollback** for `stop-old-first` (since we caused the outage), manual for `fail` (nothing changed).

## Scope boundaries

- **In:** gpu placement probe (nvidia-smi), free-slot vs sequenced decision, nginx-upstream flip, readiness-gated cutover reusing the vllm prober, auto-rollback on sequenced failure.
- **Out:** multi-GPU model sharding, multi-host GPU pools, Swarm, `cutover.warmup` beyond a stub, non-nginx proxies. These are follow-ups.
- **Depends on:** the `vllm` readiness probe (separate plan) — GPU blue-green is only correct if "ready" means "model served", not "port open".

## Open decisions summary (need your answers to write the plan)

1. VRAM headroom: `vram_min` as-is, or with a safety factor?
2. GPU→container binding: dockrail assigns `DOCKRAIL_GPU` (compose convention), or check-only?
3. nginx flip mechanism: Option A (upstream rewrite) / B (stable name) / C (port swap)?
4. Warmup: no-op stub for v1, or real warmup requests?
5. Auto-rollback on failed `stop-old-first` cutover: yes (proposed) or manual?

Once you settle these, this becomes a task-by-task implementation plan (placement probe → proxy strategy → sequenced/auto-rollback → docs), mirroring the other two plans.
