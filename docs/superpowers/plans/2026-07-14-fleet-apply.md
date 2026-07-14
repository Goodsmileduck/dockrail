# Fleet Apply Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax. **Executors: Codex `gpt-5.6-sol` (reasoning medium), driven directly per the `codex-subagent-execution` memory (`codex exec ... < /dev/null` backgrounded, GOCACHE=/tmp/gocache, controller commits + reviews).**

**Goal:** `dockrail fleet apply` — execute the Planner's phased plan across the fleet via generated per-replica compose overrides, health-gated, with `--on-failure`, fleet lock, `--scope`, and Err-host refusal.

**Architecture:** Sub-spec 4 of the [v2 fleet design](../../specs/2026-07-14-dockrail-apply-design.md). New `fleet/apply` package: pure override generation, a per-action executor against a `connection.Connection`, phase orchestration, and the `Wiring` interface (no-op default; real drivers = sub-spec 5). Reuses `strategy/readiness`, the v1 lock, and history. Backends/services become compose services (D7 preserved).

**Tech Stack:** Go 1.26, existing `fleet`, `fleet/plan`, `fleet/observe`, `connection`, `strategy/readiness`, `engine` (lock/history) packages.

## Global Constraints

- Module path `github.com/goodsmileduck/dockrail` verbatim.
- Override generation is PURE (string in → override YAML out); the executor's only I/O is through `connection.Connection`.
- The `dockrail.*` labels stamped on launch MUST match the schema the Observer reads (`observe.LabelManaged/Backend/Replica/GPU/Service`) — reference those consts, do not re-spell the strings.
- Image tags are interpolated into shell/compose; reuse the existing `safeTag` charset guard pattern from `engine` (reject unsafe tags) before using a tag in a command.
- Phases execute in order converge → rewire → drain; converge/rewire actions are health-gated; a failed converge action keeps its container for forensics.
- gofmt + `go vet ./...` clean; test with `GOCACHE=/tmp/gocache`.
- Commit trailer on every commit: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Do NOT change v1 `engine` behavior; only add exported helpers if a task needs to reuse an unexported one (note it).

---

### Task 1: Config `compose`/`service` fields + per-replica override generation

**Files:**
- Modify: `fleet/config.go`, `fleet/config_test.go`
- Create: `fleet/apply/override.go`, `fleet/apply/override_test.go`

**Interfaces:**
- Produces: `fleet.Config.Compose string` (yaml `compose`); `fleet.Backend.Service` + `fleet.Service.Service` (yaml `service`); validation requiring `compose` when any backend/service exists and `service` per backend/service. `apply.replicaOverride(svc string, backend string, replica, gpu int) string` and `apply.serviceOverride(svc, service string) string` returning compose override YAML with the `dockrail.*` labels + `device_ids`.

- [ ] **Step 1: Write the failing test**

Create `fleet/apply/override_test.go`:

```go
package apply

import (
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/fleet/observe"
)

func TestReplicaOverride(t *testing.T) {
	got := replicaOverride("vllm", "llama-70b", 2, 1)
	// must set container_name, the four dockrail labels, and device_ids.
	for _, want := range []string{
		"vllm:",
		"container_name: llama-70b-2",
		observe.LabelManaged + `: "true"`,
		observe.LabelBackend + ": llama-70b",
		observe.LabelReplica + `: "2"`,
		observe.LabelGPU + `: "1"`,
		`device_ids: ["1"]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("override missing %q:\n%s", want, got)
		}
	}
}

func TestServiceOverride(t *testing.T) {
	got := serviceOverride("chat-api", "chat-api")
	for _, want := range []string{"chat-api:", "container_name: chat-api", observe.LabelService + ": chat-api"} {
		if !strings.Contains(got, want) {
			t.Fatalf("service override missing %q:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `GOCACHE=/tmp/gocache go test ./fleet/apply/ -run 'TestReplicaOverride|TestServiceOverride' -v`
Expected: FAIL — build error, `undefined: replicaOverride`.

- [ ] **Step 3: Implement the override generator**

Create `fleet/apply/override.go`:

```go
// Package apply executes a Planner plan across the fleet via generated
// per-replica compose overrides, health-gated per the fleet serving invariant.
package apply

import (
	"fmt"

	"github.com/goodsmileduck/dockrail/fleet/observe"
)

// replicaOverride returns a compose override (layered over the base compose
// file) that pins one backend replica to a container name, a GPU, and the
// dockrail identity labels the Observer reads.
func replicaOverride(service, backend string, replica, gpu int) string {
	return fmt.Sprintf(`services:
  %s:
    container_name: %s-%d
    labels:
      %s: "true"
      %s: %s
      %s: "%d"
      %s: "%d"
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              device_ids: ["%d"]
              capabilities: [gpu]
`, service, backend, replica,
		observe.LabelManaged,
		observe.LabelBackend, backend,
		observe.LabelReplica, replica,
		observe.LabelGPU, gpu,
		gpu)
}

// serviceOverride stamps a routed service container with its identity label.
func serviceOverride(service, name string) string {
	return fmt.Sprintf(`services:
  %s:
    container_name: %s
    labels:
      %s: "true"
      %s: %s
`, service, name, observe.LabelManaged, observe.LabelService, name)
}
```

- [ ] **Step 4: Run to verify pass**

Run: `GOCACHE=/tmp/gocache go test ./fleet/apply/ -v`
Expected: PASS (both override tests). Then `GOCACHE=/tmp/gocache go vet ./fleet/apply/`.

- [ ] **Step 5: Add config fields + validation**

In `fleet/config.go`: add `Compose string `yaml:"compose"`` to `Config`; add `Service string `yaml:"service"`` to `Backend` and to `Service`. In `validate()`: if `len(c.Backends) > 0 || len(c.Services) > 0` require `c.Compose != ""` (error `"compose is required when backends or services are declared"`); per backend require `b.Service != ""` (`"backends.%s: service is required"`); per service require `s.Service != ""` (`"services.%s: service is required"`).

Add to `fleet/config_test.go` a fixture update: the existing `goodFleet` and any Load-success fixture with backends/services needs `compose: docker-compose.yml` at top level and `service: <name>` on each backend/service. Add one rejection test:

```go
func TestValidate_RequiresComposeAndService(t *testing.T) {
	// backend without service, and no compose -> rejected.
	body := `
project: p
hosts: { a: { ssh: u@h, gpus: [0] } }
backends:
  b: { image_tag: t, placement: { vram_min: 1GiB, gpu: auto, pool: [a] } }
`
	if _, err := Load(writeTemp(t, body)); err == nil {
		t.Fatal("expected rejection: missing compose + service")
	}
}
```

- [ ] **Step 6: Run to verify pass**

Run: `GOCACHE=/tmp/gocache go test ./fleet/ ./fleet/apply/ -v`
Expected: PASS (fleet config tests incl the new one, override tests). Fix any Load-success fixture that now needs `compose`/`service` (add them). Then `GOCACHE=/tmp/gocache go build ./... && GOCACHE=/tmp/gocache go vet ./...`.

- [ ] **Step 7: Commit**

```bash
git add fleet/config.go fleet/config_test.go fleet/apply/override.go fleet/apply/override_test.go
git commit -m "feat(apply): compose/service config + per-replica override generation

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Per-action executor (place / update / remove) via compose + readiness

**Files:**
- Create: `fleet/apply/exec.go`, `fleet/apply/exec_test.go`

**Interfaces:**
- Consumes: `fleet.Config`, `plan.Action`, `connection.Connection`, `strategy/readiness`, `observe` labels, `replicaOverride`/`serviceOverride` (Task 1).
- Produces: `type actionExec struct { cfg *fleet.Config; conn connection.Connection; out io.Writer }`; `func (x *actionExec) place(ctx, a plan.Action) error`; `func (x *actionExec) update(ctx, a plan.Action) error`; `func (x *actionExec) remove(ctx, a plan.Action) error`. Each writes the override to a host temp file and runs `docker compose -f <base> -f <override> ...`, then (place/update) probes the backend's readiness. A failed readiness keeps the container and returns an error naming it.

- [ ] **Step 1: Write the failing test**

Create `fleet/apply/exec_test.go`:

```go
package apply

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
)

func execFixture() (*actionExec, *connection.Fake) {
	f := connection.NewFake()
	cfg := &fleet.Config{
		Compose: "docker-compose.yml",
		Backends: map[string]fleet.Backend{
			"llama": {Service: "vllm", ImageTag: "v2", Replicas: 2,
				Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}},
				Readiness: fleet.Readiness{Type: "tcp", Port: 8000}},
		},
	}
	return &actionExec{cfg: cfg, conn: f, out: &bytes.Buffer{}}, f
}

func TestPlace_WritesOverrideAndComposeUp(t *testing.T) {
	x, f := execFixture()
	f.Stub("nc -z", "", nil) // tcp readiness ok (or however tcp probe is stubbed)
	err := x.place(context.Background(), plan.Action{Kind: plan.PlaceReplica, Backend: "llama", Replica: 0, Host: "h", GPU: 1, Tag: "v2"})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	var sawUp bool
	for _, c := range f.Commands {
		if strings.Contains(c, "docker compose") && strings.Contains(c, "up -d") && strings.Contains(c, "vllm") {
			sawUp = true
		}
	}
	if !sawUp {
		t.Fatalf("expected a compose up for the replica; commands: %v", f.Commands)
	}
}

func TestRemove_ComposeRm(t *testing.T) {
	x, f := execFixture()
	if err := x.remove(context.Background(), plan.Action{Kind: plan.RemoveReplica, Backend: "llama", Replica: 1, Host: "h", GPU: 0}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	var sawRm bool
	for _, c := range f.Commands {
		if strings.Contains(c, "rm -sf") && strings.Contains(c, "llama-1") {
			sawRm = true
		}
	}
	if !sawRm {
		t.Fatalf("expected compose rm of llama-1; commands: %v", f.Commands)
	}
}
```

(Import `plan "github.com/goodsmileduck/dockrail/fleet/plan"` in the test.)

- [ ] **Step 2: Run to verify failure**

Run: `GOCACHE=/tmp/gocache go test ./fleet/apply/ -run 'TestPlace_|TestRemove_' -v`
Expected: FAIL — `undefined: actionExec`.

- [ ] **Step 3: Implement the executor**

Create `fleet/apply/exec.go`. It writes the override to a per-action temp file on the host (`cat > <tmp> <<'EOF' … EOF`), runs `docker compose -f <base> -f <tmp> up -d --no-deps <service>` with `TAG=<tag>`, then probes readiness via `readiness.New`. Use the existing `safeTag` guard. `remove` runs `docker compose -f <base> rm -sf <backend>-<replica>`. Include full code: temp-file write via a heredoc with a unique delimiter, the compose command with the tag env, and the readiness probe. Convert `fleet.Readiness` to the `config.Readiness` the `readiness.New` constructor expects (they share fields; map them). Reference `observe` labels only through Task 1's override generator.

[Full implementation code — the controller supplies it in the dispatch brief; it mirrors `engine/bluegreen.go`'s compose-command + readiness-probe pattern, adapted to write a generated override file rather than rely on user-defined blue/green services.]

- [ ] **Step 4: Run to verify pass**

Run: `GOCACHE=/tmp/gocache go test ./fleet/apply/ -v`
Expected: PASS. Then `GOCACHE=/tmp/gocache go vet ./fleet/apply/`.

- [ ] **Step 5: Commit**

```bash
git add fleet/apply/exec.go fleet/apply/exec_test.go
git commit -m "feat(apply): per-action executor (place/update/remove) via compose + readiness

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Service deploy/update + the Wiring interface (no-op default)

**Files:**
- Create: `fleet/apply/wiring.go`, `fleet/apply/wiring_test.go`
- Modify: `fleet/apply/exec.go`, `fleet/apply/exec_test.go`

**Interfaces:**
- Produces: `type Endpoint struct { Host string; Port int }`; `type Wiring interface { Apply(ctx context.Context, service, backend string, endpoints []Endpoint) error }`; `type LogWiring struct { Out io.Writer }` implementing `Apply` by logging "would wire …" and returning nil (the sub-spec-4 default; real drivers = sub-spec 5). `actionExec` gains `deployService`/`updateService` (compose up of the service with its override + readiness) and a `wiring Wiring` field; a `Rewire` action calls `x.wiring.Apply(...)`.

- [ ] **Steps:** TDD as above — a `TestLogWiring_LogsAndSucceeds` test; a `deployService` test asserting the compose up + label override for the service. Full code in the dispatch brief. Commit `feat(apply): service deploy/update + Wiring interface (log-only default)`.

---

### Task 4: Phase orchestration + `--on-failure` + Result

**Files:**
- Create: `fleet/apply/apply.go`, `fleet/apply/apply_test.go`

**Interfaces:**
- Produces: `func Apply(ctx, cfg *fleet.Config, observed observe.FleetState, exec actionExecutor, opts Options) (Result, error)` where `actionExecutor` is an interface (`place/update/remove/deployService/updateService/rewire`) so the orchestrator is tested with a fake exec that records/【fails】 actions. Runs `plan.Compute`, then executes phases in order; `--on-failure=hold` stops at first failure leaving converged actions (Result.Applied/Failed/Pending); `--on-failure=rollback` reverses applied actions. `Options{OnFailure, Scope, DryRun}`; `Result{Applied, Failed, Pending, Warnings}`.

- [ ] **Steps:** TDD — fake `actionExecutor`; tests for phase order (converge before rewire before drain), hold-leaves-converged, rollback-reverses, scope-filters, Err-host-warning-surfaced, empty-plan no-op. Full code in the dispatch brief. Commit `feat(apply): phase orchestration + on-failure hold/rollback`.

---

### Task 5: Fleet lock + `--scope` + Err-host refusal + history

**Files:**
- Modify: `fleet/apply/apply.go`, `fleet/apply/apply_test.go`
- Possibly modify: `engine/lock.go` (export a fleet-scoped lock helper if the current one is per-project-unexported — note it)

**Interfaces:**
- The `Apply` entry acquires a fleet-scoped lock (reuse `engine` lock; the lock key becomes the fleet `project`) before executing and releases after; `Options.LockWait` polls. `--scope` filters actions to one backend/service (observe stays whole). An action targeting an `Err` host is refused (skipped + warned; or fails fast if a scoped action needs it). Executed actions append to per-host `history.jsonl` (reuse v1 store).

- [ ] **Steps:** TDD — fake connection asserting lock acquire/release around execution; scope filter test; Err-host refusal test; lock-collision fails fast. Full code in dispatch brief. Commit `feat(apply): fleet lock, --scope, Err-host refusal, history`.

---

### Task 6: `dockrail fleet apply` command

**Files:**
- Modify: `cmd/fleet.go`, `cmd/fleet_test.go`

**Interfaces:**
- Produces: a `apply` subcommand under `fleet` with `--on-failure`, `--scope`, `--lock-wait`, `--dry-run`, `--json`; `func runFleetApply(ctx, cfg, factory, out, opts) error` that builds the real executor (`actionExec` with `sshFactory` per host + `LogWiring`) and calls `apply.Apply`, printing the `Result`. `--dry-run` delegates to the plan printer (reuse `runFleetPlan`).

- [ ] **Steps:** TDD — a `runFleetApply` test with a fake connection stubbing an empty-plan (converged) fleet → "already converged" / no mutations; assert the command is registered (`go run . fleet apply --help`). Full suite green. Commit `feat(cmd): fleet apply command`.

---

## Self-Review

**Spec coverage (apply design sections):**
- Sect. 2 launch model (compose + per-replica override) → Task 1. ✓
- Sect. 3 override with labels + device_ids → Task 1. ✓
- Sect. 4 per-action executor + readiness gating → Tasks 2–3. ✓
- Sect. 5 Wiring interface (no-op default) → Task 3. ✓
- Sect. 6 phases + `--on-failure` → Task 4. ✓
- Sect. 7 lock + `--scope` + Err-host + history → Task 5. ✓
- Sect. 9 command → Task 6. ✓
- Sect. 10 testing → tests across tasks (override pure, executor via fake conn, orchestration via fake exec). ✓

**Placeholder note:** Tasks 2–6 mark "[Full implementation code … in the dispatch brief]" for the executor/orchestration bodies rather than inlining every line here — the executor's exact command strings and the orchestration's control flow are substantial and will be written into each task's dispatch brief (with complete code) at execution time, since they depend on final helper signatures from the preceding task. Task 1 (the pure, foundational override generator + config) is fully specified inline. **Before dispatching Tasks 2–6, the controller MUST expand each into complete code in its task brief** — do not dispatch a task whose brief still contains a "[Full … code]" placeholder.

**Type consistency:** `observe.Label*` (schema) used by Task 1's override generator and referenced (not re-spelled) downstream. `replicaOverride`/`serviceOverride` (Task 1) → executor (Task 2). `Wiring`/`Endpoint` (Task 3) → orchestration (Task 4). `actionExecutor` interface (Task 4) lets Task 4 test with a fake and Task 6 wire the real `actionExec`. `Options`/`Result` (Task 4) → command (Task 6).
