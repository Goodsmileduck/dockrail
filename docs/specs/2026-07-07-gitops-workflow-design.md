# GitOps workflow (step 1: PR-driven deploys) + config `vars:` — design

Status: approved for planning
Date: 2026-07-07

## Goal

Make dockrail usable in a GitOps-style workflow, sequenced in steps. This spec
covers **step 1**: git holds the desired state (`deploy.yml`, including
`image_tag`), changes flow through pull requests, and a GitHub Action runs
`dockrail deploy` on merge. It also adds a `vars:` block to the config to
remove repetition while keeping the file fully reviewable.

Step 2 (pull-based reconciler) is deliberately deferred; its agreed decisions
are recorded below so they are not lost.

## Drivers

- PR-gated deploys: review, approvals, audit trail in the git host.
- A stepping stone to pull-based reconciliation (step 2), which adds the
  security win (no push credentials into prod) and drift correction. Nothing
  in step 1 is throwaway: the same git-holds-the-tag convention is exactly
  what the reconciler will consume.

Known trade-off accepted for step 1: CI holds SSH keys to the target host.
That is unchanged from today's push model and goes away in step 2.

## Workflow (convention, documented — not new engine behavior)

1. CI builds and pushes `registry/app:v42` (dockrail does not build — D10).
2. A human opens a PR editing `deploy.yml`: `image_tag: v41 -> v42`.
   (No bot bump PRs in step 1; may be added later.)
3. Review, merge to main.
4. A GitHub Action fires on merge (path-filtered to the deploy config) and
   runs `dockrail deploy`.
5. Health-gated cutover as today. On failure, dockrail auto-rolls back and
   the Action run goes red — git still says `v42`; the red run is the alarm.
6. Rollback = a revert PR. (`dockrail rollback` remains available for
   emergencies; git then lags reality until a revert PR lands.)

Desired-state location: **same repo as the app** is the default and the
documented happy path. A separate deploy repo works with zero extra code —
the Action just checks out that repo instead; it is a convention, not a mode.

## Deliverables

### 1. `dockrail-action` (composite GitHub Action)

- Download a pinned dockrail release binary (version input, checksum
  verified).
- Set up SSH: private key from a repo secret, known_hosts input.
- On pull requests: run `dockrail deploy --dry-run` and publish the plan
  print to the step summary, so review sees what will happen.
- On merge to main: run `dockrail deploy --lock-wait <duration>`
  (lock-wait guards overlapping merges).
- Inputs: config path (default `deploy.yml`), dockrail version, SSH key
  secret, known_hosts, lock-wait duration.
- Lives in this repo under `action/` initially; can move to a dedicated
  `dockrail-action` repo when published to the marketplace.

### 2. Config: `vars:` block

- New top-level `vars:` map (string -> string) in `deploy.yml`.
- Referenced as `${vars.name}` in any string **value** elsewhere in the file.
  Not in keys. Not recursive (no vars inside vars).
- Resolved as a single text pass on the raw YAML after read, before
  parse/validate — strict schema checking (`KnownFields`) sees final values.
- Unknown reference -> hard error naming the variable. Unused vars are
  allowed (a `check` warning is optional polish).
- `$${` escapes a literal `${` (compose convention).
- Deliberately **not** supported: environment lookups, defaults (`:-`),
  nesting. The file must remain the complete truth for PR review.
  Secrets continue to flow only via `secrets.from_env` / host `env_file` (D8).

### Git-platform portability

The workflow is git-convention only; dockrail has no GitHub coupling. The
composite Action is packaging convenience — the docs page includes an
equivalent `.gitlab-ci.yml` snippet (dry-run job on MRs, deploy job on main,
path-filtered to the deploy config). Gitea/Forgejo GitHub-compatible runners
may use the Action as-is. Step 2's reconciler is fully platform-agnostic
(read-only `git pull` from any remote).

### 3. Docs page: "GitOps-style workflow"

- The workflow above, end to end.
- Revert-PR-as-rollback story, and how `dockrail audit` history relates to
  git history (audit = what actually happened on the host; git = what was
  desired).
- Same-repo vs separate deploy-repo layouts.

### 4. CLI polish (verify, small)

- Confirm `deploy` is fully non-interactive with meaningful exit codes in CI
  (expected already true; verify and fix if not).
- Optional: nicer machine-readable/step-summary output. Not required for
  step 1.

## Deferred: step 2 — pull-based reconciler (decisions recorded)

Not in this plan. Agreed shape, so step 1 doesn't paint us into a corner:

- **One-shot first**: a `dockrail reconcile` command — pull repo (read-only),
  compare desired (`deploy.yml`) vs running state, run the normal deploy
  engine if they differ. Loop provided by a systemd timer initially; a
  `dockrail agent` daemon mode later is a thin wrapper (sleep loop, optional
  webhook listener).
- **Failure semantics: mark-and-hold with limited retry.** On a failed deploy
  of tag X at commit C: retry 2–3 times with backoff (transient failures),
  then record "X failed at C" in local host state and stop converging until
  the repo changes. Old version keeps serving. Surfaced via `dockrail status`
  and notifier events (D9).
- **The agent never writes to git.** Git access is read-only; reality is
  reconciled toward git, never the reverse.
- Runs on the target host via the existing local-exec connection (D2); no
  separate agent binary or codebase.

## Testing

- `vars:` interpolation: unit tests in `config/` — happy path, unknown var
  error with name, escape sequence, vars in values only, strict-schema
  interaction (typo in a field that came from a var still rejected).
- Action: exercised by a workflow in this repo against `--dry-run` with a
  fixture config (no real host); real-host use is dogfooding.
- Docs reviewed against an actual dogfood run of the PR flow.

## Out of scope

- Bot/auto bump PRs (Renovate-style).
- `reconcile` / `agent` (step 2).
- Multi-environment destinations (spec section 12 — separate files like
  `deploy.staging.yml`, not variables).
- Any git write access from dockrail.
