# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this is

**`dockrail`** is a
single-binary CLI that deploys a **Docker Compose project** to a server over SSH
with **health-gated, zero-downtime cutover and rollback**, and that treats
**LLM/GPU services as first-class** (GPU placement, model warmup, "is it actually
serving tokens" readiness).

Positioning: a **general compose deployer whose headline feature is LLM-aware
cutover** — the thing no general deployer (Coolify, Dokploy, CapRover, Kamal,
Portainer) does. Intended for general release to anyone running containers,
especially self-hosted LLMs, on a single or home server. An internal ML
platform is the first dogfood user, but **keep dogfood-project specifics out
of the tool** — they belong in a `deploy.yml`, not in the code.

Status: **core implemented, v1 gaps remain** (see "v1 progress" below). The
authoritative design is
[`docs/specs/2026-07-05-dockrail-design.md`](docs/specs/2026-07-05-dockrail-design.md)
— read it before changing behavior.

## Core concepts (read the spec for detail)

- **One engine, pluggable strategies.** Generic and LLM services run through the
  same deploy state machine; they differ only by three interfaces:
  **Readiness** (`http` / `tcp` / `vllm` / `cmd`), **Cutover**
  (v1: `nginx-upstream`), **Placement** (`none` / `gpu`).
- **`deploy.yml` points at the existing compose file** and adds only the deploy
  metadata compose can't express (target host, readiness, cutover, placement,
  secrets, notify). The tool never replaces compose.
- **Deploy = start new alongside old → prove readiness → atomic proxy flip →
  drain+stop old.** Old keeps serving until new is proven. Rollback re-points to
  the previously running tag (recorded before cutover).
- **The tool does NOT build images.** CI builds+pushes; this tool deploys a tag.
- **Image tag = code version; mount path = model version** — two independent axes.

## Locked decisions

| ID | Decision |
|----|----------|
| D1 | Language = **Go** (single static binary, no runtime deps on host or CI) |
| D2 | Connection = **SSH-agentless** (Kamal model), or local exec on-host — same engine |
| D3 | v1 = **single host**; multi-host is v2 |
| D4 | One engine + pluggable **Readiness / Cutover / Placement** strategies |
| D5 | v1 cutover = **drive existing nginx** (`proxy: nginx-upstream`); proxy is an interface (Traefik/Caddy/managed later). **Not** shipping our own proxy in v1 |
| D6 | Networking = **bridge** (no `network_mode: host`) — what makes proxy cutover work |
| D8 | Secrets via host **`env_file`**, never on the command line |
| D9 | **Notifier is an interface**; engine emits events from day one, no channel ships in v1 (Telegram first in v2) |
| D10 | Tool **deploys**, does not build |
| D11 | Cutover = two strategies: `recreate` (blip) and `proxy` (zero-downtime, optional `warmup`); no `blue-green` |
| D12 | Two-slot = generated compose override with second named service (`<svc>-next`); live slot derived from running containers |
| D13 | GPU degrade = `placement.on_no_free_gpu: fail \| stop-old-first`, default `fail` |

## v1 scope

**In:** single SSH host · compose-native · health-gated cutover via existing
nginx · rollback · GPU placement + vLLM warmup readiness · secrets via env_file ·
preflight + `check` command · `deploy --dry-run` (plan print) · failure
forensics (failed NEW kept for inspection, log tail in output) ·
lifecycle hooks (`.dockrail/hooks`) · deploy history + `audit` ·
`retain_containers` retention with `rollback [TAG]` · lock with `--lock-wait` ·
`deploy` / `rollback` / `status` / `logs` / `check` / `config` / `audit` /
`lock` commands.

**Planned post-v1 (spec section 12):** destinations (`-d staging` overlay) ·
`exec` + aliases · maintenance mode · secrets adapters. Consciously skipped:
accessories, owning the proxy, image building, `asset_path`.

**Deferred (v2+):** multi-host / roles / destinations · managed/Traefik/Caddy
proxy drivers · notification channels (Telegram first) · more engines
(Ollama/TGI/llama.cpp) · web UI · agent daemon · image building.

## Conventions

- Don't use the `§` symbol in markdown documents — write "section 6" /
  "sect. 6" instead when cross-referencing.

- Match Go standard style (`gofmt`, `go vet`). Prefer small, interface-bounded
  packages: `connection`, `engine`, `strategy/{readiness,cutover,placement}`,
  `config`, `notify`, `cmd`.
- Every strategy is an interface with a fake for tests. The engine is tested
  against a **fake Connection that records issued commands** — assert the
  state-machine order and the rollback path.
- Never `git commit` / `git push` without an explicit request — leave changes in
  the working tree for review.
- Keep the tool generic. Anything that only makes sense for the dogfood project is a config
  concern or a bug.

## v1 progress

**Done** (build-order steps 1–6): cobra CLI (`deploy --dry-run` / `rollback` /
`status` / `logs` / `check`) · `deploy.yml` parse+validate (`config/`) ·
Connection SSH + local with fakes (`connection/`) · engine state machine with
preflight, host state, registry auth, secrets `env_file` (`engine/`) ·
`recreate` and `proxy` cutover with nginx-upstream flip, two-slot blue-green
and auto-rollback (`engine/bluegreen.go`, `engine/nginx.go` — note: cutover
lives in `engine/`, not `strategy/cutover/`) · readiness `http` / `tcp` /
`vllm` (`strategy/readiness/`) · `gpu` placement with nvidia-smi VRAM probe
(`strategy/placement/`) · deploy history + `audit` + `rollback [TAG]` +
`retain_containers` retention with log-tail capture (`engine/history.go`,
`engine/audit.go`, `engine/retention.go`, `cmd/audit.go`) · deploy lock with
`--lock-wait`, holder metadata, and lock status/acquire/release
(`engine/lock.go`, `cmd/lock.go`).

**Remaining for v1:**

1. `config` command.
2. Lifecycle hooks (`.dockrail/hooks`).
3. Dogfood on the internal ML services (routed API service first, then a
   GPU/vLLM one).

## Open items (see spec section 13)

- Real binary/repo name (placeholder `dockrail`).
- VRAM query: currently `nvidia-smi` parse (`strategy/placement/vram.go`);
  NVML binding still an option.
