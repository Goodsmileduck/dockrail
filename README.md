# dockrail

`dockrail` is a single-binary Go CLI that deploys a Docker Compose project over
SSH with health-gated, zero-downtime cutover, rollback, and LLM/GPU-aware
readiness — like putting your Docker deploys on rails.

It is agentless: it drives the target host through the system `ssh` binary
(with connection multiplexing) and `docker` / `docker compose` already
installed there. No daemon, no agent to install.

> **Status:** early. The `deploy` (recreate cutover, `http` readiness,
> `--dry-run`), `rollback`, `status`, `logs`, and `check` commands are
> implemented and tested. The `proxy` cutover and `gpu` placement remain
> stubbed and return `not implemented`.

## Install

Requires Go 1.26+.

```bash
go install github.com/goodsmileduck/dockrail@latest
```

Or grab a prebuilt binary from the [Releases](../../releases) page.

## Usage

```bash
dockrail check              # validate config + probe target host readiness
dockrail deploy --dry-run   # print the plan without mutating the host
dockrail deploy             # pull, recreate, wait for readiness, cut over
dockrail rollback           # restore the previously deployed image tag
dockrail status             # show deployed + running tag per service
dockrail logs web --tail 50 # show a service's logs on the target host
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
