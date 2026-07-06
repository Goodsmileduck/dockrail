# dockrail GPU-Aware Blue-Green Cutover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the stubbed `cutover.strategy: proxy` and `placement.type: gpu` so dockrail can do zero-downtime blue-green cutover for vLLM services on a shared GPU, honoring VRAM capacity and rolling back automatically when a VRAM-forced sequenced cutover fails.

**Architecture:** Three pieces off the existing config. (1) A GPU **placement probe** parses `nvidia-smi` and picks a free GPU (`free >= vram_min × 1.2`) from the configured pool, or reports none free. (2) A **proxy cutover** runs blue and green as distinct compose services (`<svc>-blue` / `<svc>-green`), brings up the inactive color, health-gates it with the readiness prober, then flips an nginx upstream fragment and reloads. (3) The engine **orchestrates** two paths: free-slot (start green alongside blue → flip → stop blue; zero gap) and `stop-old-first` (stop blue to free VRAM → start green → readiness → flip; gap, with auto-rollback to blue on failure). Color is derived from what is running, so no state-schema change.

**Tech Stack:** Go 1.26, existing `engine` / `config` / `connection` / `strategy/placement` / `strategy/readiness` packages, stdlib `errors`/`strconv`/`strings`. Depends on the `vllm` readiness probe (separate plan) for correct "model served" gating. No new dependencies.

## Global Constraints

- Module `github.com/goodsmileduck/dockrail`; Go `1.26.0` / toolchain `go1.26.4`. Local: `GOTOOLCHAIN=auto`.
- All host interaction goes through `connection.Connection.Run` so the `Fake` records commands and dry-run/MCP preview works.
- **Resolved design decisions (from the design spec, do not re-litigate):**
  1. Free GPU slot ⇔ `memory.free >= vram_min × 1.2`.
  2. dockrail assigns the GPU by exporting `DOCKRAIL_GPU=<index>`; the compose file maps it to the green container's device reservation.
  3. nginx flip = rewrite an upstream fragment to `server <svc>-<color>:<port>;` then reload nginx; blue and green are distinct compose services `<svc>-blue` / `<svc>-green`.
  4. `cutover.warmup` is a parsed no-op in v1.
  5. On a `stop-old-first` cutover whose green fails readiness, dockrail auto-restarts blue's previous tag and records the failure; the `fail` branch changes nothing and stays manual.
- `vram_min` is parsed as a human size (e.g. `"18GiB"`, `"18000MiB"`); compare against `nvidia-smi` free MiB.
- Blue-green only applies when `cutover.strategy == "proxy"`. `recreate` behavior is unchanged.
- The nginx upstream fragment path convention is `$HOME/.dockrail/<project>/nginx/<service>.conf`; the operator's nginx config `include`s this directory. `cutover.proxy` holds the nginx container name to `docker exec … nginx -s reload`.

---

### Task 1: Parse `vram_min` sizes

**Files:**
- Create: `strategy/placement/vram.go`
- Test: `strategy/placement/vram_test.go`

**Interfaces:**
- Produces: `func parseMiB(s string) (int, error)` — converts `"18GiB"`, `"18Gi"`, `"18000MiB"`, `"512Mi"`, bare `"18000"` (MiB) to integer MiB. Consumed by the GPU placer in Task 2.

- [ ] **Step 1: Write the failing test**

```go
package placement

import "testing"

func TestParseMiB(t *testing.T) {
	cases := map[string]int{
		"18GiB":    18432,
		"18Gi":     18432,
		"18000MiB": 18000,
		"512Mi":    512,
		"18000":    18000, // bare = MiB
	}
	for in, want := range cases {
		got, err := parseMiB(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("%s: got %d want %d", in, got, want)
		}
	}
	if _, err := parseMiB("garbage"); err == nil {
		t.Error("want error on garbage")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./strategy/placement/... -run TestParseMiB -v`
Expected: FAIL — `parseMiB` undefined.

- [ ] **Step 3: Implement `strategy/placement/vram.go`**

```go
package placement

import (
	"fmt"
	"strconv"
	"strings"
)

// parseMiB converts a VRAM size string to integer mebibytes. Accepts GiB/Gi,
// MiB/Mi, and a bare number (treated as MiB, matching nvidia-smi's unit).
func parseMiB(s string) (int, error) {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	mult := 1
	num := lower
	switch {
	case strings.HasSuffix(lower, "gib"):
		num, mult = strings.TrimSuffix(lower, "gib"), 1024
	case strings.HasSuffix(lower, "gi"):
		num, mult = strings.TrimSuffix(lower, "gi"), 1024
	case strings.HasSuffix(lower, "mib"):
		num, mult = strings.TrimSuffix(lower, "mib"), 1
	case strings.HasSuffix(lower, "mi"):
		num, mult = strings.TrimSuffix(lower, "mi"), 1
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid vram size %q: %w", s, err)
	}
	return int(v * float64(mult)), nil
}
```

- [ ] **Step 4: Run the test**

Run: `GOTOOLCHAIN=auto go test ./strategy/placement/... -run TestParseMiB -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add strategy/placement/vram.go strategy/placement/vram_test.go
git commit -m "feat: parse vram_min size strings to MiB"
```

---

### Task 2: GPU placement probe (nvidia-smi → free-slot pick)

**Files:**
- Create: `strategy/placement/gpu.go`
- Modify: `strategy/placement/placement.go` (dispatch `case "gpu"`; export `ErrNoFreeGPU`)
- Test: `strategy/placement/gpu_test.go`

**Interfaces:**
- Consumes: `config.Placement{Pool, VRAMMin}`, `connection.Connection`, `parseMiB`.
- Produces: `var ErrNoFreeGPU = errors.New(...)`; `GPU` implementing `Placer.Pick(ctx, conn) (string, error)` — returns the chosen GPU index (as a string, e.g. `"1"`) when a pool GPU has `free >= vram_min × 1.2`, else returns `("", ErrNoFreeGPU)`. Real errors (nvidia-smi failure, parse failure) return a non-sentinel error.

- [ ] **Step 1: Write the failing test**

```go
package placement

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

const smiHeader = "nvidia-smi --query-gpu=index,memory.free"

func TestGPUPicksFreeSlot(t *testing.T) {
	p, err := newGPU(config.Placement{Type: "gpu", Pool: []int{0, 1}, VRAMMin: "18GiB"})
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake()
	// GPU0 nearly full, GPU1 has 40GiB free -> pick "1".
	f.Stub(smiHeader, "0, 2000\n1, 40960\n", nil)
	got, err := p.Pick(context.Background(), f)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got != "1" {
		t.Fatalf("want GPU 1, got %q", got)
	}
}

func TestGPUNoFreeSlotSentinel(t *testing.T) {
	p, _ := newGPU(config.Placement{Type: "gpu", Pool: []int{0}, VRAMMin: "18GiB"})
	f := connection.NewFake()
	// 18GiB*1.2 = 22118 MiB required; 20000 free is not enough.
	f.Stub(smiHeader, "0, 20000\n", nil)
	_, err := p.Pick(context.Background(), f)
	if !errors.Is(err, ErrNoFreeGPU) {
		t.Fatalf("want ErrNoFreeGPU, got %v", err)
	}
}

func TestGPUIgnoresOutOfPool(t *testing.T) {
	p, _ := newGPU(config.Placement{Type: "gpu", Pool: []int{0}, VRAMMin: "1GiB"})
	f := connection.NewFake()
	// GPU3 is free but not in pool; GPU0 is full -> no free slot.
	f.Stub(smiHeader, "0, 100\n3, 80000\n", nil)
	_, err := p.Pick(context.Background(), f)
	if !errors.Is(err, ErrNoFreeGPU) {
		t.Fatalf("want ErrNoFreeGPU (pool-restricted), got %v", err)
	}
	_ = strings.TrimSpace
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./strategy/placement/... -run TestGPU -v`
Expected: FAIL — `newGPU` / `ErrNoFreeGPU` undefined.

- [ ] **Step 3: Implement `strategy/placement/gpu.go`**

```go
package placement

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

// ErrNoFreeGPU signals that no GPU in the pool has enough free VRAM for a
// second model copy. The engine interprets it via on_no_free_gpu.
var ErrNoFreeGPU = errors.New("no free GPU with sufficient VRAM")

const vramSafetyFactor = 1.2 // reserve 20% for KV-cache growth under load

type GPU struct {
	pool     map[int]bool
	needMiB  int
}

func newGPU(p config.Placement) (*GPU, error) {
	need, err := parseMiB(p.VRAMMin)
	if err != nil {
		return nil, err
	}
	pool := make(map[int]bool, len(p.Pool))
	for _, idx := range p.Pool {
		pool[idx] = true
	}
	return &GPU{pool: pool, needMiB: int(float64(need) * vramSafetyFactor)}, nil
}

// Pick returns the index of a pool GPU with enough free VRAM, or ErrNoFreeGPU.
func (g *GPU) Pick(ctx context.Context, conn connection.Connection) (string, error) {
	out, err := conn.Run(ctx,
		"nvidia-smi --query-gpu=index,memory.free --format=csv,noheader,nounits")
	if err != nil {
		return "", fmt.Errorf("nvidia-smi: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			return "", fmt.Errorf("unexpected nvidia-smi line %q", line)
		}
		idx, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return "", fmt.Errorf("bad gpu index in %q: %w", line, err)
		}
		freeMiB, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return "", fmt.Errorf("bad free mem in %q: %w", line, err)
		}
		if g.pool[idx] && freeMiB >= g.needMiB {
			return strconv.Itoa(idx), nil
		}
	}
	return "", ErrNoFreeGPU
}
```

- [ ] **Step 4: Dispatch in `placement.go`**

```go
	case "gpu":
		return newGPU(p)
```

Keep the existing `None` for `""`/`none`.

- [ ] **Step 5: Run the tests**

Run: `GOTOOLCHAIN=auto go test ./strategy/placement/... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add strategy/placement/
git commit -m "feat: gpu placement probe with vram-aware free-slot pick"
```

---

### Task 3: nginx upstream flip helper

**Files:**
- Create: `engine/nginx.go`
- Test: `engine/nginx_test.go`

**Interfaces:**
- Consumes: `connection.Connection`, project name, nginx container name (`cutover.proxy`), service name, active color, serving port.
- Produces: `func flipUpstream(ctx, conn, project, nginxContainer, service, color string, port int) error` — writes `$HOME/.dockrail/<project>/nginx/<service>.conf` containing `upstream <service> { server <service>-<color>:<port>; }` and runs `docker exec <nginxContainer> nginx -s reload`.

- [ ] **Step 1: Write the failing test**

```go
package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

func TestFlipUpstreamWritesAndReloads(t *testing.T) {
	f := connection.NewFake()
	if err := flipUpstream(context.Background(), f, "demo", "nginx", "web", "green", 8000); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.Commands, "\n")
	if !strings.Contains(all, ".dockrail/demo/nginx/web.conf") {
		t.Fatalf("must write per-service upstream fragment:\n%s", all)
	}
	if !strings.Contains(all, "server web-green:8000;") {
		t.Fatalf("upstream must target the active color:\n%s", all)
	}
	if !strings.Contains(all, "docker exec nginx nginx -s reload") {
		t.Fatalf("must reload nginx:\n%s", all)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./engine/... -run TestFlipUpstream -v`
Expected: FAIL — `flipUpstream` undefined.

- [ ] **Step 3: Implement `engine/nginx.go`**

```go
package engine

import (
	"context"
	"fmt"

	"github.com/goodsmileduck/dockrail/connection"
)

// flipUpstream points the nginx upstream for a service at the given color's
// container and reloads nginx. The operator's nginx config must `include` the
// $HOME/.dockrail/<project>/nginx/ directory. Reload is issued inside the
// nginx container named by cutover.proxy.
func flipUpstream(ctx context.Context, conn connection.Connection, project, nginxContainer, service, color string, port int) error {
	dir := fmt.Sprintf("$HOME/.dockrail/%s/nginx", project)
	path := fmt.Sprintf("%s/%s.conf", dir, service)
	frag := fmt.Sprintf("upstream %s { server %s-%s:%d; }\n", service, service, color, port)
	write := fmt.Sprintf("mkdir -p %s && cat > %s <<'DDEOF'\n%sDDEOF", dir, path, frag)
	if _, err := conn.Run(ctx, write); err != nil {
		return fmt.Errorf("write upstream fragment: %w", err)
	}
	if _, err := conn.Run(ctx, fmt.Sprintf("docker exec %s nginx -s reload", nginxContainer)); err != nil {
		return fmt.Errorf("nginx reload: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run the test**

Run: `GOTOOLCHAIN=auto go test ./engine/... -run TestFlipUpstream -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add engine/nginx.go engine/nginx_test.go
git commit -m "feat: nginx upstream flip helper"
```

---

### Task 4: Blue-green cutover orchestration

**Files:**
- Create: `engine/bluegreen.go`
- Modify: `engine/engine.go` (route `proxy` strategy in `recreate`'s caller; see Step 5)
- Test: `engine/bluegreen_test.go`

**Interfaces:**
- Consumes: `placement.New`/`Pick`/`ErrNoFreeGPU`, `readiness.New`, `flipUpstream`, `config.Service`, `connection.Connection`.
- Produces: `func (e *Engine) proxyCutover(ctx, name string, svc config.Service, tag string) error`. Decides free-slot vs sequenced from the placement probe and `on_no_free_gpu`, performs the flip, and auto-rolls-back blue on a failed sequenced cutover.

**Color derivation:** `activeColor` = whichever of `<svc>-blue` / `<svc>-green` currently has a container (`docker compose ps -q`). Target color = the other; default to `blue` when neither is running (first deploy).

- [ ] **Step 1: Write the failing tests**

```go
package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func bgService() config.Service {
	return config.Service{
		ImageTag:  "v2",
		Readiness: config.Readiness{Type: "http", Path: "/health", Port: 8000, Timeout: "1s"},
		Cutover:   config.Cutover{Strategy: "proxy", Proxy: "nginx"},
		Placement: config.Placement{Type: "gpu", Pool: []int{0, 1}, VRAMMin: "18GiB", OnNoFreeGPU: "stop-old-first"},
	}
}

// Free slot -> green starts, flip, blue stops. Blue never stopped before flip.
func TestProxyCutoverFreeSlotIsZeroGap(t *testing.T) {
	f := connection.NewFake()
	f.Stub("query-gpu", "0, 2000\n1, 40960\n", nil)   // GPU1 free
	f.Stub("ps -q web-blue", "cid-blue\n", nil)        // blue active
	e := &Engine{Conn: f, Cfg: bgCfg(), Out: discard()}
	if err := e.proxyCutover(context.Background(), "web", bgService(), "v2"); err != nil {
		t.Fatalf("cutover: %v", err)
	}
	all := strings.Join(f.Commands, "\n")
	upGreen := strings.Index(all, "up -d --no-deps web-green")
	flip := strings.Index(all, "web.conf")
	stopBlue := strings.Index(all, "stop web-blue")
	if !(upGreen >= 0 && flip > upGreen && stopBlue > flip) {
		t.Fatalf("order must be up-green -> flip -> stop-blue:\n%s", all)
	}
	if strings.Contains(all, "DOCKRAIL_GPU=1") == false {
		t.Fatalf("green must be pinned to the free GPU:\n%s", all)
	}
}

// No free slot + stop-old-first -> blue stopped, green up, flip. Gap accepted.
func TestProxyCutoverStopOldFirst(t *testing.T) {
	f := connection.NewFake()
	f.Stub("query-gpu", "0, 1000\n1, 1000\n", nil)     // none free
	f.Stub("ps -q web-blue", "cid-blue\n", nil)
	e := &Engine{Conn: f, Cfg: bgCfg(), Out: discard()}
	if err := e.proxyCutover(context.Background(), "web", bgService(), "v2"); err != nil {
		t.Fatalf("cutover: %v", err)
	}
	all := strings.Join(f.Commands, "\n")
	stopBlue := strings.Index(all, "stop web-blue")
	upGreen := strings.Index(all, "up -d --no-deps web-green")
	if !(stopBlue >= 0 && upGreen > stopBlue) {
		t.Fatalf("stop-old-first: blue must stop before green starts:\n%s", all)
	}
}

// No free slot + fail -> abort, nothing mutated.
func TestProxyCutoverFailBranch(t *testing.T) {
	f := connection.NewFake()
	f.Stub("query-gpu", "0, 1000\n", nil)
	f.Stub("ps -q web-blue", "cid-blue\n", nil)
	svc := bgService()
	svc.Placement.Pool = []int{0}
	svc.Placement.OnNoFreeGPU = "fail"
	e := &Engine{Conn: f, Cfg: bgCfg(), Out: discard()}
	err := e.proxyCutover(context.Background(), "web", svc, "v2")
	if err == nil || !strings.Contains(err.Error(), "no free GPU") {
		t.Fatalf("want no-free-GPU abort, got %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "up -d") {
		t.Fatal("fail branch must not start anything")
	}
	_ = errors.Is
}

// stop-old-first green fails readiness -> auto-rollback restarts blue.
func TestProxyCutoverAutoRollback(t *testing.T) {
	f := connection.NewFake()
	f.Stub("query-gpu", "0, 1000\n", nil)
	f.Stub("ps -q web-blue", "cid-blue\n", nil)
	f.Stub("curl", "", errors.New("green not ready")) // readiness fails
	svc := bgService()
	svc.Placement.Pool = []int{0}
	e := &Engine{Conn: f, Cfg: bgCfg(), Out: discard()}
	err := e.proxyCutover(context.Background(), "web", svc, "v2")
	if err == nil {
		t.Fatal("want cutover error")
	}
	all := strings.Join(f.Commands, "\n")
	// blue must be brought back up after green failed
	if !strings.Contains(all, "up -d --no-deps web-blue") {
		t.Fatalf("auto-rollback must restart blue:\n%s", all)
	}
}
```

Add helpers `bgCfg()` (project `demo`, compose `docker-compose.yml`, one service `web` = `bgService()`) and `discard()` (`&bytes.Buffer{}`) to the test file.

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./engine/... -run TestProxyCutover -v`
Expected: FAIL — `proxyCutover` undefined.

- [ ] **Step 3: Implement `engine/bluegreen.go`**

```go
package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/strategy/placement"
	"github.com/goodsmileduck/dockrail/strategy/readiness"
)

func otherColor(c string) string {
	if c == "blue" {
		return "green"
	}
	return "blue"
}

// activeColor returns the currently-running color for a service, or "" if
// neither blue nor green is up (first deploy).
func (e *Engine) activeColor(ctx context.Context, service string) (string, error) {
	for _, c := range []string{"blue", "green"} {
		out, err := e.Conn.Run(ctx, fmt.Sprintf("docker compose -f %s ps -q %s-%s",
			e.Cfg.Compose, service, c))
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(out) != "" {
			return c, nil
		}
	}
	return "", nil
}

func (e *Engine) composeCmd(tag, gpu, action, svcColor string) string {
	env := fmt.Sprintf("TAG=%s ", tag)
	if gpu != "" {
		env += fmt.Sprintf("DOCKRAIL_GPU=%s ", gpu)
	}
	return fmt.Sprintf("%sdocker compose -f %s %s %s", env, e.Cfg.Compose, action, svcColor)
}

// proxyCutover performs a blue-green cutover for one service. With a free GPU
// slot it is zero-gap (green up alongside blue, flip, stop blue); otherwise it
// follows on_no_free_gpu (fail = abort; stop-old-first = stop blue, start
// green, flip, with auto-rollback to blue on readiness failure).
func (e *Engine) proxyCutover(ctx context.Context, name string, svc config.Service, tag string) error {
	if !safeTag.MatchString(tag) {
		return fmt.Errorf("unsafe image tag %q", tag)
	}
	prober, err := readiness.New(svc.Readiness, svc.Model)
	if err != nil {
		return err
	}
	active, err := e.activeColor(ctx, name)
	if err != nil {
		return err
	}
	if active == "" {
		active = "blue" // nothing running; treat blue as the (empty) old color
	}
	target := otherColor(active)
	blueSvc := name + "-" + active
	greenSvc := name + "-" + target

	// GPU capacity decision (only for gpu placement).
	gpu := ""
	sequenced := false
	if svc.Placement.Type == "gpu" {
		placer, err := placement.New(svc.Placement)
		if err != nil {
			return err
		}
		idx, perr := placer.Pick(ctx, e.Conn)
		switch {
		case perr == nil:
			gpu = idx // free slot -> zero-gap
		case errors.Is(perr, placement.ErrNoFreeGPU):
			if svc.Placement.OnNoFreeGPU == "fail" {
				return fmt.Errorf("%s: no free GPU and on_no_free_gpu=fail", name)
			}
			sequenced = true // stop-old-first
		default:
			return perr
		}
	}

	e.logf("step pull: %s tag %s (%s)", name, tag, target)
	if _, err := e.Conn.Run(ctx, e.composeCmd(tag, gpu, "pull", greenSvc)); err != nil {
		return fmt.Errorf("pull green: %w", err)
	}

	if sequenced {
		// Free VRAM first: stop blue, then bring up green.
		e.logf("step stop-old-first: freeing VRAM by stopping %s", blueSvc)
		if _, err := e.Conn.Run(ctx, e.composeCmd(tag, "", "stop", blueSvc)); err != nil {
			return fmt.Errorf("stop blue: %w", err)
		}
		if _, err := e.Conn.Run(ctx, e.composeCmd(tag, gpu, "up -d --no-deps", greenSvc)); err != nil {
			return fmt.Errorf("start green: %w", err)
		}
		if err := prober.Probe(ctx, e.Conn); err != nil {
			// Auto-rollback: green never became ready and blue is down.
			e.logf("green failed readiness; auto-rolling back to %s", blueSvc)
			_, _ = e.Conn.Run(ctx, e.composeCmd(tag, "", "up -d --no-deps", blueSvc))
			return fmt.Errorf("green readiness failed, rolled back to blue: %w", err)
		}
		return flipUpstream(ctx, e.Conn, e.Cfg.Project, svc.Cutover.Proxy, name, target, svc.Readiness.Port)
	}

	// Zero-gap: green up alongside blue, gate, flip, then stop blue.
	e.logf("step blue-green: starting %s alongside %s", greenSvc, blueSvc)
	if _, err := e.Conn.Run(ctx, e.composeCmd(tag, gpu, "up -d --no-deps", greenSvc)); err != nil {
		return fmt.Errorf("start green: %w", err)
	}
	if err := prober.Probe(ctx, e.Conn); err != nil {
		// Green failed but blue still serves; tear down green, leave blue.
		_, _ = e.Conn.Run(ctx, e.composeCmd(tag, "", "stop", greenSvc))
		return fmt.Errorf("green readiness failed (blue still serving): %w", err)
	}
	if err := flipUpstream(ctx, e.Conn, e.Cfg.Project, svc.Cutover.Proxy, name, target, svc.Readiness.Port); err != nil {
		return err
	}
	e.logf("flip complete; stopping old %s", blueSvc)
	if _, err := e.Conn.Run(ctx, e.composeCmd(tag, "", "stop", blueSvc)); err != nil {
		return fmt.Errorf("stop blue after flip: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Route `proxy` strategy from the service loop**

In `engine.go`, `recreate` currently errors for non-`recreate` strategies. Replace the dispatch so the per-service call chooses the path. The cleanest seam: rename the current `recreate` body stays, and the caller (`Deploy`/`Rollback` service loop) picks:

```go
func (e *Engine) cutover(ctx context.Context, name string, svc config.Service, tag string) error {
	switch svc.Cutover.Strategy {
	case "recreate":
		return e.recreate(ctx, name, svc, tag)
	case "proxy":
		return e.proxyCutover(ctx, name, svc, tag)
	default:
		return fmt.Errorf("cutover strategy %q not implemented yet", svc.Cutover.Strategy)
	}
}
```

Replace the `e.recreate(ctx, name, svc, svc.ImageTag)` call in `Deploy` and the `e.recreate(ctx, name, svc, target)` call in `Rollback` with `e.cutover(...)`. Remove the strategy check from the top of `recreate` (now handled by `cutover`).

> Note: if the secrets plan (a) has landed, `recreate` and `proxyCutover` should both prepend the secrets prefix to their compose commands. Thread the prefix through `cutover` the same way. If (a) has not landed, skip — this plan does not depend on it.

- [ ] **Step 5: Run the full suite and build**

Run: `GOTOOLCHAIN=auto go test ./... && GOTOOLCHAIN=auto go build ./...`
Expected: PASS. Existing `recreate` tests still pass (routed through `cutover`); `TestDeployProxyStrategyNotImplemented` must be updated — proxy is now implemented, so change it to assert a successful proxy cutover or remove it in favor of the new `TestProxyCutover*` tests.

- [ ] **Step 6: Commit**

```bash
git add engine/
git commit -m "feat: gpu-aware blue-green proxy cutover with auto-rollback"
```

---

### Task 5: Documentation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document the proxy strategy and its conventions**

Cover: `cutover.strategy: proxy` requires `<svc>-blue` / `<svc>-green` compose services and `cutover.proxy: <nginx-container>`; the operator's nginx must `include $HOME/.dockrail/<project>/nginx/*.conf`; GPU services must map `${DOCKRAIL_GPU}` in their device reservation; the free-slot vs `stop-old-first` behavior and the `×1.2` VRAM rule; auto-rollback on sequenced failure. Include an annotated example:

```yaml
services:
  parse-agent:
    image_tag: v2
    model: Qwen2.5-VL
    readiness: { type: vllm, port: 8000, timeout: 900s }
    cutover:   { strategy: proxy, proxy: mlops-nginx }
    placement: { type: gpu, pool: [0, 1], vram_min: 18GiB, on_no_free_gpu: stop-old-first }
```

- [ ] **Step 2: Verify and commit**

Run: `GOTOOLCHAIN=auto gofmt -l . && GOTOOLCHAIN=auto go test ./...`
Expected: no gofmt output; PASS.

```bash
git add README.md
git commit -m "docs: document gpu-aware blue-green proxy cutover"
```

---

## Self-Review Notes

- **Correctness gate:** the whole feature is only safe when readiness means "model served" — this plan assumes the `vllm` probe (plan b) is merged. On the free-slot path, a green readiness failure leaves blue untouched (no outage). On the sequenced path, blue is already down, so auto-rollback restarts it — this is the single riskiest sequence and has a dedicated test (`TestProxyCutoverAutoRollback`).
- **No state-schema change:** color is derived from running containers, and the existing tag pair still records current/previous. `finalize` in `Deploy`/`Rollback` is unchanged; the color flip is orthogonal to the tag bookkeeping.
- **GPU assignment contract:** `DOCKRAIL_GPU` is injected only for the green start; blue restarts (rollback/stop) don't re-pin. Reviewer should confirm the compose convention is documented (Task 5) since it's a the dogfood project-side requirement.
- **`warmup` is intentionally a no-op** (decision 4). Do not let a reviewer flag its absence as a gap.
- **Existing test to update:** `TestDeployProxyStrategyNotImplemented` asserts the old stub error and must be replaced — called out in Task 4 Step 5 so it isn't missed.
- **Spec coverage:** vram parse ✅ (T1), gpu probe + ×1.2 + pool restriction ✅ (T2), nginx flip ✅ (T3), free-slot/sequenced/fail/auto-rollback orchestration ✅ (T4), docs + conventions ✅ (T5).
