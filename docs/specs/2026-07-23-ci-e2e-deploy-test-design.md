# CI End-to-End Deploy Test Design

**Status:** design. Authoritative for the e2e CI work; read before implementing.

**Goal:** Prove `dockrail` actually deploys — over a real transport, against a
real Docker host — by running its four headline scenarios (proxy zero-downtime
cutover, recreate, rollback, failed-deploy forensics) end-to-end in CI, then
tearing everything down. Today the whole engine is exercised only against
`connection.Fake`; nothing runs the real SSH path or real `docker`.

## Positioning

This is a **test/CI concern only**. It ships no new tool behavior and adds no
code to the `dockrail` binary. All assets live under `test/e2e/` and
`.github/workflows/`. Nothing dogfood-specific goes in — the fixtures use
`example.com`-style placeholders and generic public tooling.

## Architecture: two tiers over one shared harness

The core discipline: **the scenario assertions exist once**, parametrized only
by transport (`CONN=local|ssh`) and image source. Both tiers run byte-identical
assertions, so the cheap tier is a faithful proxy for the expensive one.

### Tier A — `e2e-local` (every PR, including forks)

- Runs on an `ubuntu-latest` runner; `dockrail` drives the runner's **own
  Docker** via local exec (`connection.NewLocal`).
- v1/v2/bad images are built **locally** — no registry, so it works on fork PRs
  that have no secrets.
- Stands up the nginx fixture, runs all four scenarios, tears down.
- Fast (~1 min), free, no secrets. This is the gate that runs on every change.

### Tier B — `e2e-droplet` (manual `workflow_dispatch` only)

- `doctl` creates a small droplet, waits for **real SSH**, then `dockrail`
  deploys **over SSH**.
- v1/v2/bad images are built and **pushed to `ghcr.io`**; the droplet pulls them
  — exercising `docker pull` with registry auth via the `env_file` secret path.
- Runs the **same** scenario harness, then destroys the droplet.
- Dispatch-only keeps cost bounded and predictable; it can't run on fork PRs
  anyway (no secrets), so no `pull_request_target` risk.

### Why the droplet still earns its cost

It is the only thing that exercises: (a) the genuine SSH transport, (b)
`docker pull` from an authenticated registry via `env_file`, and (c) a host with
cold Docker and real network latency. Everything else the local tier covers for
free.

## Components

All under `test/e2e/` unless noted.

1. **Test app + images** — `app/Dockerfile`: a minimal static server (tiny Go
   binary or `busybox httpd`) exposing `GET /health` → `200` and
   `GET /version` → the tag baked in at build via `ARG VER`. Built as `:v1` and
   `:v2` so a cutover is a *provable* version swap, not a no-op. A `:bad`
   variant whose `/health` returns `500` drives the failed-deploy scenario.

2. **Proxy fixture** — `fixture/`: a user-defined `docker network` plus an nginx
   container whose http block `include`s `$HOME/.dockrail/<project>/nginx/*.conf`
   and `proxy_pass`es `:18080 → http://<service>`. This is the "existing nginx"
   `dockrail` drives (see `engine/nginx.go`): reload is
   `docker exec <name> nginx -s reload`, and the app containers must resolve by
   name (`<service>-<color>`) on this network. Brought up once per run before
   the proxy scenario.

3. **deploy.yml fixtures** — `deploy-proxy.yml` and `deploy-recreate.yml`,
   pointing at a shared `compose.yml` that attaches the app to the fixture
   network. A `${TAG}` var selects v1/v2/bad.

4. **Scenario harness** — `run.sh` (or a `//go:build e2e` Go test): the four
   scenarios as ordered assertions, parametrized by `CONN`/`HOST`. Each scenario
   runs in its **own project namespace** so the forensics leftovers don't bleed
   into the next.

5. **Droplet lifecycle** — `droplet.sh`: `doctl` create → wait-for-SSH →
   (deploy) → destroy. Plus a reaper workflow as the leak backstop.

6. **Workflows** — `.github/workflows/e2e.yml` (the two jobs) and
   `.github/workflows/e2e-reap.yml` (scheduled reaper).

## The four scenarios (identical across tiers)

1. **Proxy zero-downtime + no-blip assertion.** Deploy `:v1` behind the nginx
   fixture, confirm `/version` = v1. Start a background probe against **nginx**
   (the flip point) every ~100ms counting non-200s *and* connection resets.
   Deploy `:v2` via `cutover: {strategy: proxy}`. Assert: probe recorded **zero**
   failures across the flip window, and `/version` now = v2. Warm the probe ~1s
   before cutover so we measure the flip, not startup.

2. **Recreate (blip) deploy.** `:v1` → `:v2` via `cutover: {strategy: recreate}`.
   Assert the command exits 0 (readiness gated) and `/version` = v2 afterward.

3. **Rollback to previous tag.** After v2 is live, run `dockrail rollback` and
   assert `/version` = v1 again. Exercises history + retention + rollback over
   the real transport.

4. **Failed-deploy forensics.** Deploy `:bad` (never becomes ready). Assert:
   command exits non-zero, old container still serves (`/version` = previous),
   and the failed NEW container + log tail are left for inspection.

## Edge cases and mitigations

- **Droplet leak (the expensive failure).** `always()` destroy is not enough — a
  cancelled/crashed job or runner OOM can skip it. Defense in depth: name every
  droplet `dockrail-ci-<run_id>` and tag it `dockrail-ci`; a `concurrency:` group
  serializes dispatches; a **reaper** workflow (hourly cron) lists tag
  `dockrail-ci` and destroys any droplet older than ~30 min; a hard
  `timeout-minutes` on the job self-terminates a hang. The reaper is the real
  backstop.

- **"Active" ≠ "SSH-ready".** The droplet reports `active` before cloud-init
  finishes and sshd accepts keys. Inject the pubkey via user-data, then poll the
  SSH port (or `cloud-init status --wait` over ssh) with bounded retries before
  deploying. Host-key handling uses `accept-new` (or the fingerprint from
  `doctl`) — never blanket-disable verification, since the SSH path is precisely
  what is under test.

- **Zero-downtime measurement fidelity.** The probe must hit nginx, not the app
  container directly, and count both non-200s and connection resets. Warm ~1s
  before cutover. A single failure fails the test.

- **Registry pull path getting masked.** On the droplet, `:v2` must be a real
  pull — distinct tags prevent a cache shortcut, and auth flows through
  `dockrail`'s `env_file` secret. The local tier deliberately skips the registry;
  that is the tier split doing its job.

- **Docker-network name resolution.** `<service>-<color>` resolves only if the
  blue-green override attaches the app to nginx's user-defined network. If it
  does not, the proxy scenario fails loudly — a genuine bug to surface, not to
  paper over.

- **Fixture cleanup between scenarios.** The failed-deploy scenario intentionally
  leaves a broken container + log tail. Each scenario runs in its own project
  namespace, torn down after, so leftovers do not poison the next.

- **Fork PRs / no secrets.** Only the local tier runs on forks; the droplet tier
  is dispatch-only, so `DIGITALOCEAN_TOKEN` and ghcr creds are always present
  when it runs.

- **Runner port conflict.** The fixture nginx binds a fixed, uncommon host port
  (`18080`) to avoid collisions with anything preinstalled on the runner.

- **Flaky droplet creation.** Bounded retry on `doctl` create; smallest droplet
  size and nearest region to cap cost and latency.

## Secrets and configuration (Tier B)

- `DIGITALOCEAN_TOKEN` — repo secret, used by `doctl`.
- ghcr push uses the built-in `GITHUB_TOKEN` (write on same-repo dispatch).
- Ephemeral SSH keypair generated per run; pubkey injected at droplet creation,
  private key held only in the job.
- Droplet: smallest size, a fixed nearby region, a current Ubuntu image with
  Docker installed via user-data (or a Docker-preinstalled image).

## Out of scope (deliberately)

- **GPU / vLLM readiness scenarios** — need a GPU droplet; too costly for this
  pass. The `http` readiness path covers the state machine end-to-end.
- **Multi-host fleet e2e** — the fleet commands are read-mostly today; this
  covers the v1 single-host deploy engine.
- **Any change to the `dockrail` binary** — this is test/CI only.

## How we know it works

The scenarios *are* the test. The local tier runs them on every PR, so the e2e
harness itself is continuously exercised; the droplet tier proves the same
assertions hold over real SSH + a real registry pull on demand.
