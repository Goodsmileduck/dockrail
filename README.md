# dockrail

`dockrail` is a single-binary Go CLI that deploys a Docker Compose project over
SSH with health-gated, zero-downtime cutover, rollback, and LLM/GPU-aware
readiness — like putting your Docker deploys on rails.

It is agentless: it drives the target host through the system `ssh` binary
(with connection multiplexing) and `docker` / `docker compose` already
installed there. No daemon, no agent to install.

> **Status:** early. The `deploy` (`recreate` and `proxy` cutover, `http`,
> `tcp`, and `vllm` readiness, `--dry-run`), `rollback`, `status`, `logs`, and
> `check` commands are implemented and tested.

## Install

Requires Go 1.26+.

```bash
go install github.com/goodsmileduck/dockrail@latest
```

Or grab a prebuilt binary from the [Releases](../../releases) page.

## Usage

```bash
dockrail check              # validate config + compose file + probe target host readiness
dockrail deploy --dry-run   # print the plan without mutating the host
dockrail deploy             # pull, recreate, wait for readiness, cut over
dockrail deploy --lock-wait 5m # wait for a concurrent deploy's lock instead of failing
dockrail rollback           # restore the previously deployed image tag
dockrail status             # show deployed + running tag per service
dockrail status --json      # same, as machine-readable JSON (for agents/scripts)
dockrail logs web --tail 50 # show a service's logs on the target host
dockrail audit              # print the deploy history recorded on the target host
dockrail lock status         # show whether the deploy lock is held
dockrail lock acquire        # take the deploy lock manually (freeze deploys)
dockrail lock release        # remove the deploy lock unconditionally (stale-lock override)
dockrail --version
```

All commands read `deploy.yml` by default (override with `-c/--config`).

## Configuration

```yaml
project: myapp
compose: docker-compose.yml
registry:
  server: ghcr.io
target:
  host: deploy@example.com   # empty = run locally
  port: 22
secrets:
  from_env: [DATABASE_URL, API_KEY]
services:
  web:
    image_tag: ghcr.io/me/myapp:sha-abc123
    readiness:
      type: http             # http | tcp | vllm | cmd
      path: /healthz
      port: 8080
      timeout: 90s
    cutover:
      strategy: recreate     # recreate | proxy
    placement:
      type: none             # none | gpu
```

Host deploy state (previous/current tag, last failure) lives on the target in
`~/.dockrail/<project>/state.json`, guarded by a per-project deploy lock.

## Readiness

`http` readiness polls the configured path on `localhost:<port>` from the
target host. `tcp` waits for the configured port to accept connections and
defaults to a 60s timeout.

`vllm` waits for `http://localhost:<port>/health` and, when `model:` is set on
the service, waits for that model id to appear in `/v1/models`. Its default
timeout is 600s because loading model weights can take minutes; override it
with `readiness.timeout`.

```yaml
services:
  parse-agent:
    image_tag: v2
    model: Qwen2.5-VL
    readiness:
      type: vllm
      port: 8000
      timeout: 900s
    cutover:
      strategy: recreate
```

## Proxy Cutover and GPU Placement

`cutover.strategy: proxy` uses blue-green Compose services named
`<service>-blue` and `<service>-green`. The service's `cutover.proxy` value is
the nginx container name that should be reloaded after dockrail writes
`$HOME/.dockrail/<project>/nginx/<service>.conf`.

Your nginx config must include the generated fragments:

```nginx
include /home/deploy/.dockrail/myapp/nginx/*.conf;
```

GPU-backed proxy services should map `${DOCKRAIL_GPU}` into their device
reservation. During a free-slot cutover, dockrail starts the inactive color
with `DOCKRAIL_GPU=<index>`, waits for readiness, flips nginx, then stops the
old color. A GPU is considered free when `nvidia-smi` reports
`memory.free >= vram_min * 1.2`, leaving 20% headroom for KV-cache growth.

If no configured GPU has enough free VRAM and `on_no_free_gpu:
stop-old-first`, dockrail stops the old color first to free VRAM, starts the
new color, waits for readiness, and flips nginx. If readiness fails in this
sequenced path, dockrail automatically restarts the old color and records the
failure. With `on_no_free_gpu: fail`, dockrail aborts before mutating
containers.

```yaml
services:
  parse-agent:
    image_tag: v2
    model: Qwen2.5-VL
    readiness: { type: vllm, port: 8000, timeout: 900s }
    cutover:   { strategy: proxy, proxy: mlops-nginx }
    placement: { type: gpu, pool: [0, 1], vram_min: 18GiB, on_no_free_gpu: stop-old-first }
```

## Secrets & private registries

`secrets.from_env` lists required environment variable names that `dockrail`
reads from its own process environment, such as the invoking shell or CI job.
Those values are forwarded to the target in a mode-600 env-file at
`~/.dockrail/<project>/env`, and every `docker compose` command sources that
file. If any listed variable is unset or empty, deploy aborts before any
service pull or recreate.

When `registry.server` is set, `dockrail` reads `DOCKRAIL_REGISTRY_USER` and
`DOCKRAIL_REGISTRY_PASSWORD` from its own environment. If both are present, it
runs `docker login` on the target before pulling images; if either is missing,
it skips login and assumes the host is already authenticated. Secret values are
written to the target env-file or login pipe, but are not passed as command
arguments to later compose commands or `docker login`.

## Docs

- [GitOps-style workflow](docs/gitops.md) — PR-driven deploys with GitHub Actions or GitLab CI

## Development

```bash
go test ./...     # run the suite
go vet ./...
gofmt -l .        # should print nothing
go build ./...
```

CI runs the same gate on every push, then cross-compiles the CLI for
linux/darwin/windows (amd64 + arm64). Pushing a `vX.Y.Z` tag builds and
publishes those binaries to a GitHub release.

## License

[MIT](LICENSE)
