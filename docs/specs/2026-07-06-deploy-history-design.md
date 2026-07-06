# Deploy history, audit, rollback [TAG], retention — design

Status: approved 2026-07-06. Implements the deploy-history cluster from the
v1 scope (main design doc, section 8). Supersedes the flat
`state.json` (`previous_tag`/`current_tag`/`last_failure`) written by the
current code; no live hosts carry that format, so it is replaced without
migration.

## 1. History storage (host)

Per-project, append-only `$HOME/.dockrail/<project>/history.jsonl` on the
target host. One JSON record per line, one line per deploy or rollback
attempt:

```json
{"ts":"2026-07-06T11:45:00Z","tag":"v1.3.1","services":{"api":"g7h8i9"},"performer":"alice","outcome":"deployed"}
```

Fields:

- `ts` — RFC 3339 UTC timestamp, set on the invoking machine.
- `tag` — image tag deployed (or restored, for rollback records).
- `services` — map of service name to container id as of the attempt
  (empty allowed for failed attempts that never started a container).
- `performer` — `$USER` on the invoking machine; `DOCKRAIL_PERFORMER`
  env var overrides (CI).
- `outcome` — `deployed` | `failed@<step>` (e.g. `failed@readiness`) |
  `rolled-back` (a rollback record; its `tag` is the tag restored).

Semantics:

- Records are appended via shell `>>` over the Connection, under the
  existing per-project deploy lock. The file is never rewritten.
- The last record with `outcome: deployed` (or `rolled-back`) is the
  **current** anchor; the latest earlier record with a different tag and a
  successful outcome is **previous**. This replaces the `PreviousTag` /
  `CurrentTag` fields in `engine/state.go`.
- Rollbacks append their own `rolled-back` record; nothing is deleted.

## 2. `audit` command

`dockrail audit [-n 20]` reads `history.jsonl` over the Connection and prints
a table — timestamp, tag, performer, outcome — oldest first, newest last,
limited to the last `-n` records (default 20). The current anchor record is
marked. Like `status`, it works from any machine that can reach the host.

## 3. `rollback [TAG]`

- **No argument** — unchanged behavior: restart the stopped slot container if
  present, else re-pull and start the previous tag; the previous tag now
  comes from history instead of `state.json`.
- **With TAG** — validate that the tag appears as a successful record within
  the last `retain_containers` distinct deployed tags **and** the image is
  still present on the host. If valid, run the normal cutover path with that
  tag (`compose up` with `TAG=<tag>` — readiness-gated, same engine). If not,
  fail with a clear error listing the valid candidate tags from history.
- Every rollback appends a `rolled-back` record naming the restored tag.

## 4. Retention and log capture

New project-level config key `retain_containers` (default 5): the number of
most recent distinct successfully-deployed tags kept as rollback targets.
(The name is kept for spec continuity; the retained unit is **images plus
captured log tails**, not containers — the two-slot model (D12) reuses the
`<svc>-blue`/`<svc>-green` container names, so at most one stopped old
container exists per service and a "last N stopped containers" set cannot
exist without un-managing containers from compose.)

Mechanics:

- **Log capture** — at deploy start, before `compose up` recreates the
  target slot's existing container, capture
  `docker logs --tail 1000` of that container to
  `$HOME/.dockrail/<project>/logs/<svc>-<tag>-<ts>.log`. This preserves a
  truncated forensic log for each generation, since Docker deletes a
  container's logs when the container is removed.
- **Image pruning** — post-deploy (after cutover succeeds), remove images
  whose tags fall outside the last `retain_containers` distinct `deployed`
  tags in history. Only images belonging to the service's configured image
  repository are candidates; unrelated images are never touched.
- **Log pruning** — saved log files are pruned by the same tag window.
- **Forensics exemption** — a failed NEW container (and its image) is exempt
  from pruning until the next deploy's cleanup step, per the existing
  failure-forensics rule.
- Retention is best-effort: pruning failures are reported but do not fail
  the deploy.

## 5. Testing

Per repo convention, everything runs against the fake Connection that
records issued commands:

- Engine state machine appends history records at the right points and in
  the right order (attempt start/outcome, rollback record).
- `audit` renders correctly from canned `history.jsonl` content, including
  the `-n` limit and anchor marking.
- `rollback TAG` — accepted tag, tag missing from history, tag in history
  but image gone.
- Retention selects the correct prune victims (images and log files) and
  respects the forensics exemption.
- Log capture command is issued before the slot's `compose up`.

## 6. Main-spec amendments

Section 8 of `2026-07-05-dockrail-design.md` is amended:

- Retention wording changes from "the last `retain_containers` stopped OLD
  containers and their images are kept" to "the last `retain_containers`
  deployed images are kept as rollback targets, with a captured log tail per
  generation" — rationale: D12's two-slot model reuses container names.
- The log-tail capture feature is added to the forensics paragraph.
