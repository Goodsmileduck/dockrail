# Fleet Apply Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax. **Executors: Codex `gpt-5.6-sol` (reasoning medium), driven directly per the `codex-subagent-execution` memory (`codex exec ... < /dev/null` backgrounded, GOCACHE=/tmp/gocache, controller commits + reviews).**

**Goal:** `dockrail fleet apply` — execute the Planner's phased plan across the fleet via generated per-replica compose overrides, health-gated, with `--on-failure`, fleet lock, `--scope`, and Err-host refusal.

**Architecture:** Sub-spec 4 of the [v2 fleet design](../../specs/2026-07-14-dockrail-apply-design.md). New `fleet/apply` package: pure override generation, a per-action executor against a `connection.Connection`, phase orchestration, and the `Wiring` interface (no-op default; real drivers = sub-spec 5). Reuses `strategy/readiness` and (sub-spec 5) the v1 lock. Backends/services become compose services (D7 preserved).

**Tech Stack:** Go 1.26, existing `fleet`, `fleet/plan`, `fleet/observe`, `connection`, `config`, `strategy/readiness` packages.

## Global Constraints

- Module path `github.com/goodsmileduck/dockrail` verbatim.
- **Per-replica launch model (verified):** each replica is its own generated compose service `<backend>-<replica>` that `extends` the template service, in an override file written beside the base compose. `docker compose -f <override> up -d <backend>-<replica>` launches just that replica (extends pulls the base). Do NOT set `container_name` on the shared template service — compose operates on services, so N replicas must be N distinct services.
- Override files are written **beside the base compose** (same directory) so the override's relative `extends: {file: <base>}` resolves identically to how the user's `compose:` path resolves.
- The `dockrail.*` labels stamped on launch MUST use the `observe.Label*` consts (`LabelManaged/Backend/Replica/GPU/Service`), not re-spelled strings.
- Image tags are interpolated into shell/compose; guard with `safeTag = ^[A-Za-z0-9][A-Za-z0-9._-]*$` and reject unsafe tags before use.
- Override generation is PURE. The executor's only I/O is through `connection.Connection`; the orchestrator is tested against a fake `actionExecutor`.
- Phases execute converge → rewire → drain; converge/rewire actions are health-gated; a failed converge action keeps its container for forensics.
- gofmt + `go vet ./...` clean; test with `GOCACHE=/tmp/gocache`.
- Commit trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- **Simplification (documented):** services (routed APIs) are deployed via the same override + compose-up + readiness mechanism as backends (a brief per-service blip on redeploy). Zero-downtime *service* cutover (v1's proxy blue-green) is NOT reused here — it stays a v1 single-host feature; fleet service zero-downtime is a later refinement. Backends are already rolling (one replica at a time, N-1 keep serving).

---

### Task 1: Config `compose`/`service` fields + per-replica override generation

**Files:**
- Modify: `fleet/config.go`, `fleet/config_test.go`
- Create: `fleet/apply/override.go`, `fleet/apply/override_test.go`

**Interfaces:**
- Produces: `fleet.Config.Compose string` (yaml `compose`); `fleet.Backend.Service` + `fleet.Service.Service` (yaml `service`); validation. `apply.replicaOverride(base, template, backend string, replica, gpu int) string` and `apply.serviceOverride(base, template, service string) string` returning override YAML (extends + labels + device_ids).

- [ ] **Step 1: Write the failing test** — create `fleet/apply/override_test.go`:

```go
package apply

import (
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/fleet/observe"
)

func TestReplicaOverride(t *testing.T) {
	got := replicaOverride("docker-compose.yml", "vllm", "llama-70b", 2, 1)
	for _, want := range []string{
		"llama-70b-2:",
		"file: docker-compose.yml",
		"service: vllm",
		"container_name: llama-70b-2",
		observe.LabelManaged + `: "true"`,
		observe.LabelBackend + ": llama-70b",
		observe.LabelReplica + `: "2"`,
		observe.LabelGPU + `: "1"`,
		`device_ids: ["1"]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("replica override missing %q:\n%s", want, got)
		}
	}
}

func TestServiceOverride(t *testing.T) {
	got := serviceOverride("docker-compose.yml", "chat-api", "chat-api")
	for _, want := range []string{"chat-api:", "service: chat-api", "container_name: chat-api", observe.LabelService + ": chat-api"} {
		if !strings.Contains(got, want) {
			t.Fatalf("service override missing %q:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure** — `GOCACHE=/tmp/gocache go test ./fleet/apply/ -run Override -v` → FAIL (`undefined: replicaOverride`).

- [ ] **Step 3: Implement** — create `fleet/apply/override.go`:

```go
// Package apply executes a Planner plan across the fleet via generated
// per-replica compose overrides, health-gated per the fleet serving invariant.
package apply

import (
	"fmt"

	"github.com/goodsmileduck/dockrail/fleet/observe"
)

// replicaOverride returns a compose override defining the replica as its own
// service <backend>-<replica> that extends the template service, pinned to a
// GPU and stamped with the dockrail identity labels the Observer reads. It is
// a distinct service (not a container_name on the shared template) because
// docker compose operates on services.
func replicaOverride(base, template, backend string, replica, gpu int) string {
	name := fmt.Sprintf("%s-%d", backend, replica)
	return fmt.Sprintf(`services:
  %s:
    extends:
      file: %s
      service: %s
    container_name: %s
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
`, name, base, template, name,
		observe.LabelManaged,
		observe.LabelBackend, backend,
		observe.LabelReplica, replica,
		observe.LabelGPU, gpu,
		gpu)
}

// serviceOverride returns an override for a routed service: its own service
// extending the template, stamped with the dockrail.service label.
func serviceOverride(base, template, service string) string {
	return fmt.Sprintf(`services:
  %s:
    extends:
      file: %s
      service: %s
    container_name: %s
    labels:
      %s: "true"
      %s: %s
`, service, base, template, service,
		observe.LabelManaged, observe.LabelService, service)
}
```

- [ ] **Step 4: Run to verify pass** — `GOCACHE=/tmp/gocache go test ./fleet/apply/ -v` → PASS; then `go vet ./fleet/apply/`.

- [ ] **Step 5: Config fields + validation** — in `fleet/config.go` add `Compose string `yaml:"compose"`` to `Config`, `Service string `yaml:"service"`` to `Backend` and `Service`. In `validate()`:

```go
	if len(c.Backends) > 0 || len(c.Services) > 0 {
		if c.Compose == "" {
			return fmt.Errorf("compose is required when backends or services are declared")
		}
	}
```

and inside the backend loop `if b.Service == "" { return fmt.Errorf("backends.%s: service is required", name) }`, and inside the service loop `if s.Service == "" { return fmt.Errorf("services.%s: service is required", name) }`.

- [ ] **Step 6: Fix fixtures + add rejection test** — in `fleet/config_test.go`, add `compose: docker-compose.yml` (top level) and `service: <name>` to every backend/service in Load-SUCCESS fixtures (`goodFleet`, `TestLoad_SchedulerPolicy`, `TestValidate_ReplicasDefaultsToOne`, `TestLoad_PinnedReplicasDefaultToPinCount`, any other that expects success). Add:

```go
func TestValidate_RequiresComposeAndService(t *testing.T) {
	body := `
project: p
hosts: { a: { ssh: u@h, gpus: [0] } }
backends:
  b: { image_tag: t, service: vllm, placement: { vram_min: 1GiB, gpu: auto, pool: [a] } }
`
	// compose missing at top level -> rejected.
	if _, err := Load(writeTemp(t, body)); err == nil {
		t.Fatal("expected rejection: missing compose")
	}
}
```

- [ ] **Step 7: Run** — `GOCACHE=/tmp/gocache go test ./fleet/ ./fleet/apply/ && go build ./... && go vet ./...` → all pass, clean. (Fix any remaining success-fixture that now needs `compose`/`service`.)

- [ ] **Step 8: Commit** —
```bash
git add fleet/config.go fleet/config_test.go fleet/apply/override.go fleet/apply/override_test.go
git commit -m "feat(apply): compose/service config + extends-based per-replica override

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Per-action executor (place / update / remove)

**Files:** Create `fleet/apply/exec.go`, `fleet/apply/exec_test.go`.

**Interfaces:** `type actionExec struct { cfg *fleet.Config; conn connection.Connection; out io.Writer; wiring Wiring }` (Wiring added in Task 3; declare the field now with an interface defined in Task 3 — to keep Task 2 self-contained, define a minimal `Wiring` interface stub here and let Task 3 flesh the default). Methods `place`, `update`, `remove` on `plan.Action`.

- [ ] **Step 1: Failing test** — `fleet/apply/exec_test.go`:

```go
package apply

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
	plan "github.com/goodsmileduck/dockrail/fleet/plan"
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
	err := x.place(context.Background(), plan.Action{Kind: plan.PlaceReplica, Backend: "llama", Replica: 0, Host: "h", GPU: 1, Tag: "v2"})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	var sawWrite, sawUp bool
	for _, c := range f.Commands {
		if strings.Contains(c, "DOCKRAILEOF") && strings.Contains(c, "llama-0") {
			sawWrite = true
		}
		if strings.Contains(c, "docker compose") && strings.Contains(c, "up -d") && strings.Contains(c, "llama-0") {
			sawUp = true
		}
	}
	if !sawWrite || !sawUp {
		t.Fatalf("want override write + compose up for llama-0; commands: %v", f.Commands)
	}
}

func TestPlace_RejectsUnsafeTag(t *testing.T) {
	x, _ := execFixture()
	if err := x.place(context.Background(), plan.Action{Kind: plan.PlaceReplica, Backend: "llama", Replica: 0, Host: "h", GPU: 1, Tag: "v2; rm -rf /"}); err == nil {
		t.Fatal("expected unsafe-tag rejection")
	}
}

func TestRemove_DockerRm(t *testing.T) {
	x, f := execFixture()
	if err := x.remove(context.Background(), plan.Action{Kind: plan.RemoveReplica, Backend: "llama", Replica: 1, Host: "h", GPU: 0}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	var sawRm bool
	for _, c := range f.Commands {
		if strings.Contains(c, "rm -f llama-1") || strings.Contains(c, "rm -sf llama-1") {
			sawRm = true
		}
	}
	if !sawRm {
		t.Fatalf("want removal of llama-1; commands: %v", f.Commands)
	}
}
```

(The `tcp` readiness probe against a `connection.Fake` returns empty stdout / nil error for its stubbed command — confirm the tcp prober treats that as success in this harness; if it needs a stub, add `f.Stub(...)` matching the tcp probe's command. Adjust after seeing the RED run.)

- [ ] **Step 2: Run to verify failure** — `GOCACHE=/tmp/gocache go test ./fleet/apply/ -run 'TestPlace_|TestRemove_' -v` → FAIL (`undefined: actionExec`).

- [ ] **Step 3: Implement** — create `fleet/apply/exec.go`:

```go
package apply

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
	plan "github.com/goodsmileduck/dockrail/fleet/plan"
	"github.com/goodsmileduck/dockrail/strategy/readiness"
)

// safeTag restricts image tags interpolated into shell/compose commands.
var safeTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type actionExec struct {
	cfg    *fleet.Config
	conn   connection.Connection
	out    io.Writer
	wiring Wiring
}

func (x *actionExec) logf(format string, a ...any) {
	if x.out != nil {
		fmt.Fprintf(x.out, format+"\n", a...)
	}
}

// overridePath is the host path for a generated override, a sibling of the base
// compose so its relative `extends: {file: <base>}` resolves.
func (x *actionExec) overridePath(name string) string {
	dir := filepath.Dir(x.cfg.Compose)
	return filepath.Join(dir, ".dockrail-"+name+".override.yml")
}

// baseName is the compose path as referenced from the override's directory
// (same dir), i.e. its basename.
func (x *actionExec) baseName() string { return filepath.Base(x.cfg.Compose) }

func (x *actionExec) writeFile(ctx context.Context, path, content string) error {
	cmd := fmt.Sprintf("cat > %s <<'DOCKRAILEOF'\n%s\nDOCKRAILEOF\n", path, content)
	_, err := x.conn.Run(ctx, cmd)
	return err
}

func (x *actionExec) place(ctx context.Context, a plan.Action) error {
	b, ok := x.cfg.Backends[a.Backend]
	if !ok {
		return fmt.Errorf("place: unknown backend %q", a.Backend)
	}
	if !safeTag.MatchString(a.Tag) {
		return fmt.Errorf("place %s/%d: unsafe image tag %q", a.Backend, a.Replica, a.Tag)
	}
	name := fmt.Sprintf("%s-%d", a.Backend, a.Replica)
	ov := x.overridePath(name)
	if err := x.writeFile(ctx, ov, replicaOverride(x.baseName(), b.Service, a.Backend, a.Replica, a.GPU)); err != nil {
		return fmt.Errorf("place %s: write override: %w", name, err)
	}
	x.logf("place %s on %s:%d (%s)", name, a.Host, a.GPU, a.Tag)
	up := fmt.Sprintf("TAG=%s docker compose -f %s up -d --no-deps %s", a.Tag, ov, name)
	if _, err := x.conn.Run(ctx, up); err != nil {
		return fmt.Errorf("place %s: compose up: %w", name, err)
	}
	return x.probe(ctx, b, name)
}

// update recreates the replica with the new tag. compose up -d recreates the
// container when its image changed; the backend's other replicas keep serving.
func (x *actionExec) update(ctx context.Context, a plan.Action) error { return x.place(ctx, a) }

func (x *actionExec) remove(ctx context.Context, a plan.Action) error {
	name := fmt.Sprintf("%s-%d", a.Backend, a.Replica)
	x.logf("remove %s on %s:%d", name, a.Host, a.GPU)
	// Graceful: stop then remove the single container by its name.
	if _, err := x.conn.Run(ctx, fmt.Sprintf("docker rm -f %s", name)); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

func (x *actionExec) probe(ctx context.Context, b fleet.Backend, who string) error {
	prober, err := readiness.New(config.Readiness{
		Type: b.Readiness.Type, Path: b.Readiness.Path, Port: b.Readiness.Port, Timeout: b.Readiness.Timeout,
	}, b.Model)
	if err != nil {
		return fmt.Errorf("%s: readiness config: %w", who, err)
	}
	if err := prober.Probe(ctx, x.conn); err != nil {
		return fmt.Errorf("%s: readiness failed (container kept for inspection): %w", who, err)
	}
	return nil
}
```

- [ ] **Step 4: Run to verify pass** — `GOCACHE=/tmp/gocache go test ./fleet/apply/ -v`. If the tcp probe needs a stub to succeed against the fake, add it in `execFixture`. Then `go vet ./fleet/apply/`.

- [ ] **Step 5: Commit** — `feat(apply): per-action executor (place/update/remove) via compose + readiness`.

---

### Task 3: Wiring interface (no-op default) + service deploy/update

**Files:** Create `fleet/apply/wiring.go`, `fleet/apply/wiring_test.go`; modify `fleet/apply/exec.go`, `fleet/apply/exec_test.go`.

**Interfaces:** `type Endpoint struct { Host string; Port int }`; `type Wiring interface { Apply(ctx context.Context, service, backend string, endpoints []Endpoint) error }`; `type LogWiring struct { Out io.Writer }` (default: logs, returns nil). `actionExec.deployService`/`updateService` (override + compose up + readiness for the service's compose service). `actionExec.rewire(ctx, a plan.Action)` calls `x.wiring.Apply` (converting `a.Endpoints []string` hosts to `[]Endpoint` with a zero/derived port — port derivation is sub-spec 5).

- [ ] **Step 1: Failing test** — `fleet/apply/wiring_test.go`:

```go
package apply

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestLogWiring_LogsAndSucceeds(t *testing.T) {
	var buf bytes.Buffer
	w := LogWiring{Out: &buf}
	if err := w.Apply(context.Background(), "chat", "llama", []Endpoint{{Host: "h", Port: 8000}}); err != nil {
		t.Fatalf("LogWiring.Apply: %v", err)
	}
	if !strings.Contains(buf.String(), "chat") || !strings.Contains(buf.String(), "llama") {
		t.Fatalf("expected a wiring log line, got %q", buf.String())
	}
}
```

- [ ] **Step 2: Run to verify failure** — `GOCACHE=/tmp/gocache go test ./fleet/apply/ -run TestLogWiring -v` → FAIL.

- [ ] **Step 3: Implement** — create `fleet/apply/wiring.go`:

```go
package apply

import (
	"context"
	"fmt"
	"io"
)

type Endpoint struct {
	Host string
	Port int
}

// Wiring points a service at a backend's current healthy endpoints. Real
// drivers (nginx-upstream, env-list) land in sub-spec 5.
type Wiring interface {
	Apply(ctx context.Context, service, backend string, endpoints []Endpoint) error
}

// LogWiring is the sub-spec-4 default: it logs the intended wiring and succeeds,
// so apply is end-to-end runnable before the real drivers exist.
type LogWiring struct{ Out io.Writer }

func (w LogWiring) Apply(_ context.Context, service, backend string, endpoints []Endpoint) error {
	if w.Out != nil {
		fmt.Fprintf(w.Out, "wire %s -> %s %v (no-op: real driver in sub-spec 5)\n", service, backend, endpoints)
	}
	return nil
}
```

Add to `fleet/apply/exec.go`: `deployService`/`updateService` (mirror `place` using `serviceOverride` + the service's `Readiness`) and `rewire`:

```go
func (x *actionExec) deployService(ctx context.Context, a plan.Action) error {
	s, ok := x.cfg.Services[a.Service]
	if !ok {
		return fmt.Errorf("deploy: unknown service %q", a.Service)
	}
	if a.Tag != "" && !safeTag.MatchString(a.Tag) {
		return fmt.Errorf("deploy %s: unsafe tag %q", a.Service, a.Tag)
	}
	ov := x.overridePath(a.Service)
	if err := x.writeFile(ctx, ov, serviceOverride(x.baseName(), s.Service, a.Service)); err != nil {
		return fmt.Errorf("deploy %s: write override: %w", a.Service, err)
	}
	x.logf("deploy service %s on %s (%s)", a.Service, a.Host, a.Tag)
	up := fmt.Sprintf("TAG=%s docker compose -f %s up -d --no-deps %s", a.Tag, ov, a.Service)
	if _, err := x.conn.Run(ctx, up); err != nil {
		return fmt.Errorf("deploy %s: compose up: %w", a.Service, err)
	}
	prober, err := readiness.New(config.Readiness{
		Type: s.Readiness.Type, Path: s.Readiness.Path, Port: s.Readiness.Port, Timeout: s.Readiness.Timeout,
	}, "")
	if err != nil {
		return fmt.Errorf("%s: readiness config: %w", a.Service, err)
	}
	return prober.Probe(ctx, x.conn)
}

func (x *actionExec) updateService(ctx context.Context, a plan.Action) error { return x.deployService(ctx, a) }

func (x *actionExec) rewire(ctx context.Context, a plan.Action) error {
	eps := make([]Endpoint, 0, len(a.Endpoints))
	for _, h := range a.Endpoints {
		eps = append(eps, Endpoint{Host: h}) // port derivation = sub-spec 5
	}
	if x.wiring == nil {
		x.wiring = LogWiring{Out: x.out}
	}
	return x.wiring.Apply(ctx, a.Service, a.Backend, eps)
}
```

- [ ] **Step 4: Run to verify pass** — `GOCACHE=/tmp/gocache go test ./fleet/apply/ -v`; `go vet ./fleet/apply/`.

- [ ] **Step 5: Commit** — `feat(apply): Wiring interface (log-only default) + service deploy/update`.

---

### Task 4: Phase orchestration + `--on-failure` + Result

**Files:** Create `fleet/apply/apply.go`, `fleet/apply/apply_test.go`.

**Interfaces:**
- `type actionExecutor interface { place(context.Context, plan.Action) error; update(...) error; remove(...) error; deployService(...) error; updateService(...) error; rewire(...) error }` (satisfied by `*actionExec`; the orchestrator is tested with a fake).
- `type Options struct { OnFailure string; Scope string }`; `type Result struct { Applied []plan.Action; Failed *plan.Action; Pending []plan.Action; Warnings []string }`.
- `func run(ctx context.Context, p plan.Plan, x actionExecutor, opts Options) (Result, error)` — executes phases in order, dispatching each action by `Kind`; on `--on-failure=hold` stops at first error (Failed set, remaining → Pending); on `rollback` reverses `Applied` (best-effort). `--scope` filters actions whose `Backend`/`Service` != scope.

- [ ] **Step 1: Failing test** — `fleet/apply/apply_test.go`:

```go
package apply

import (
	"context"
	"errors"
	"testing"

	plan "github.com/goodsmileduck/dockrail/fleet/plan"
)

// fakeExec records dispatched actions and can fail a chosen kind.
type fakeExec struct {
	done   []plan.Action
	failOn plan.ActionKind
}

func (f *fakeExec) do(a plan.Action) error {
	if a.Kind == f.failOn {
		return errors.New("boom")
	}
	f.done = append(f.done, a)
	return nil
}
func (f *fakeExec) place(_ context.Context, a plan.Action) error         { return f.do(a) }
func (f *fakeExec) update(_ context.Context, a plan.Action) error        { return f.do(a) }
func (f *fakeExec) remove(_ context.Context, a plan.Action) error        { return f.do(a) }
func (f *fakeExec) deployService(_ context.Context, a plan.Action) error { return f.do(a) }
func (f *fakeExec) updateService(_ context.Context, a plan.Action) error { return f.do(a) }
func (f *fakeExec) rewire(_ context.Context, a plan.Action) error        { return f.do(a) }

func demoPlan() plan.Plan {
	return plan.Plan{Phases: []plan.Phase{
		{Name: "converge", Actions: []plan.Action{{Kind: plan.PlaceReplica, Backend: "llama", Replica: 1}}},
		{Name: "rewire", Actions: []plan.Action{{Kind: plan.Rewire, Service: "chat", Backend: "llama"}}},
		{Name: "drain", Actions: []plan.Action{{Kind: plan.RemoveReplica, Backend: "old", Replica: 0}}},
	}}
}

func TestRun_PhaseOrder(t *testing.T) {
	f := &fakeExec{}
	res, err := run(context.Background(), demoPlan(), f, Options{OnFailure: "hold"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Applied) != 3 || f.done[0].Kind != plan.PlaceReplica || f.done[1].Kind != plan.Rewire || f.done[2].Kind != plan.RemoveReplica {
		t.Fatalf("wrong order/applied: %+v", f.done)
	}
}

func TestRun_HoldStopsAndReportsPending(t *testing.T) {
	f := &fakeExec{failOn: plan.Rewire}
	res, _ := run(context.Background(), demoPlan(), f, Options{OnFailure: "hold"})
	if res.Failed == nil || res.Failed.Kind != plan.Rewire {
		t.Fatalf("want Failed=rewire, got %+v", res.Failed)
	}
	if len(res.Applied) != 1 || len(res.Pending) != 1 || res.Pending[0].Kind != plan.RemoveReplica {
		t.Fatalf("want 1 applied (place) + 1 pending (remove): applied=%+v pending=%+v", res.Applied, res.Pending)
	}
}

func TestRun_ScopeFilters(t *testing.T) {
	f := &fakeExec{}
	res, _ := run(context.Background(), demoPlan(), f, Options{OnFailure: "hold", Scope: "llama"})
	// only actions touching backend/service "llama" execute (place + rewire); the
	// "old" remove is filtered out.
	for _, a := range res.Applied {
		if a.Backend == "old" {
			t.Fatalf("scope should exclude old: %+v", res.Applied)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure** — `GOCACHE=/tmp/gocache go test ./fleet/apply/ -run TestRun_ -v` → FAIL (`undefined: run`).

- [ ] **Step 3: Implement** — create `fleet/apply/apply.go`:

```go
package apply

import (
	"context"
	"fmt"

	plan "github.com/goodsmileduck/dockrail/fleet/plan"
)

type actionExecutor interface {
	place(context.Context, plan.Action) error
	update(context.Context, plan.Action) error
	remove(context.Context, plan.Action) error
	deployService(context.Context, plan.Action) error
	updateService(context.Context, plan.Action) error
	rewire(context.Context, plan.Action) error
}

type Options struct {
	OnFailure string // "hold" (default) | "rollback"
	Scope     string // "" = whole fleet; else a backend/service name
}

type Result struct {
	Applied  []plan.Action
	Failed   *plan.Action
	Pending  []plan.Action
	Warnings []string
}

func dispatch(ctx context.Context, x actionExecutor, a plan.Action) error {
	switch a.Kind {
	case plan.PlaceReplica:
		return x.place(ctx, a)
	case plan.UpdateReplica:
		return x.update(ctx, a)
	case plan.RemoveReplica:
		return x.remove(ctx, a)
	case plan.DeployService:
		return x.deployService(ctx, a)
	case plan.UpdateService:
		return x.updateService(ctx, a)
	case plan.Rewire:
		return x.rewire(ctx, a)
	}
	return fmt.Errorf("unknown action kind %q", a.Kind)
}

func inScope(a plan.Action, scope string) bool {
	if scope == "" {
		return true
	}
	return a.Backend == scope || a.Service == scope
}

// run executes the plan's phases in order. On failure with OnFailure=="rollback"
// it reverses the applied actions (best effort); otherwise (hold) it stops,
// recording the failed action and the not-yet-run actions as Pending.
func run(ctx context.Context, p plan.Plan, x actionExecutor, opts Options) (Result, error) {
	res := Result{Warnings: p.Warnings}
	// Flatten phase-ordered, scope-filtered actions.
	var actions []plan.Action
	for _, ph := range p.Phases {
		for _, a := range ph.Actions {
			if inScope(a, opts.Scope) {
				actions = append(actions, a)
			}
		}
	}
	for i, a := range actions {
		if err := dispatch(ctx, x, a); err != nil {
			fa := a
			res.Failed = &fa
			res.Pending = append(res.Pending, actions[i+1:]...)
			if opts.OnFailure == "rollback" {
				rollback(ctx, x, res.Applied)
			}
			return res, fmt.Errorf("apply: action %s failed: %w", a.Kind, err)
		}
		res.Applied = append(res.Applied, a)
	}
	return res, nil
}

// rollback reverses applied actions in reverse order (best effort): a placed
// replica is removed; a removed one cannot be reliably restored, so it is
// logged as unrecoverable via the executor's own logging on failure.
func rollback(ctx context.Context, x actionExecutor, applied []plan.Action) {
	for i := len(applied) - 1; i >= 0; i-- {
		a := applied[i]
		switch a.Kind {
		case plan.PlaceReplica, plan.UpdateReplica:
			_ = x.remove(ctx, a)
		}
	}
}
```

- [ ] **Step 4: Run to verify pass** — `GOCACHE=/tmp/gocache go test ./fleet/apply/ -v`; `go vet ./fleet/apply/`.

- [ ] **Step 5: Commit** — `feat(apply): phase orchestration + on-failure hold/rollback + scope`.

---

### Task 5: `Apply` entrypoint — observe → plan → run, Err-host refusal

**Files:** Modify `fleet/apply/apply.go`, `fleet/apply/apply_test.go`.

**Interfaces:** `func Apply(ctx context.Context, cfg *fleet.Config, observed observe.FleetState, x actionExecutor, opts Options) (Result, error)` — runs `plan.Compute(cfg, observed)`, then `run(...)`. Err-host actions are already excluded by the Planner (it leaves blocked backends unplanned + warns); `Apply` propagates `plan.Warnings` into `Result` and does not execute actions on Err hosts (guaranteed by the Planner, asserted here). Fleet lock + `--scope` from the command layer.

- [ ] **Steps:** TDD — `TestApply_EmptyPlanNoop` (a fully-satisfied `FleetState` → `plan.Compute` empty → `run` does nothing, `Result.Applied` empty); `TestApply_SurfacesWarnings` (an Err host → `Result.Warnings` non-empty, no action on that host). Implement `Apply` calling `plan.Compute` then `run`. Full code:

```go
func Apply(ctx context.Context, cfg *fleet.Config, observed observe.FleetState, x actionExecutor, opts Options) (Result, error) {
	p, err := plan.Compute(cfg, observed)
	if err != nil {
		return Result{}, err
	}
	return run(ctx, p, x, opts)
}
```

(Import `observe`.) Commit `feat(apply): Apply entrypoint (observe->plan->run) + warnings surfaced`.

Note for the controller: the **fleet lock** promotion (`engine/lock.go` is per-project) is wired at the command layer in Task 6, reusing the existing lock keyed on the fleet `project`; if the lock helpers are unexported, export a thin `engine.FleetLock`/`engine.FleetUnlock` (note it in the Task 6 brief). `--scope` is already handled by `run`.

---

### Task 6: `dockrail fleet apply` command (+ fleet lock)

**Files:** Modify `cmd/fleet.go`, `cmd/fleet_test.go`.

**Interfaces:** an `apply` subcommand under `fleet` with `--on-failure`, `--scope`, `--lock-wait`, `--dry-run`, `--json`; `func runFleetApply(ctx, cfg *fleet.Config, factory observe.ConnFactory, out io.Writer, opts apply.Options, dryRun, asJSON bool) error` that: observes (`observeFleet`); if `dryRun` prints the plan (reuse `runFleetPlan`); else builds a per-host `actionExec` (each action's `Host` → `sshFactory` connection, `LogWiring{out}`) wrapped in a small dispatcher `actionExecutor` that routes each action to the exec for its host, calls `apply.Apply`, and prints `Result` (text + `--json`).

- [ ] **Steps:** TDD — `TestRunFleetApply_EmptyPlan` (fake connection: no containers but a converged desired → actually assert "already converged"/no `docker rm`/`up` issued OR that Applied is empty; use a fleet where desired == observed). Assert `go run . fleet apply --help` registers the flags. Full suite green. Implement the per-host executor router:

```go
// hostRouter dispatches each action to an actionExec bound to the action's host.
type hostRouter struct {
	cfg     *fleet.Config
	factory observe.ConnFactory
	out     io.Writer
	execs   map[string]*apply.ActionExecForHost // built lazily per host
}
```

(Expose what Task 6 needs from `apply`: either export `actionExec` as `apply.Exec` with a constructor `apply.NewExec(cfg, conn, out, wiring)`, or add an `apply.ForHost` helper. Decide in the Task 6 brief; simplest is to export `NewExec` and have the command build one exec per distinct host from `sshFactory`, then a router implementing `actionExecutor` that looks up the exec by `a.Host`.) Commit `feat(cmd): fleet apply command + fleet lock`.

---

## Self-Review

**Spec coverage:** launch model + override (Task 1) ✓; per-action executor + readiness gating (Tasks 2–3) ✓; Wiring no-op (Task 3) ✓; phases + on-failure + scope (Task 4) ✓; observe→plan→run + Err-host via Planner (Task 5) ✓; command + lock (Task 6) ✓; testing across tasks (override pure, executor via fake conn, orchestration via fake exec) ✓.

**Placeholder scan:** Tasks 1–5 contain complete code. Task 6 leaves ONE deliberate open decision (how the command exposes/constructs the per-host executor — export `NewExec` vs a `ForHost` helper) to resolve in its brief, because it depends on whether the fleet lock needs an `engine` export; the command's flags, `runFleetApply` shape, and test are specified. The controller MUST finalize that one constructor choice in the Task 6 dispatch brief before dispatching.

**Type consistency:** `observe.Label*` used by the override generator (Task 1) and not re-spelled downstream. `replicaOverride`/`serviceOverride` (Task 1) → executor (Task 2/3). `Wiring`/`Endpoint`/`LogWiring` (Task 3) referenced by the `actionExec.wiring` field declared in Task 2 (declare the field's type as the Task-3 `Wiring` interface — Task 2 and Task 3 land in the same package `apply`, so define `Wiring` in Task 3 and have Task 2's struct field use it; if Task 2 compiles before Task 3, add a minimal `type Wiring interface{ Apply(context.Context, string, string, []Endpoint) error }` + `Endpoint` in Task 2 and let Task 3 own the `LogWiring` default — note this ordering in the Task 2 brief). `actionExecutor` interface (Task 4) satisfied by `*actionExec` (Tasks 2–3) and the fake. `Options`/`Result` (Task 4) → `Apply` (Task 5) → command (Task 6).
