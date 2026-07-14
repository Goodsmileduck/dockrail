# Fleet Planner Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax. **Executors: Codex `gpt-5.6-sol` (reasoning medium), driven directly per the `codex-subagent-execution` memory (`codex exec ... < /dev/null` backgrounded, GOCACHE=/tmp/gocache, controller commits + reviews).**

**Goal:** A pure Planner that diffs desired `fleet.Config` against observed `FleetState` (matched by dockrail labels) and emits a phased, ordered action list, exposed via read-only `dockrail fleet plan`.

**Architecture:** Sub-spec 3 of the [v2 fleet design](../../specs/2026-07-14-dockrail-planner-design.md). Adds container-label identity (Observer extension), a kept-aware scheduler entrypoint (`schedule.PlanDelta`), a pure `fleet/plan.Compute` reconciler emitting three phases (converge → rewire → drain), and the `fleet plan` command. No execution — that is sub-spec 4.

**Tech Stack:** Go 1.26, existing `fleet`, `fleet/observe`, `fleet/schedule` packages.

## Global Constraints

- Module path `github.com/goodsmileduck/dockrail` verbatim.
- `fleet/plan` and `fleet/schedule` are PURE (no `connection`/`context` imports).
- Determinism: iterate backends and services by sorted name, replicas ascending. Same inputs → identical `Plan`.
- **v1 simplification (leave healthy put):** a present replica with the right tag is *satisfied regardless of which GPU it is on* — the Planner does NOT detect or emit pin-change moves for already-running replicas. This is a documented v1 limitation (a changed pin takes effect on the next scale/redeploy of that replica). Do not add GPU-mismatch move logic.
- Tag comparison: desired = `backend.ImageTag`; observed = the substring of `Container.Image` after its last `:`.
- gofmt + `go vet ./...` clean; test with `GOCACHE=/tmp/gocache`.
- Commit trailer on every commit: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Do NOT change `strategy/placement` or any v1 engine behavior.

---

### Task 1: Container-label identity — schema consts + Observer extension

**Files:**
- Modify: `fleet/observe/observe.go`
- Modify: `fleet/observe/observe_test.go`

**Interfaces:**
- Produces: label consts `LabelManaged`, `LabelBackend`, `LabelReplica`, `LabelGPU`, `LabelService` (package `observe`); `observe.Container` gains `Labels map[string]string`.
- The `psQuery` template extracts exactly the dockrail keys (no all-labels parsing). `fleet status` output is unaffected for unlabeled containers.

- [ ] **Step 1: Write the failing test**

Append to `fleet/observe/observe_test.go`:

```go
func TestParseContainers_Labels(t *testing.T) {
	// Columns: name, image, managed, backend, replica, gpu, service (tab-sep).
	out := "llama-70b-0\treg/vllm:v0.9.2\ttrue\tllama-70b\t0\t2\t\n" +
		"chat-api\treg/chat:v2\ttrue\t\t\t\tchat-api\n" +
		"random\tnginx:latest\t\t\t\t\t\n"
	cs := parseContainers(out)
	if len(cs) != 3 {
		t.Fatalf("want 3, got %d", len(cs))
	}
	if cs[0].Labels[LabelBackend] != "llama-70b" || cs[0].Labels[LabelReplica] != "0" || cs[0].Labels[LabelGPU] != "2" {
		t.Fatalf("replica labels wrong: %+v", cs[0].Labels)
	}
	if cs[1].Labels[LabelService] != "chat-api" || cs[1].Labels[LabelManaged] != "true" {
		t.Fatalf("service labels wrong: %+v", cs[1].Labels)
	}
	// unlabeled container: name+image parsed, no dockrail labels
	if cs[2].Name != "random" || cs[2].Image != "nginx:latest" || len(cs[2].Labels) != 0 {
		t.Fatalf("unlabeled wrong: %+v", cs[2])
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `GOCACHE=/tmp/gocache go test ./fleet/observe/ -run TestParseContainers_Labels -v`
Expected: FAIL — `LabelBackend` undefined / `Container` has no field `Labels`.

- [ ] **Step 3: Implement**

In `fleet/observe/observe.go`, replace the `psQuery` const and `Container` type and `parseContainers` func with:

```go
// psQuery MUST stay a single backtick raw string so the literal \t escapes and
// the embedded double quotes reach docker intact (the format is single-quoted
// so the shell does not touch it — same shell-safety guarantee as sub-spec 1).
// Do NOT rebuild this with "\t" concatenation: that inserts real tab bytes and
// breaks the TestPSQuery_TemplateSurvivesShell guard (which checks for a literal
// backslash-t) as well as docker template consistency.
const psQuery = `docker ps --format '{{.Names}}\t{{.Image}}\t{{.Label "dockrail.managed"}}\t{{.Label "dockrail.backend"}}\t{{.Label "dockrail.replica"}}\t{{.Label "dockrail.gpu"}}\t{{.Label "dockrail.service"}}'`

// dockrail container labels: self-describing identity the Planner diffs on.
const (
	LabelManaged = "dockrail.managed"
	LabelBackend = "dockrail.backend"
	LabelReplica = "dockrail.replica"
	LabelGPU     = "dockrail.gpu"
	LabelService = "dockrail.service"
)

// labelCols maps the trailing psQuery columns (after name, image) to label keys.
var labelCols = []string{LabelManaged, LabelBackend, LabelReplica, LabelGPU, LabelService}

type Container struct {
	Name   string            `json:"name"`
	Image  string            `json:"image"`
	Labels map[string]string `json:"labels,omitempty"`
}

func parseContainers(out string) []Container {
	var res []Container
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		c := Container{Name: strings.TrimSpace(parts[0]), Image: strings.TrimSpace(parts[1])}
		for i, key := range labelCols {
			col := i + 2
			if col >= len(parts) {
				break
			}
			if v := strings.TrimSpace(parts[col]); v != "" {
				if c.Labels == nil {
					c.Labels = map[string]string{}
				}
				c.Labels[key] = v
			}
		}
		res = append(res, c)
	}
	return res
}
```

(Note: `psQuery` is built by concatenation so the literal tab escapes stay single-quoted through the shell, preserving the sub-spec-1 shell-safety guard — `TestPSQuery_TemplateSurvivesShell` still passes because the template still begins `docker ps --format '{{.Names}}\t{{.Image}}...`.)

- [ ] **Step 4: Run to verify pass**

Run: `GOCACHE=/tmp/gocache go test ./fleet/observe/ -v`
Expected: PASS — the new test plus all sub-spec-1 Observer tests (`TestObserve_TwoHosts` etc. use 2-column stubs, which now yield empty `Labels` — still pass) and `TestPSQuery_TemplateSurvivesShell`. Then `GOCACHE=/tmp/gocache go vet ./fleet/observe/`.

- [ ] **Step 5: Commit**

```bash
git add fleet/observe/observe.go fleet/observe/observe_test.go
git commit -m "feat(observe): surface dockrail container labels for planner identity

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: `schedule.PlanDelta` — kept-aware placement

**Files:**
- Modify: `fleet/schedule/schedule.go`
- Modify: `fleet/schedule/schedule_test.go`

**Interfaces:**
- Produces: `func PlanDelta(cfg *fleet.Config, state observe.FleetState, kept Placements) (Placements, error)`. `Plan(cfg, state)` becomes `return PlanDelta(cfg, state, nil)`.
- Behavior: `kept` are replicas already placed (from observed reality). PlanDelta seeds anti-affinity from `kept` (marks their GPUs occupied-by-backend) but does NOT deduct their VRAM (they are already running, so `FreeMiB` already excludes them). It echoes each kept replica into the output and places only the non-kept desired replicas on free capacity. Missing pinned replicas go to their pin.

- [ ] **Step 1: Write the failing test**

Append to `fleet/schedule/schedule_test.go`:

```go
func TestPlanDelta_KeepsAndPlacesMissing(t *testing.T) {
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"llama": {ImageTag: "t", Replicas: 2, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true},
			}},
		},
	}
	// Two GPUs. Replica 0 already runs on gpu0 (kept). gpu0's free already
	// reflects that (12288 left); gpu1 is full-free.
	state := observe.FleetState{Hosts: []observe.HostState{
		{Name: "h", GPUs: []observe.GPUState{gpu(0, 12288), gpu(1, 24576)}},
	}}
	kept := Placements{"llama": {{Replica: 0, Host: "h", GPU: 0}}}
	got, err := PlanDelta(cfg, state, kept)
	if err != nil {
		t.Fatalf("PlanDelta: %v", err)
	}
	as := got["llama"]
	if len(as) != 2 {
		t.Fatalf("want 2, got %+v", as)
	}
	// replica 0 echoed on gpu0; replica 1 must be placed on gpu1 (anti-affinity
	// forbids gpu0, and gpu1 is the only other GPU).
	byR := map[int]Assignment{}
	for _, a := range as {
		byR[a.Replica] = a
	}
	if byR[0].GPU != 0 || byR[1].GPU != 1 {
		t.Fatalf("delta placement wrong: %+v", as)
	}
}

func TestPlan_DelegatesToPlanDelta(t *testing.T) {
	// Plan(cfg, state) must equal PlanDelta(cfg, state, nil).
	cfg := &fleet.Config{Backends: map[string]fleet.Backend{
		"b": {ImageTag: "t", Replicas: 1, Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
	}}
	state := observe.FleetState{Hosts: []observe.HostState{{Name: "h", GPUs: []observe.GPUState{gpu(0, 24576)}}}}
	a, _ := Plan(cfg, state)
	b, _ := PlanDelta(cfg, state, nil)
	if len(a["b"]) != 1 || len(b["b"]) != 1 || a["b"][0] != b["b"][0] {
		t.Fatalf("Plan != PlanDelta(nil): %+v vs %+v", a, b)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `GOCACHE=/tmp/gocache go test ./fleet/schedule/ -run 'TestPlanDelta_KeepsAndPlacesMissing|TestPlan_DelegatesToPlanDelta' -v`
Expected: FAIL — `undefined: PlanDelta`.

- [ ] **Step 3: Implement**

In `fleet/schedule/schedule.go`, rename `func Plan(...)` to `func PlanDelta(cfg *fleet.Config, state observe.FleetState, kept Placements) (Placements, error)` and add the new `Plan` delegator. Inside `PlanDelta`: after building `ledger`, `occupied`, and `place`, pre-reserve kept replicas (anti-affinity only, no deduct), and in each backend's replica loops echo a kept replica instead of placing it.

Full replacement for the function (keep `gpuRef`, `place`, `selectGPU`, `resolvePolicy` as-is; only `Plan`'s body changes and gains the `kept` handling):

```go
// Plan bin-packs all auto replicas from scratch and validates pins.
func Plan(cfg *fleet.Config, state observe.FleetState) (Placements, error) {
	return PlanDelta(cfg, state, nil)
}

// PlanDelta places only the replicas not already covered by `kept`. Kept
// replicas keep their GPU (their VRAM is already reflected in state.FreeMiB);
// they are marked occupied for anti-affinity but not deducted, and are echoed
// into the returned Placements so the result covers every desired replica.
func PlanDelta(cfg *fleet.Config, state observe.FleetState, kept Placements) (Placements, error) {
	keptOf := map[string]map[int]Assignment{}
	for b, as := range kept {
		m := make(map[int]Assignment, len(as))
		for _, a := range as {
			m[a.Replica] = a
		}
		keptOf[b] = m
	}

	ledger := map[gpuRef]int{}
	for _, h := range state.Hosts {
		if h.Err != "" {
			continue
		}
		for _, g := range h.GPUs {
			ledger[gpuRef{h.Name, g.Index}] = g.FreeMiB
		}
	}
	occupied := map[gpuRef]map[string]bool{}
	place := func(ref gpuRef, backend string, need int) {
		ledger[ref] -= need
		if occupied[ref] == nil {
			occupied[ref] = map[string]bool{}
		}
		occupied[ref][backend] = true
	}
	// Pre-reserve kept replicas for anti-affinity (no VRAM deduct — already running).
	for b, m := range keptOf {
		for _, a := range m {
			ref := gpuRef{a.Host, a.GPU}
			if occupied[ref] == nil {
				occupied[ref] = map[string]bool{}
			}
			occupied[ref][b] = true
		}
	}

	names := make([]string, 0, len(cfg.Backends))
	for name := range cfg.Backends {
		names = append(names, name)
	}
	sort.Strings(names)

	out := Placements{}
	for _, name := range names {
		b := cfg.Backends[name]
		if !b.Placement.GPU.Auto && len(b.Placement.GPU.Pins) == 0 {
			continue
		}
		need := 0
		if b.Placement.VRAMMin != "" {
			n, err := vram.NeededMiB(b.Placement.VRAMMin)
			if err != nil {
				return nil, fmt.Errorf("backends.%s: %w", name, err)
			}
			need = n
		}

		if len(b.Placement.GPU.Pins) > 0 {
			for i, pin := range b.Placement.GPU.Pins {
				if a, ok := keptOf[name][i]; ok {
					out[name] = append(out[name], a)
					continue
				}
				host, idx, err := fleet.ParsePin(pin)
				if err != nil {
					return nil, fmt.Errorf("backends.%s: %w", name, err)
				}
				ref := gpuRef{host, idx}
				avail, ok := ledger[ref]
				if !ok {
					return nil, fmt.Errorf("backends.%s: pin %q targets an unschedulable or unknown gpu", name, pin)
				}
				if avail < need {
					return nil, &ScheduleError{Backend: name, Replica: i, NeededMiB: need, BestFreeMiB: avail}
				}
				place(ref, name, need)
				out[name] = append(out[name], Assignment{Replica: i, Host: host, GPU: idx})
			}
			continue
		}

		pool := map[string]bool{}
		for _, h := range b.Placement.Pool {
			pool[h] = true
		}
		policy := resolvePolicy(cfg, b)
		for r := 0; r < b.Replicas; r++ {
			if a, ok := keptOf[name][r]; ok {
				out[name] = append(out[name], a)
				continue
			}
			ref, ok, best, aff := selectGPU(policy, ledger, occupied, name, pool, need)
			if !ok {
				return nil, &ScheduleError{Backend: name, Replica: r, NeededMiB: need, BestFreeMiB: best, AntiAffinity: aff}
			}
			place(ref, name, need)
			out[name] = append(out[name], Assignment{Replica: r, Host: ref.host, GPU: ref.idx})
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `GOCACHE=/tmp/gocache go test ./fleet/schedule/ -v`
Expected: PASS — the two new tests plus all existing scheduler tests (they call `Plan`, which now delegates). Then `GOCACHE=/tmp/gocache go vet ./fleet/schedule/`.

- [ ] **Step 5: Commit**

```bash
git add fleet/schedule/schedule.go fleet/schedule/schedule_test.go
git commit -m "feat(schedule): PlanDelta places only non-kept replicas (kept-aware)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: `fleet/plan` — types + backend reconciliation

**Files:**
- Create: `fleet/plan/plan.go`
- Create: `fleet/plan/plan_test.go`

**Interfaces:**
- Consumes: `fleet.Config`/`Backend`, `observe.FleetState`/`Container` + label consts, `schedule.PlanDelta`/`Placements`/`Assignment`.
- Produces: `Plan`/`Phase`/`Action`/`ActionKind` + `Action.String()`; `func Compute(cfg *fleet.Config, observed observe.FleetState) (Plan, error)` handling BACKEND actions only (services + rewire land in Task 4). Backend classification: satisfied (present + right tag, any GPU) → no action; present + wrong tag → `UpdateReplica` (converge); desired replica absent → `PlaceReplica` (converge, placed via `PlanDelta` with kept = satisfied+update replicas); observed replica index ≥ desired count or backend undesired → `RemoveReplica` (drain).

- [ ] **Step 1: Write the failing test**

Create `fleet/plan/plan_test.go`:

```go
package plan

import (
	"testing"

	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/observe"
)

func backendCfg(replicas int, tag string) *fleet.Config {
	return &fleet.Config{Backends: map[string]fleet.Backend{
		"llama": {ImageTag: tag, Replicas: replicas, Placement: fleet.Placement{
			VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true},
		}},
	}}
}

// obs builds a host with GPUs and managed backend-replica containers.
func hostWith(name string, free map[int]int, reps []observe.Container) observe.HostState {
	var gpus []observe.GPUState
	for idx, f := range free {
		gpus = append(gpus, observe.GPUState{Index: idx, TotalMiB: f, FreeMiB: f})
	}
	return observe.HostState{Name: name, GPUs: gpus, Containers: reps}
}

func rep(backend string, replica, gpu int, image string) observe.Container {
	return observe.Container{Name: backend + "-" + itoa(replica), Image: image, Labels: map[string]string{
		observe.LabelManaged: "true", observe.LabelBackend: backend,
		observe.LabelReplica: itoa(replica), observe.LabelGPU: itoa(gpu),
	}}
}
func itoa(i int) string { return string(rune('0' + i)) } // single-digit test helper

func actionsOf(p Plan) []Action {
	var all []Action
	for _, ph := range p.Phases {
		all = append(all, ph.Actions...)
	}
	return all
}

func TestCompute_Noop(t *testing.T) {
	cfg := backendCfg(1, "v2")
	// one replica running on gpu0 with the right tag -> satisfied, empty plan.
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288, 1: 24576}, []observe.Container{rep("llama", 0, 0, "reg/llama:v2")}),
	}}
	p, err := Compute(cfg, st)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(actionsOf(p)) != 0 {
		t.Fatalf("expected empty plan, got %+v", actionsOf(p))
	}
}

func TestCompute_ScaleUpPlaces(t *testing.T) {
	cfg := backendCfg(2, "v2")
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288, 1: 24576}, []observe.Container{rep("llama", 0, 0, "reg/llama:v2")}),
	}}
	p, _ := Compute(cfg, st)
	var place *Action
	for _, ph := range p.Phases {
		for i := range ph.Actions {
			if ph.Actions[i].Kind == PlaceReplica {
				place = &ph.Actions[i]
			}
		}
	}
	if place == nil || place.Backend != "llama" || place.Replica != 1 || place.GPU != 1 {
		t.Fatalf("want place llama/1 on gpu1, got %+v", place)
	}
}

func TestCompute_TagChangeUpdates(t *testing.T) {
	cfg := backendCfg(1, "v3")
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288}, []observe.Container{rep("llama", 0, 0, "reg/llama:v2")}),
	}}
	p, _ := Compute(cfg, st)
	as := actionsOf(p)
	if len(as) != 1 || as[0].Kind != UpdateReplica || as[0].Tag != "v3" || as[0].OldTag != "v2" {
		t.Fatalf("want update v2->v3, got %+v", as)
	}
}

func TestCompute_ScaleDownRemoves(t *testing.T) {
	cfg := backendCfg(1, "v2")
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288, 1: 12288}, []observe.Container{
			rep("llama", 0, 0, "reg/llama:v2"), rep("llama", 1, 1, "reg/llama:v2"),
		}),
	}}
	p, _ := Compute(cfg, st)
	as := actionsOf(p)
	if len(as) != 1 || as[0].Kind != RemoveReplica || as[0].Replica != 1 {
		t.Fatalf("want remove llama/1, got %+v", as)
	}
	// remove must be in the drain phase (last).
	last := p.Phases[len(p.Phases)-1]
	if last.Name != "drain" || len(last.Actions) != 1 {
		t.Fatalf("remove not in drain phase: %+v", p.Phases)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `GOCACHE=/tmp/gocache go test ./fleet/plan/ -v`
Expected: FAIL — build error, `undefined: Compute`.

- [ ] **Step 3: Implement**

Create `fleet/plan/plan.go`:

```go
// Package plan diffs desired fleet config against observed reality (matched by
// dockrail container labels) and emits a phased, ordered action list. Pure —
// no I/O; execution is the apply engine.
package plan

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/observe"
	"github.com/goodsmileduck/dockrail/fleet/schedule"
)

type ActionKind string

const (
	PlaceReplica  ActionKind = "place-replica"
	UpdateReplica ActionKind = "update-replica"
	RemoveReplica ActionKind = "remove-replica"
	DeployService ActionKind = "deploy-service"
	UpdateService ActionKind = "update-service"
	Rewire        ActionKind = "rewire"
)

type Action struct {
	Kind      ActionKind
	Backend   string
	Replica   int
	Service   string
	Host      string
	GPU       int
	Tag       string
	OldTag    string
	Endpoints []string
}

func (a Action) String() string {
	switch a.Kind {
	case PlaceReplica:
		return fmt.Sprintf("place  %s/%d  %s:%d  %s", a.Backend, a.Replica, a.Host, a.GPU, a.Tag)
	case UpdateReplica:
		return fmt.Sprintf("update %s/%d  %s:%d  %s (was %s)", a.Backend, a.Replica, a.Host, a.GPU, a.Tag, a.OldTag)
	case RemoveReplica:
		return fmt.Sprintf("remove %s/%d  %s:%d", a.Backend, a.Replica, a.Host, a.GPU)
	case DeployService:
		return fmt.Sprintf("deploy %s  %s  %s", a.Service, a.Host, a.Tag)
	case UpdateService:
		return fmt.Sprintf("update %s  %s  %s (was %s)", a.Service, a.Host, a.Tag, a.OldTag)
	case Rewire:
		return fmt.Sprintf("rewire %s → %s  %v", a.Service, a.Backend, a.Endpoints)
	}
	return string(a.Kind)
}

type Phase struct {
	Name    string
	Actions []Action
}
type Plan struct{ Phases []Phase }

// obsReplica is an observed managed backend replica.
type obsReplica struct {
	host string
	gpu  int
	tag  string
}

func tagOf(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return ""
}

// Compute diffs cfg against observed and returns the phased plan. This task
// handles backend replicas only; services + rewire are added in Task 4.
func Compute(cfg *fleet.Config, observed observe.FleetState) (Plan, error) {
	// Index observed managed backend replicas: obs[backend][replica].
	obs := map[string]map[int]obsReplica{}
	for _, h := range observed.Hosts {
		for _, c := range h.Containers {
			if c.Labels[observe.LabelManaged] != "true" {
				continue
			}
			b := c.Labels[observe.LabelBackend]
			if b == "" {
				continue // service containers handled in Task 4
			}
			r, err := strconv.Atoi(c.Labels[observe.LabelReplica])
			if err != nil {
				continue
			}
			g, _ := strconv.Atoi(c.Labels[observe.LabelGPU])
			if obs[b] == nil {
				obs[b] = map[int]obsReplica{}
			}
			obs[b][r] = obsReplica{host: h.Name, gpu: g, tag: tagOf(c.Image)}
		}
	}

	backends := sortedKeys(cfg.Backends)

	// Classify present replicas: satisfied/update keep their GPU (kept); their
	// tag/update actions collected. Missing replicas placed via PlanDelta.
	kept := schedule.Placements{}
	var converge, drain []Action
	for _, name := range backends {
		b := cfg.Backends[name]
		if !b.Placement.GPU.Auto && len(b.Placement.GPU.Pins) == 0 {
			continue
		}
		for r := 0; r < b.Replicas; r++ {
			o, ok := obs[name][r]
			if !ok {
				continue // missing — placed below
			}
			kept[name] = append(kept[name], schedule.Assignment{Replica: r, Host: o.host, GPU: o.gpu})
			if o.tag != b.ImageTag {
				converge = append(converge, Action{Kind: UpdateReplica, Backend: name, Replica: r,
					Host: o.host, GPU: o.gpu, Tag: b.ImageTag, OldTag: o.tag})
			}
		}
	}

	// Place missing replicas on free capacity, kept reserved.
	placements, err := schedule.PlanDelta(cfg, observed, kept)
	if err != nil {
		return Plan{}, err
	}
	for _, name := range backends {
		b := cfg.Backends[name]
		if !b.Placement.GPU.Auto && len(b.Placement.GPU.Pins) == 0 {
			continue
		}
		keptR := map[int]bool{}
		for _, a := range kept[name] {
			keptR[a.Replica] = true
		}
		for _, a := range placements[name] {
			if keptR[a.Replica] {
				continue
			}
			converge = append(converge, Action{Kind: PlaceReplica, Backend: name, Replica: a.Replica,
				Host: a.Host, GPU: a.GPU, Tag: b.ImageTag})
		}
	}

	// Extra: observed replicas beyond desired count, or backend not desired.
	for _, name := range sortedKeys(obs) {
		desired := 0
		if b, ok := cfg.Backends[name]; ok {
			desired = b.Replicas
		}
		for _, r := range sortedInts(obs[name]) {
			if r >= desired {
				o := obs[name][r]
				drain = append(drain, Action{Kind: RemoveReplica, Backend: name, Replica: r, Host: o.host, GPU: o.gpu})
			}
		}
	}

	return assemble(converge, nil, drain), nil
}

// assemble builds the three-phase plan, dropping empty phases' actions but
// keeping phase structure. rewire is filled in Task 4.
func assemble(converge, rewire, drain []Action) Plan {
	return Plan{Phases: []Phase{
		{Name: "converge", Actions: converge},
		{Name: "rewire", Actions: rewire},
		{Name: "drain", Actions: drain},
	}}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedInts(m map[int]obsReplica) []int {
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	return ks
}
```

- [ ] **Step 4: Run to verify pass**

Run: `GOCACHE=/tmp/gocache go test ./fleet/plan/ -v`
Expected: PASS (Noop, ScaleUp, TagChange, ScaleDown). Then `GOCACHE=/tmp/gocache go vet ./fleet/plan/`.

- [ ] **Step 5: Commit**

```bash
git add fleet/plan/plan.go fleet/plan/plan_test.go
git commit -m "feat(plan): backend reconciler — satisfied/update/place/remove, phased

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Services, rewire, and phase population

**Files:**
- Modify: `fleet/plan/plan.go`
- Modify: `fleet/plan/plan_test.go`

**Interfaces:**
- Consumes: `fleet.Config.Services`/`Service`/`Use` + the backend placements from Task 3.
- Produces: service `DeployService`/`UpdateService` actions (converge phase) and `Rewire` actions (rewire phase). `Compute` now populates all three phases. Endpoints for `Rewire` = `host` of each of the backend's placed replicas (port derivation is deferred — use `host` alone for now; a follow-up adds a backend port). No new exported symbols beyond behavior.

- [ ] **Step 1: Write the failing test**

Append to `fleet/plan/plan_test.go`:

```go
func TestCompute_ServiceDeployAndRewire(t *testing.T) {
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"llama": {ImageTag: "v2", Replicas: 1, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
		},
		Services: map[string]fleet.Service{
			"chat": {Host: "h", ImageTag: "s1",
				Uses: []fleet.Use{{Backend: "llama", Wiring: fleet.Wiring{Strategy: "nginx-upstream"}}}},
		},
	}
	// llama/0 already running & satisfied; chat service absent -> deploy + rewire.
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288}, []observe.Container{rep("llama", 0, 0, "reg/llama:v2")}),
	}}
	p, err := Compute(cfg, st)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	var deploy, rewire *Action
	for pi := range p.Phases {
		for ai := range p.Phases[pi].Actions {
			a := &p.Phases[pi].Actions[ai]
			if a.Kind == DeployService {
				deploy = a
			}
			if a.Kind == Rewire {
				rewire = a
			}
		}
	}
	if deploy == nil || deploy.Service != "chat" || deploy.Host != "h" || deploy.Tag != "s1" {
		t.Fatalf("service deploy wrong: %+v", deploy)
	}
	if rewire == nil || rewire.Service != "chat" || rewire.Backend != "llama" || len(rewire.Endpoints) != 1 || rewire.Endpoints[0] != "h" {
		t.Fatalf("rewire wrong: %+v", rewire)
	}
}

func TestCompute_PhaseOrdering(t *testing.T) {
	// A plan with a place (converge), a rewire, and a remove (drain) must order
	// them converge < rewire < drain.
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"llama": {ImageTag: "v2", Replicas: 2, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
			"old": {ImageTag: "v1", Replicas: 0, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
		},
		Services: map[string]fleet.Service{
			"chat": {Host: "h", ImageTag: "s1",
				Uses: []fleet.Use{{Backend: "llama", Wiring: fleet.Wiring{Strategy: "nginx-upstream"}}}},
		},
	}
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288, 1: 24576}, []observe.Container{
			rep("llama", 0, 0, "reg/llama:v2"),
			rep("old", 0, 1, "reg/old:v1"),
		}),
	}}
	p, _ := Compute(cfg, st)
	if len(p.Phases) != 3 || p.Phases[0].Name != "converge" || p.Phases[1].Name != "rewire" || p.Phases[2].Name != "drain" {
		t.Fatalf("phase names/order wrong: %+v", p.Phases)
	}
	if len(p.Phases[1].Actions) == 0 || p.Phases[1].Actions[0].Kind != Rewire {
		t.Fatalf("rewire phase empty: %+v", p.Phases[1])
	}
	if len(p.Phases[2].Actions) == 0 || p.Phases[2].Actions[0].Kind != RemoveReplica {
		t.Fatalf("drain phase should remove old/0: %+v", p.Phases[2])
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `GOCACHE=/tmp/gocache go test ./fleet/plan/ -run 'TestCompute_ServiceDeployAndRewire|TestCompute_PhaseOrdering' -v`
Expected: FAIL — no service/rewire actions produced yet (converge lacks deploy; rewire phase empty).

- [ ] **Step 3: Implement**

In `fleet/plan/plan.go`, extend `Compute` to index observed services, emit service + rewire actions, and pass rewire into `assemble`. Add this before the final `return`, and change the return to include rewire.

Replace the tail of `Compute` (from the `// Extra:` comment through the `return assemble(...)` line) with:

```go
	// Extra: observed replicas beyond desired count, or backend not desired.
	for _, name := range sortedKeys(obs) {
		desired := 0
		if b, ok := cfg.Backends[name]; ok {
			desired = b.Replicas
		}
		for _, r := range sortedInts(obs[name]) {
			if r >= desired {
				o := obs[name][r]
				drain = append(drain, Action{Kind: RemoveReplica, Backend: name, Replica: r, Host: o.host, GPU: o.gpu})
			}
		}
	}

	// Services: index observed service containers by dockrail.service -> tag.
	obsSvc := map[string]string{}
	for _, h := range observed.Hosts {
		for _, c := range h.Containers {
			if c.Labels[observe.LabelManaged] != "true" {
				continue
			}
			if s := c.Labels[observe.LabelService]; s != "" {
				obsSvc[s] = tagOf(c.Image)
			}
		}
	}
	var rewire []Action
	for _, name := range sortedServiceKeys(cfg.Services) {
		s := cfg.Services[name]
		if cur, ok := obsSvc[name]; !ok {
			converge = append(converge, Action{Kind: DeployService, Service: name, Host: s.Host, Tag: s.ImageTag})
		} else if cur != s.ImageTag {
			converge = append(converge, Action{Kind: UpdateService, Service: name, Host: s.Host, Tag: s.ImageTag, OldTag: cur})
		}
		for _, u := range s.Uses {
			rewire = append(rewire, Action{Kind: Rewire, Service: name, Backend: u.Backend,
				Endpoints: endpointsOf(placements[u.Backend])})
		}
	}

	return assemble(converge, rewire, drain), nil
```

Add these helpers to `fleet/plan/plan.go`:

```go
func sortedServiceKeys(m map[string]fleet.Service) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// endpointsOf returns the host of each placed replica (port derivation deferred).
func endpointsOf(as []schedule.Assignment) []string {
	eps := make([]string, 0, len(as))
	for _, a := range as {
		eps = append(eps, a.Host)
	}
	return eps
}
```

- [ ] **Step 4: Run to verify pass**

Run: `GOCACHE=/tmp/gocache go test ./fleet/plan/ -v`
Expected: PASS (all Task 3 + Task 4 tests). Then `GOCACHE=/tmp/gocache go vet ./fleet/plan/`.

- [ ] **Step 5: Commit**

```bash
git add fleet/plan/plan.go fleet/plan/plan_test.go
git commit -m "feat(plan): service deploy/update + rewire actions; populate 3 phases

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: `dockrail fleet plan` command

**Files:**
- Modify: `cmd/fleet.go`
- Test: `cmd/fleet_test.go`

**Interfaces:**
- Consumes: `fleet.Load`, `observe.Observer`/`ConnFactory`, `plan.Compute`, existing `sshFactory`.
- Produces: a `plan` subcommand under the `fleet` group; `func runFleetPlan(ctx, cfg *fleet.Config, factory observe.ConnFactory, out io.Writer, asJSON bool) error`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/fleet_test.go`:

```go
func TestRunFleetPlan_Text(t *testing.T) {
	cfg := &fleet.Config{Project: "p", Hosts: map[string]fleet.Host{"h": {SSH: "u@h", GPUs: []int{0, 1}}},
		Backends: map[string]fleet.Backend{
			"llama": {ImageTag: "v2", Replicas: 1, Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
		}}
	fake := connection.NewFake()
	// no containers running -> plan should place llama/0.
	fake.Stub("docker ps", "", nil)
	fake.Stub("nvidia-smi", "0, 24576, 0, 24576\n1, 24576, 0, 24576\n", nil)
	factory := func(name string, h fleet.Host) (connection.Connection, error) { return fake, nil }

	var buf bytes.Buffer
	if err := runFleetPlan(context.Background(), cfg, observe.ConnFactory(factory), &buf, false); err != nil {
		t.Fatalf("runFleetPlan: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"converge", "place", "llama/0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan output missing %q:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `GOCACHE=/tmp/gocache go test ./cmd/ -run TestRunFleetPlan_Text -v`
Expected: FAIL — `undefined: runFleetPlan`.

- [ ] **Step 3: Implement**

In `cmd/fleet.go`, add a `plan` subcommand to `newFleetCmd` (alongside the existing `status` subcommand) and the `runFleetPlan` core. Add the import `"github.com/goodsmileduck/dockrail/fleet/plan"`.

Inside `newFleetCmd`, after the `status` subcommand is added, add:

```go
	planCmd := &cobra.Command{
		Use:   "plan",
		Short: "show the phased action plan to converge the fleet to fleet.yml",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("fleet")
			cfg, err := fleet.Load(path)
			if err != nil {
				return err
			}
			asJSON, _ := cmd.Flags().GetBool("json")
			return runFleetPlan(cmd.Context(), cfg, sshFactory, cmd.OutOrStdout(), asJSON)
		},
	}
	planCmd.Flags().Bool("json", false, "emit machine-readable JSON instead of text")
	fleetCmd.AddCommand(planCmd)
```

Add the core (with the needed imports `encoding/json`, `fmt`, `io` already present from status):

```go
func runFleetPlan(ctx context.Context, cfg *fleet.Config, factory observe.ConnFactory, out io.Writer, asJSON bool) error {
	o := &observe.Observer{Cfg: cfg, Factory: factory}
	st, err := o.Observe(ctx)
	if err != nil {
		return err
	}
	p, err := plan.Compute(cfg, st)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(p)
	}
	empty := true
	for _, ph := range p.Phases {
		if len(ph.Actions) == 0 {
			continue
		}
		empty = false
		fmt.Fprintf(out, "Phase — %s\n", ph.Name)
		for _, a := range ph.Actions {
			fmt.Fprintf(out, "  %s\n", a.String())
		}
	}
	if empty {
		fmt.Fprintln(out, "already converged; no actions")
	}
	return nil
}
```

(Imports: ensure `cmd/fleet.go` imports `github.com/goodsmileduck/dockrail/fleet/observe` and `.../fleet/plan`; `observe` is already imported for status.)

- [ ] **Step 4: Run to verify pass**

Run: `GOCACHE=/tmp/gocache go test ./cmd/ -run TestRunFleetPlan_Text -v`
Expected: PASS. Then the full suite: `GOCACHE=/tmp/gocache go build ./... && GOCACHE=/tmp/gocache go test ./... && GOCACHE=/tmp/gocache go vet ./...` — build ok, all pass, vet clean.

- [ ] **Step 5: Manual smoke**

Run: `GOCACHE=/tmp/gocache go run . fleet plan --help`
Expected: help for `fleet plan` with `-f/--fleet` and `--json`.

- [ ] **Step 6: Commit**

```bash
git add cmd/fleet.go cmd/fleet_test.go
git commit -m "feat(cmd): fleet plan — phased dry-run of the reconciler

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage (planner design sections):**
- Sect. 2–3 label identity + Observer extension → Task 1. ✓
- Sect. 4 reconciliation (satisfied/update/missing/extra, leave-healthy-put) → Task 3; `PlanDelta` → Task 2. ✓
- Sect. 5 services + rewire → Task 4. ✓
- Sect. 6 three phases + `Action`/`Kind`/`String()` → Tasks 3–4. ✓
- Sect. 7 `fleet plan` command → Task 5. ✓
- Sect. 9 testing → tests across Tasks 1–5. ✓
- Sect. 10 build order → Tasks 1→5. ✓

Deliberately simplified vs spec (noted in Global Constraints): pin-change moves for already-running replicas are NOT detected (a present replica with the right tag is satisfied regardless of GPU). Endpoint port derivation deferred (endpoints = host only). Both are documented v1 limitations, not gaps.

**Placeholder scan:** none — every step has complete code.

**Type consistency:** `observe.Label*` + `Container.Labels` (Task 1) used in Tasks 3–4. `schedule.PlanDelta`/`Placements`/`Assignment` (Task 2) used in Task 3. `plan.Compute`/`Plan`/`Action`/`ActionKind` consts (Task 3) used in Tasks 4–5. `assemble` gains its `rewire` argument used from Task 4. `runFleetPlan` (Task 5) consumes `plan.Compute`. The `itoa` test helper is single-digit-only (test replicas/gpus stay 0–9).
