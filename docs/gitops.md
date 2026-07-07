# GitOps-style workflow

dockrail fits a PR-driven deploy flow: git holds the desired state
(`deploy.yml`, including each service's `image_tag`), changes go through
pull requests, and CI runs `dockrail deploy` on merge.

## The flow

1. CI builds and pushes `registry.example.com/app:v42` (dockrail does not
   build images).
2. Open a PR editing `deploy.yml`: `image_tag: v41` -> `image_tag: v42`.
3. On the PR, CI runs `dockrail deploy --dry-run` so review sees the plan.
4. Merge to main. CI runs `dockrail deploy` — health-gated cutover; the old
   version serves until the new one is proven ready.
5. If readiness fails, dockrail rolls back automatically and the CI run goes
   red. Git still points at v42 — the red run is your alarm. Fix forward or
   open a revert PR.

**Rollback = a revert PR.** `dockrail rollback` remains available for
emergencies, but then git lags reality until a revert PR lands.

**Git vs `dockrail audit`:** git history is what was *desired*; `dockrail
audit` is what actually *happened* on the host (including auto-rollbacks).

## GitHub Actions

```yaml
name: deploy
on:
  pull_request:
    paths: [deploy.yml]
  push:
    branches: [main]
    paths: [deploy.yml]

concurrency: deploy-prod

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: goodsmileduck/dockrail/action@main
        with:
          version: v0.2.0            # pin a dockrail release
          config: deploy.yml
          ssh-key: ${{ secrets.DEPLOY_SSH_KEY }}
          known-hosts: ${{ secrets.DEPLOY_KNOWN_HOSTS }}
          dry-run: ${{ github.event_name == 'pull_request' }}
          lock-wait: 5m
```

## GitLab CI

The workflow is git convention, not GitHub coupling — the same flow on
GitLab (MRs instead of PRs):

```yaml
.install_dockrail: &install_dockrail
  - curl -fsSLo /usr/local/bin/dockrail
    "https://github.com/goodsmileduck/dockrail/releases/download/v0.2.0/dockrail-linux-amd64"
  - chmod +x /usr/local/bin/dockrail
  - mkdir -p ~/.ssh && chmod 700 ~/.ssh
  - printf '%s\n' "$DEPLOY_SSH_KEY" > ~/.ssh/id_ed25519 && chmod 600 ~/.ssh/id_ed25519
  - printf '%s\n' "$DEPLOY_KNOWN_HOSTS" >> ~/.ssh/known_hosts

plan:
  stage: test
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
      changes: [deploy.yml]
  script:
    - *install_dockrail
    - dockrail deploy --dry-run

deploy:
  stage: deploy
  resource_group: deploy-prod
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
      changes: [deploy.yml]
  script:
    - *install_dockrail
    - dockrail deploy --lock-wait 5m
```

Gitea/Forgejo runners are GitHub-Actions compatible; the action above
generally works as-is.

## The deploy lock

Concurrent deploys to the same host are serialized by a per-project lock on
the target. `--lock-wait 5m` makes a second deploy wait instead of failing —
use it in CI so back-to-back merges queue up. `dockrail lock status` shows
who holds the lock; `dockrail lock release` clears a stale one (e.g. after a
crashed deploy).

Do **not** wire `dockrail lock release` into automated cleanup: it has no
staleness check, so a script can force-release a lock held by a live, slow
deploy (LLM model warmup can legitimately take many minutes). Releasing is a
human decision.

## Repo layouts

- **Same repo as the app** (default): `deploy.yml` next to your compose
  file; path-filter CI to it.
- **Separate deploy repo**: a small repo holding `deploy.yml` (one dir per
  host/env if you like). Same CI job, just in that repo. No dockrail
  configuration differs between the two.

## Variables

`deploy.yml` supports a `vars:` block to avoid repetition while keeping the
file the complete, PR-reviewable truth:

```yaml
vars:
  registry: registry.example.com/team
  tag: "v42"
services:
  api:
    image_tag: "${vars.tag}"
```

Rules: only `${vars.name}` references, values only (not keys), no
environment lookups, no defaults, no nesting. `$${` escapes a literal
`${`. Referencing an undefined variable fails loudly. Secrets never go in
`vars:` — use `secrets.from_env` with a host `env_file`.

## What this is not (yet)

CI still pushes over SSH in this flow. A pull-based reconciler
(`dockrail reconcile` on the host, read-only git access, no inbound
credentials) is planned — see the design spec's deferred section.
