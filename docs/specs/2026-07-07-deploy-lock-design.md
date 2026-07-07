# Deploy lock: `--lock-wait`, metadata, `lock` command — design

Status: approved for planning
Date: 2026-07-07

## Goal

Finish the v1 lock story. Today `engine/lock.go` takes a per-project
`mkdir`-based lock on the host and fails fast on collision; a crashed deploy
leaves the lock held forever with no tooling to inspect or clear it. This
spec adds: holder metadata, a `--lock-wait` flag on `deploy` and `rollback`,
a `dockrail lock` command, and a `lock-wait` input on the GitHub Action.

## Decisions

- **Stale-lock strategy: metadata + manual release.** No TTL auto-expiry —
  LLM deploys are legitimately slow (image pull + model warmup can exceed
  any reasonable TTL), so auto-breaking a lock mid-flight is worse than
  making a human decide. The lock error always shows who holds it and since
  when; `dockrail lock release` is the explicit override.
- Lock stays per-project, `mkdir`-based, on the target host (atomic on any
  POSIX filesystem, works over SSH and local exec alike). No ownership or
  permission tiers.

## 1. Lock metadata

After the `mkdir` succeeds, `acquireLock` writes `<lockdir>/info.json`:

```json
{"acquired_at": "2026-07-07T12:00:00Z", "tag": "v42", "by": "user@host"}
```

- `by` = `os/user.Current()` + `os.Hostname()` of the machine running
  dockrail (the deployer, not the target).
- `tag` = the tag being deployed; empty for `rollback` (tag not yet known at
  lock time) and `lock acquire`.
- Metadata write failure does not fail the acquisition (the mkdir is the
  lock; the file is advisory) and is silently tolerated — the collision
  error's "no holder metadata" fallback covers the resulting gap.
- **Accepted gap:** a crash or dropped connection between the `mkdir` and
  the metadata write leaves a held lock with no `info.json` — reported as
  "no holder metadata", indistinguishable from a pre-metadata lock dir.
  The remedy is the same either way (`lock release`), so this is not
  worth closing. Locks created before this change (bare dirs) degrade to
  the same message.
- SSH exit-status ambiguity (network drop after the server-side `mkdir`
  succeeded reports failure to the client) can only produce a stuck lock —
  never two holders. The failure direction is safe; the remedy is
  `lock release`. Accepted.
- On collision, the error reads `info.json` and reports:
  `another deploy appears to be running: lock held by <by> since
  <acquired_at> (deploying <tag>)`. If the file is missing or unreadable:
  `... lock held (no holder metadata)`.
- Release removes `info.json` (best effort) then `rmdir`s the lock dir.

## 2. `--lock-wait <duration>`

- New flag on `deploy` and `rollback`. Default `0` = current fail-fast
  behavior, fully backward compatible.
- Semantics: poll acquisition every 5 seconds until the duration expires.
  On the first collision, print one line:
  `waiting for deploy lock (held by <by> since <acquired_at>)`. Subsequent
  retries are silent. On success, proceed normally. On timeout, return the
  holder-info error above.
- Implemented as `acquireLockWait(ctx, conn, project, wait, out)` in
  `engine/lock.go`, wrapping `acquireLock`. Respects ctx cancellation
  (Ctrl-C stops waiting immediately).
- The engine gets the wait duration via a new `LockWait time.Duration`
  field on `engine.Engine` (zero value = fail fast). Both cmd sites need
  wiring: `cmd/deploy.go` and `cmd/rollback.go` each register the
  `--lock-wait` flag and set the field — rollback currently registers no
  flags at all, and `RollbackTo` takes the same lock. Other Engine
  construction sites (status, audit) don't lock and need no change.

## 3. `dockrail lock` command

`cmd/lock.go`, three subcommands; each loads `-c` config for target host and
project, like other commands:

- `dockrail lock status` — prints `free` or
  `held by <by> since <acquired_at> (deploying <tag>)`.
- `dockrail lock acquire` — takes the lock manually (e.g. to freeze deploys
  during host maintenance). Fails with the holder error if already held.
  Writes metadata with empty tag.
- `dockrail lock release` — removes the lock unconditionally, printing who
  held it. Releasing a free lock says `lock is not held` and exits 0.
  No `--force` tier: release is the force; running the command is the
  consent. This is the escape hatch for stale locks.
  **Accepted risk:** because release has no staleness check, a scripted
  `lock release` (e.g. a CI cleanup step) can force-release a live lock
  held by a slow-but-healthy deploy. It is a human override by design;
  docs/gitops.md must warn against wiring it into automated cleanup.

Exit codes: `lock status` exits 0 when free, 1 when held, 2 on
connection/config error — so CI can script against it. `acquire` and
`release` exit 0 on success, non-zero on failure.

## 4. Action and docs

- New `lock-wait` input on `action/action.yml` (default `5m`): non-empty
  value appends `--lock-wait <value>` to the deploy invocation. The
  `extra-args` description drops its "once available" reference to
  lock-wait.
- `docs/gitops.md`: warning that `lock release` must not be wired into
  automated cleanup (it can force-release a live slow deploy); GitHub
  Actions example gains `lock-wait: 5m`
  (with the concurrency-group note kept — GitHub `concurrency` serializes
  runs, lock-wait guards everything else: manual deploys, other repos
  targeting the same host); GitLab example gains `--lock-wait 5m`.
- README command list mentions `lock` and `--lock-wait`.
- CLAUDE.md "Remaining for v1" updated (items 1–2 done by this work).

## Testing

Engine tests against the fake connection (existing pattern):
- Acquire writes metadata; release removes file then dir.
- Collision error includes holder metadata; degrades gracefully when
  `info.json` is missing.
- `acquireLockWait`: acquires immediately when free; succeeds when the lock
  frees mid-wait (fake conn: fail mkdir N times, then succeed); returns
  holder error on timeout; aborts on ctx cancel.
- Poll interval injectable (package-level var or param) so wait tests run
  in milliseconds, not seconds.

Cmd tests: `lock status` / `acquire` / `release` against a fake connection,
including release-when-free and status-when-held-without-metadata.

## Out of scope

- TTL auto-expiry (rejected — see Decisions).
- Lock ownership checks or permission tiers.
- Per-service locks.
- Locking for read-only commands (`status`, `logs`, `audit`, `check`).
