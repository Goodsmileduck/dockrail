# dockrail — Compose-native Deployer with LLM-aware Cutover

**Date:** 2026-07-05
**Status:** Draft for review
**Working name:** `dockrail` (placeholder — rename before release)
**Scope:** v1 (single-host) design for a new, standalone deployment CLI. the dogfood project is
the first dogfood user; the tool is intended for general release to anyone
running containers (especially self-hosted LLMs) on a single or home server.

---

## 1. What this is

A single-binary CLI that deploys a **Docker Compose project** to a server over
SSH with **health-gated, zero-downtime cutover and rollback** — and that
understands **LLM/GPU services** as a first-class case (GPU placement, model
warmup, "is it actually serving tokens" readiness), which no general-purpose
deployer (Coolify, Dokploy, CapRover, Kamal, Portainer) does today.

Positioning: a **general compose deployer whose headline feature is LLM-aware
cutover.** Compose is used as the already-known way to declare containers (vs.
plain `docker run` like Kamal); the tool adds the *deployment* behaviour compose
itself cannot express.

### Why not just adopt an existing tool

- **Kamal** — closest reference for transport + orchestration (SSH-agentless,
  multi-server config, health-gated cutover, rollback, hooks). But it does **not
  drive docker-compose** (runs `docker run` itself, positioned as a Compose
  alternative) and wants to **own the proxy/port**. It has **no** GPU/model
  concept. We copy its skeleton, not the tool.
- **docker-rollout / compose --wait** — solve generic zero-downtime but nothing
  LLM-specific, and historically broke on `network_mode: host` fixed ports.
- **Coolify / Dokploy / Portainer** — GUI-first PaaS/fleet tools; not CLI/CI-first,
  and none are LLM-aware.

### Relationship to the June 2026 ML-deploy spec

Supersedes the *mechanism* of `2026-06-26-ml-deploy-pipelines-design.md` (GitLab
CI templates + `docker-rollout` + per-repo `deploy.sh`) with a purpose-built
binary. It **keeps** that spec's correctness goals: image-tag-based deploy,
real rollback, no whole-home bind-mount, secrets via `env_file` not cmdline,
bridge networking instead of `network_mode: host`.

## 2. Goals / Non-goals

**v1 goals:**

1. Deploy a compose project to **one** SSH host (or locally) from a single
   static binary — no runtime dependency (no Ruby/Python) on host or in CI.
2. **Health-gated zero-downtime cutover**: start new container alongside old,
   prove readiness, atomically flip traffic, drain+stop old.
3. **Always-available rollback** to the previously running image tag.
4. **LLM/GPU as first-class**: GPU placement from a pool (VRAM-aware), model
   warmup, and readiness that means "actually serving tokens."
5. Generic and LLM services run through the **same engine**, differing only by
   pluggable **Readiness / Cutover / Placement** strategies.
6. Secrets injected via host `env_file`, never on the command line.
7. `Notifier` interface with engine events from day one (no channel ships in
   v1 — CI exit codes + structured logs are the signal).
8. Commands: `deploy` (with `--dry-run`), `rollback`, `status`, `logs`, `check`.

**Non-goals (deferred to v2+):** multi-host fan-out / roles / destinations ·
managed/Traefik/Caddy proxy drivers or a shipped proxy · more inference engines
(Ollama / TGI / llama.cpp) · notification channels (Telegram first, then
webhook/Slack/email) · web UI / dashboard · agent daemon on the host ·
secrets manager / SSH-CA integration · build of images (CI still builds+pushes;
this tool only deploys).

## 3. Decisions

| ID | Decision | Status |
|----|----------|--------|
| D1 | Language = **Go** (single static binary; matches `our other CLIs`; strong SSH + cobra) | Locked |
| D2 | Connection = **SSH-agentless** (Kamal model), or local exec when run on-host; same engine either way | Locked |
| D3 | v1 = **single host**; multi-host deferred | Locked |
| D4 | Engine = one state machine; generic vs LLM differ only by pluggable **Readiness / Cutover / Placement** strategies | Locked |
| D5 | Cutover proxy v1 = **drive the user's existing nginx** (`proxy: nginx-upstream`); proxy is a **strategy interface** so Traefik/Caddy/managed can be added later. **Not** shipping our own proxy in v1 | Locked |
| D6 | Networking = **bridge** (drop `network_mode: host`), which is what makes proxy-based zero-downtime work | Locked |
| D7 | Config = a `deploy.yml` that **points at** the existing compose file and adds deploy metadata only | Locked |
| D8 | Secrets = host `env_file` referenced by compose; injected from CI env | Locked |
| D9 | `Notifier` is an **interface**; the engine emits events from day one, but no channel ships in v1. First channel = Telegram in v2 | Locked (revised 2026-07-05) |
| D11 | Cutover = two strategies: `recreate` (blip) and `proxy` (zero-downtime, optional `warmup`); no standalone `blue-green` | Locked |
| D12 | Two-slot mechanism = generated compose override defining a second named service (`<svc>-next`); live slot recorded in the host state file | Locked |
| D13 | GPU degrade path = `placement.on_no_free_gpu: fail \| stop-old-first`, default `fail` | Locked |
| D10 | This tool **does not build images**; CI builds+pushes, tool deploys a tag | Locked |

## 4. Architecture

```
  deploy.yml  ──►  ┌────────────────────────────────────────┐
 (declarative)     │  CLI (cobra):  deploy · rollback ·      │
                   │  status · logs                          │
                   └───────────────┬────────────────────────┘
                                   │
        ┌──────────────────────────┼──────────────────────────┐
        ▼                          ▼                           ▼
  ┌───────────┐            ┌───────────────┐          ┌──────────────┐
  │ Connection│            │ Deploy engine │          │  Notifier    │
  │ SSH/local │◄───────────│ state machine │─────────►│ iface        │
  │ exec      │            │  + rollback   │          │ (v1: events  │
  │           │            │               │          │  only)       │
  └───────────┘            └───────┬───────┘          └──────────────┘
                                   │ strategy interfaces
                     ┌─────────────┼─────────────┐
                     ▼             ▼             ▼
              ┌───────────┐ ┌───────────┐ ┌─────────────┐
              │ Readiness │ │ Cutover   │ │ Placement   │
              │ http/tcp/ │ │ nginx-    │ │ none /      │
              │ vllm/cmd  │ │ upstream  │ │ gpu(pool)   │
              └───────────┘ └───────────┘ └─────────────┘
```

**Component responsibilities & boundaries:**

- **Connection** — runs a command string on the target (SSH with `ControlMaster`
  multiplexing, or local `exec`). Only this layer knows about SSH. Everything
  else calls `conn.Run(cmd)`.
- **Deploy engine** — owns the state machine (section 6) and rollback. Pure
  orchestration; delegates all decisions to the three strategy interfaces.
- **Readiness** — `Probe(ctx, svc) error`. Impls: `http` (path+port), `tcp`,
  `vllm` (fires a token-generating completion, waits for output), `cmd`.
- **Cutover** — `Switch(old, new) error` / `Revert()`. v1 impl:
  `nginx-upstream` (save current upstream conf, write new one, validate with
  `nginx -t`, then `nginx -s reload`; `Revert()` restores the saved conf and
  reloads). The interface is semantic ("atomically move traffic"), not
  file-based: future drivers differ in control model — Caddy = admin-API
  update, Traefik = container-label mutation picked up by its docker provider.
  Contract: `Switch` must not return until the driver has **confirmed** traffic
  reaches NEW (reload completion / API ack / discovery propagation), because
  step 7 stops OLD immediately after.
- **Placement** — `Pick(ctx, svc) (slot, error)`. Impls: `none` (generic),
  `gpu` (query `nvidia-smi`, choose a GPU from `pool` with ≥ `vram_min` free).
- **Notifier** — `Send(event)`; interface + fake only in v1 (no shipped
  channel; Telegram first in v2).

Each unit is independently testable: strategies are interfaces with fakes; the
engine is tested against a fake Connection that records issued commands.

## 5. Config format (`deploy.yml`)

One file per project. It references the existing compose file; the tool never
replaces compose, only augments it.

```yaml
project: generic-api-service
compose: docker-compose.prod.yml          # existing compose file, unchanged

registry:
  server: registry.example.com/acme/ml
  # auth via env: DEPLOY_REGISTRY_USER / DEPLOY_REGISTRY_PASSWORD

target:                                    # v1 = one host
  host: deploy@example.com
  port: 32
  # key via --ssh-key / agent; host key pinned in known_hosts

secrets:
  from_env: [APP_API_KEY, APP_DB_CONNECTION_URL]   # → host env_file

services:
  filter-service:
    image_tag: "${TAG}"                    # defaults to git SHA
    readiness: { type: http, path: /health, port: 8010, timeout: 90s }
    cutover:   { strategy: proxy, proxy: nginx-upstream }

  vllm:
    image_tag: "v0.9.2"                    # pinned explicitly (not ${TAG})
    model: /models/cd02/best_merged        # model version axis (mounted)
    placement:
      type: gpu
      pool: [0,1,2,3]
      vram_min: "20GiB"
      on_no_free_gpu: fail                 # or stop-old-first (accepts downtime)
    readiness: { type: vllm, prompt_probe: true, timeout: 300s }
    cutover:   { strategy: proxy, proxy: nginx-upstream, warmup: true }
```

Notes:
- **`readiness.type: vllm`** — the differentiator in config form; waits for real
  token output, not HTTP-200.
- **`placement.type: gpu`** — declarative "pick a free GPU, warm up there."
- **`cutover.strategy`** — per service, exactly two (D11): `recreate` (brief
  blip, simplest) and `proxy` (zero-downtime via a proxy driver; v1 driver =
  `nginx-upstream`). `warmup: true` on `proxy` starts NEW, warms it up, then
  flips — what used to be called blue-green.
- **Image tag = code version; mount path = model version** — two independent,
  explicit axes (inherited from the June spec).

## 6. Deploy state machine

Identical for generic and LLM services on the zero-downtime path (`proxy`);
they differ only at steps 3 and 5. `recreate` is the exception: it skips the
overlap (steps 4/6/7 collapse to stop OLD → start NEW → probe), accepts a
brief blip, and its rollback is re-pull+start the recorded old tag. The
invariant below applies to the `proxy` strategy only.

```
 0. preflight (same checks as `dockrail check`, see section 8);
    remove any failed NEW left by a previous run
 1. pull image :TAG on host
 2. record CURRENT (running container id + image tag)        ← rollback anchor
 3. [placement=gpu] pick a free GPU from pool (≥ vram_min); bind new container
        none free ─► on_no_free_gpu=fail: abort with actionable error
                     on_no_free_gpu=stop-old-first: switch to that sequence (section 7)
 4. start NEW container alongside OLD (generated compose override
    defining `<svc>-next`, per D12)
 5. readiness probe NEW  (http | tcp | vllm-token | cmd)
        fail ─► stop NEW but KEEP the container for inspection, keep OLD
                serving, emit log tail of NEW, notify(failure), exit non-zero
 6. cutover: proxy.Switch(OLD → NEW)   (atomic; v1 = nginx upstream + reload)
 7. drain + stop OLD    ([gpu] frees its GPU back to the pool)
 8. prune old images (retention), notify(success)
        └─ any error after step 6 ► rollback: proxy.Switch(NEW → OLD),
           notify(rollback), exit non-zero
```

`deploy --dry-run` executes steps 0–3 read-only, then prints the resolved plan
(image to pull, slot/container names, GPU that would be picked, nginx conf
diff) without touching host state; the plan rendering is the same structured
step format the real deploy logs.

Invariant: **OLD keeps serving until NEW is proven ready.** A bare
`docker compose up -d` cannot guarantee this; this is the tool's core value.

## 7. LLM/GPU behaviour (the differentiator)

- **Placement** (step 3): query `nvidia-smi` for a GPU in `pool` with ≥
  `vram_min` free; bind NEW via compose `device_ids`. OLD keeps its GPU until
  step 7, so a rolling model swap normally lands NEW on a *different* free GPU
  (the common case with 4 GPUs); step 7 releases OLD's GPU.
- **Warmup + readiness** (step 5): `vllm` probe issues a tiny completion and
  waits for tokens, long timeout (model loads take minutes). "Ready" = serving.
- **Degrade path** (D13): when no GPU in `pool` has ≥ `vram_min` free — the
  normal case on a single-GPU host, where OLD holds the only GPU — behaviour is
  `placement.on_no_free_gpu`. Default `fail`: abort with an error naming the
  fix. `stop-old-first`: stop OLD → start NEW on the freed GPU → probe → on
  probe failure restart OLD. This is **not** zero-downtime, and its failure
  mode means minutes of downtime (model reload); the spec is honest about that
  — it is the inherent cost of one GPU.
- Generic services set `placement: none` and an `http`/`tcp` probe; they use the
  exact same engine.

## 8. Cross-cutting behaviours

- **Rollback** — `dockrail rollback`: re-point proxy to the prior container
  if still present, else re-pull+start the previous tag recorded in step 2.
- **Deploy state on host** — the step-2 rollback anchor (previous tag +
  container id) is persisted in a per-project state file **on the target host**,
  so `rollback` and `status` work from any machine/CI runner, not just the one
  that deployed.
- **Deploy lock** — a per-project lock file on the host guards the whole state
  machine; a second concurrent deploy fails fast with a clear message (stale
  locks detectable via recorded pid/timestamp).
- **Drain** (step 7) — configurable `drain_timeout` per service before OLD is
  stopped. On the `nginx-upstream` path, `nginx -s reload` already lets
  in-flight requests finish on old workers; the timeout matters for
  long-running streaming (LLM) responses.
- **Secrets** — written to a host `env_file` referenced by compose `env_file:`;
  never passed on a command line (fixes June spec gap G6). File is `0600`,
  owned by the deploy user. Secret-only changes still require a deploy to take
  effect (containers are recreated on deploy).
- **Preflight / `check`** — before step 1 of every deploy, and standalone as
  `dockrail check`: SSH reachable, docker + compose present, registry
  login OK, compose file parses, nginx conf path writable + `nginx -t` passes,
  `nvidia-smi` present when any service uses `placement: gpu`. Converts
  mid-deploy environment failures into clean pre-failures.
- **Failure forensics** — a NEW that fails readiness is stopped but **kept**
  (removed by the next deploy's step 0); the failure output includes the tail
  of its logs, and `status` reports the failed attempt (tag, step, container
  kept) from the host state file.
- **Notify** — the engine emits success/failure/rollback events (project, tag,
  duration, target) through the `Notifier` interface. v1 ships no channel —
  exit codes + structured logs are the signal; Telegram is the first v2
  channel, then webhook/Slack/email.
- **Status / logs** — `dockrail status` shows running tag + health per
  service; `dockrail logs <svc>` tails. Deploy emits structured
  step-by-step logs so a CI log reads cleanly (same rendering as `--dry-run`).

## 9. Security

- SSH-agentless with a **dedicated `deploy` user** (not personal `youruser`),
  locked-down key (`restrict`, `from=<runner-ip>` in `authorized_keys`), pinned
  host key (`known_hosts`), ed25519.
- No secrets on command lines (section 8). No whole-home bind-mount (models mounted
  read-only, HF cache mounted).
- Registry auth from CI env vars, not committed.

## 10. Testing

- **Unit**: each strategy against fakes; engine against a fake Connection that
  records the command sequence (assert the state-machine order + rollback path).
- **Readiness probes**: table tests incl. the `vllm` token-probe (mock server
  that streams tokens after a delay to exercise the warmup timeout).
- **Integration (dogfood)**: deploy `generic-api-service` to the real host; verify
  running tag == pushed tag, a deliberately-broken tag rolls back and leaves OLD
  serving, GPU released after cutover, `/home` not mounted.

## 11. v1 build order

1. Skeleton: cobra CLI, `deploy.yml` parse+validate, Connection (SSH+local),
   preflight checks + `check` command (byproduct of config-parse work).
2. Engine state machine with `recreate` cutover + `http` readiness +
   `placement: none` — deploy a *generic* service end-to-end. `--dry-run`
   lands here (the fake-Connection test harness pointed at stdout).
3. Rollback + `status`/`logs` (incl. failed-attempt reporting).
4. `nginx-upstream` cutover strategy (zero-downtime for the routed service),
   two-slot compose override (D12).
5. `gpu` placement (incl. `on_no_free_gpu`) + `vllm` readiness (warmup) — the
   differentiator.
6. Secrets `env_file`.
7. Dogfood on the dogfood project `generic-api-service`; then `gpu-llm-service`.

## 12. Open items for reviewer

- **Real binary/repo name** (placeholder `dockrail`) and where it lives
  (its own repo vs. inside the workspace).
- **VRAM query source** — `nvidia-smi` parsing vs NVML binding.
- **Config secrets for registry** — confirm all via env (no file).

Resolved 2026-07-05 (now decisions D11–D13 in section 3): cutover-strategy
collapse (`blue-green` removed), two-slot mechanism (generated compose
override, `<svc>-next`), GPU degrade path (`on_no_free_gpu`). Also resolved:
Telegram demoted to v2 (D9 revised); note the two-slot choice was
sanity-checked against the future Traefik driver's label/discovery model —
`--scale` was rejected partly because Traefik's docker provider would
load-balance across both replicas, breaking the OLD-serves-until-NEW-proven
invariant.
