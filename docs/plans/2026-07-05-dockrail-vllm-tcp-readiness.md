# dockrail vLLM + TCP Readiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the `vllm` and `tcp` readiness probe types — already accepted by config validation but stubbed in `readiness.New` — so dockrail can health-gate model servers (vLLM loading weights into VRAM) and plain TCP services.

**Architecture:** Both are new `Prober` implementations alongside the existing `HTTP`, dispatched from `readiness.New`. `tcp` waits for a port to accept connections. `vllm` extends the HTTP idea with model-aware semantics: it polls vLLM's `/health` AND confirms the configured model appears in `/v1/models`, with a long default timeout because weight loading takes minutes. Both reuse the existing poll-until-deadline loop shape from `HTTP.Probe`.

**Tech Stack:** Go 1.26, existing `strategy/readiness` package, `connection.Connection.Run` (probes run *on the target* via shell, same as HTTP). No new dependencies.

## Global Constraints

- Module `github.com/goodsmileduck/dockrail`; Go `1.26.0` / toolchain `go1.26.4`. Local: `GOTOOLCHAIN=auto`.
- Probes execute on the target through `connection.Connection.Run` (curl/bash one-liners), never from the dockrail host — consistent with `HTTP.Probe`, and required so the `Fake` records them and SSH targets work.
- `vllm` default timeout is `600s` (weight loading is slow); `tcp` default is `60s`. A `readiness.timeout` in yaml overrides the default.
- `vllm` MUST verify the model is actually served, not merely that the server answers: check `/v1/models` contains `Service.Model` when `Model` is set. If `Model` is empty, fall back to `/health` only.
- Retry cadence matches `HTTP`: poll every 2s until the deadline; return the last error on timeout.
- No config schema change — `Readiness.Type` already validates `http|tcp|vllm|cmd`; this plan implements two of the three currently-stubbed types (`cmd` is out of scope here).

---

### Task 1: TCP readiness probe

**Files:**
- Create: `strategy/readiness/tcp.go`
- Modify: `strategy/readiness/readiness.go` (dispatch `case "tcp"`)
- Test: `strategy/readiness/tcp_test.go`

**Interfaces:**
- Consumes: `config.Readiness{Port, Timeout}`, `connection.Connection`.
- Produces: `newTCP(config.Readiness) (*TCP, error)`; `TCP.Probe(ctx, conn) error`.

- [ ] **Step 1: Write the failing test**

The Fake returns success/failure per stubbed substring; assert the probe issues a bash TCP check against the port and succeeds when the check succeeds, fails on timeout when it never does.

```go
package readiness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestTCPProbeSucceeds(t *testing.T) {
	p, err := newTCP(config.Readiness{Type: "tcp", Port: 8010, Timeout: "5s"})
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake() // unstubbed = success
	if err := p.Probe(context.Background(), f); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"), "/dev/tcp/localhost/8010") {
		t.Fatalf("expected a /dev/tcp check, got %v", f.Commands)
	}
}

func TestTCPProbeTimesOut(t *testing.T) {
	p, err := newTCP(config.Readiness{Type: "tcp", Port: 8010, Timeout: "10ms"})
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake()
	f.Stub("/dev/tcp/localhost/8010", "", errors.New("refused"))
	if err := p.Probe(context.Background(), f); err == nil {
		t.Fatal("want timeout error")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./strategy/readiness/... -run TestTCPProbe -v`
Expected: FAIL — `newTCP` undefined.

- [ ] **Step 3: Implement `strategy/readiness/tcp.go`**

```go
package readiness

import (
	"context"
	"fmt"
	"time"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type TCP struct {
	Port       int
	Timeout    time.Duration
	retryEvery time.Duration
}

func newTCP(r config.Readiness) (*TCP, error) {
	timeout := 60 * time.Second
	if r.Timeout != "" {
		var err error
		if timeout, err = time.ParseDuration(r.Timeout); err != nil {
			return nil, err
		}
	}
	if r.Port == 0 {
		return nil, fmt.Errorf("tcp readiness requires a port")
	}
	return &TCP{Port: r.Port, Timeout: timeout, retryEvery: 2 * time.Second}, nil
}

func (p *TCP) Probe(ctx context.Context, conn connection.Connection) error {
	// bash's /dev/tcp pseudo-device opens a connection; redirect closes it.
	cmd := fmt.Sprintf("timeout 5 bash -c '</dev/tcp/localhost/%d' 2>/dev/null", p.Port)
	deadline := time.Now().Add(p.Timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, lastErr = conn.Run(ctx, cmd); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.retryEvery):
		}
	}
	return fmt.Errorf("tcp readiness on port %d failed after %s: %w", p.Port, p.Timeout, lastErr)
}
```

- [ ] **Step 4: Dispatch it in `readiness.go`**

```go
	case "tcp":
		return newTCP(r)
```

- [ ] **Step 5: Run the tests**

Run: `GOTOOLCHAIN=auto go test ./strategy/readiness/... -run TestTCPProbe -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add strategy/readiness/tcp.go strategy/readiness/tcp_test.go strategy/readiness/readiness.go
git commit -m "feat: add tcp readiness probe"
```

---

### Task 2: vLLM readiness probe (health + model-served check)

**Files:**
- Create: `strategy/readiness/vllm.go`
- Modify: `strategy/readiness/readiness.go` (dispatch `case "vllm"`; thread the model name)
- Test: `strategy/readiness/vllm_test.go`

**Interfaces:**
- Consumes: `config.Readiness{Port, Path, Timeout}` plus the service's `Model` string. Because `readiness.New` currently takes only `config.Readiness`, extend it to also accept the model name.
- Produces: `newVLLM(r config.Readiness, model string) (*VLLM, error)`; `VLLM.Probe(ctx, conn) error`.

**Signature change:** `readiness.New(r config.Readiness)` becomes `readiness.New(r config.Readiness, model string)`. The only caller is `engine.recreate` (`readiness.New(svc.Readiness)`), which becomes `readiness.New(svc.Readiness, svc.Model)`. Update the existing HTTP/TCP branches to ignore the model argument.

- [ ] **Step 1: Write the failing test**

```go
package readiness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestVLLMProbeChecksHealthAndModel(t *testing.T) {
	p, err := newVLLM(config.Readiness{Type: "vllm", Port: 8000, Timeout: "5s"}, "Qwen2.5-VL")
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake()
	f.Stub("/health", "", nil)               // server up
	f.Stub("/v1/models", `{"data":[{"id":"Qwen2.5-VL"}]}`, nil) // model served
	if err := p.Probe(context.Background(), f); err != nil {
		t.Fatalf("probe: %v", err)
	}
	all := strings.Join(f.Commands, "\n")
	if !strings.Contains(all, ":8000/health") || !strings.Contains(all, ":8000/v1/models") {
		t.Fatalf("expected health + models checks, got:\n%s", all)
	}
}

func TestVLLMProbeFailsWhenModelAbsent(t *testing.T) {
	p, err := newVLLM(config.Readiness{Type: "vllm", Port: 8000, Timeout: "20ms"}, "Qwen2.5-VL")
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake()
	f.Stub("/health", "", nil)
	f.Stub("/v1/models", `{"data":[{"id":"other-model"}]}`, nil) // wrong model
	if err := p.Probe(context.Background(), f); err == nil {
		t.Fatal("want failure when configured model is not served")
	}
}

func TestVLLMProbeHealthOnlyWhenNoModel(t *testing.T) {
	p, err := newVLLM(config.Readiness{Type: "vllm", Port: 8000, Timeout: "5s"}, "")
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake()
	f.Stub("/health", "", nil)
	if err := p.Probe(context.Background(), f); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "/v1/models") {
		t.Fatal("must not check /v1/models when no model configured")
	}
	_ = errors.New // keep import
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./strategy/readiness/... -run TestVLLMProbe -v`
Expected: FAIL — `newVLLM` undefined.

- [ ] **Step 3: Implement `strategy/readiness/vllm.go`**

```go
package readiness

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

// VLLM waits for a vLLM server to become ready. Unlike a plain HTTP probe it
// confirms the *model* is loaded and served: /health returning 200 only means
// the process is up, while weight loading into VRAM can take minutes and the
// model appears in /v1/models only once it is servable.
type VLLM struct {
	Port       int
	Model      string
	Timeout    time.Duration
	retryEvery time.Duration
}

func newVLLM(r config.Readiness, model string) (*VLLM, error) {
	timeout := 600 * time.Second // weight loading is slow
	if r.Timeout != "" {
		var err error
		if timeout, err = time.ParseDuration(r.Timeout); err != nil {
			return nil, err
		}
	}
	port := r.Port
	if port == 0 {
		port = 8000 // vLLM default
	}
	return &VLLM{Port: port, Model: model, Timeout: timeout, retryEvery: 2 * time.Second}, nil
}

func (p *VLLM) Probe(ctx context.Context, conn connection.Connection) error {
	health := fmt.Sprintf("curl -fsS -m 5 http://localhost:%d/health >/dev/null", p.Port)
	models := fmt.Sprintf("curl -fsS -m 5 http://localhost:%d/v1/models", p.Port)
	deadline := time.Now().Add(p.Timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = p.check(ctx, conn, health, models)
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.retryEvery):
		}
	}
	return fmt.Errorf("vllm readiness on port %d failed after %s: %w", p.Port, p.Timeout, lastErr)
}

func (p *VLLM) check(ctx context.Context, conn connection.Connection, health, models string) error {
	if _, err := conn.Run(ctx, health); err != nil {
		return fmt.Errorf("health: %w", err)
	}
	if p.Model == "" {
		return nil
	}
	out, err := conn.Run(ctx, models)
	if err != nil {
		return fmt.Errorf("models: %w", err)
	}
	// vLLM returns {"data":[{"id":"<model>"}...]}. A substring check on the
	// configured id is sufficient and avoids a JSON dependency in the probe.
	if !strings.Contains(out, fmt.Sprintf(`"id":"%s"`, p.Model)) &&
		!strings.Contains(out, fmt.Sprintf(`"id": "%s"`, p.Model)) {
		return fmt.Errorf("model %q not yet served", p.Model)
	}
	return nil
}
```

- [ ] **Step 4: Change `readiness.New` signature and dispatch vllm**

```go
func New(r config.Readiness, model string) (Prober, error) {
	switch r.Type {
	case "http":
		return newHTTP(r)
	case "tcp":
		return newTCP(r)
	case "vllm":
		return newVLLM(r, model)
	default:
		return nil, fmt.Errorf("readiness type %q not implemented yet", r.Type)
	}
}
```

- [ ] **Step 5: Update the caller in `engine/engine.go`**

```go
	prober, err := readiness.New(svc.Readiness, svc.Model)
```

- [ ] **Step 6: Run the full suite and build**

Run: `GOTOOLCHAIN=auto go test ./... && GOTOOLCHAIN=auto go build ./...`
Expected: PASS (engine tests use `type: http`; the added model arg is ignored there).

- [ ] **Step 7: Commit**

```bash
git add strategy/readiness/ engine/engine.go
git commit -m "feat: add vllm readiness probe (health + model-served)"
```

---

### Task 3: Document readiness types

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document `tcp` and `vllm`**

Add to the readiness docs: `tcp` waits for the port to accept connections (default 60s); `vllm` waits for `/health` and, when `model:` is set on the service, for that model to appear in `/v1/models` (default 600s because weight loading is slow, override via `readiness.timeout`). Show a vLLM service example:

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

- [ ] **Step 2: Verify and commit**

Run: `GOTOOLCHAIN=auto gofmt -l . && GOTOOLCHAIN=auto go test ./...`
Expected: no gofmt output; PASS.

```bash
git add README.md
git commit -m "docs: document tcp and vllm readiness types"
```

---

## Self-Review Notes

- **vLLM readiness is the differentiator.** The model-served check (not just `/health`) is the whole point — a plain HTTP 200 passes long before the model can serve a request, which would let dockrail cut over to a not-yet-ready GPU service. The `Model` field already exists on `config.Service` for exactly this.
- **Signature change is localized.** `readiness.New` gains a `model` param; the sole caller is `engine.recreate`. HTTP/TCP ignore it, so behavior is unchanged for existing configs.
- **Substring model match** avoids pulling a JSON parser into the probe; acceptable because vLLM ids are exact and controlled. If a config ever uses an id that is a substring of another, revisit — noted for the reviewer.
- **Long default timeout** (600s) is deliberate and matches the dogfood project's GPU reality; it is overridable. Do not let a reviewer "simplify" it back to 60s.
- **Spec coverage:** tcp ✅ (T1), vllm health+model ✅ (T2), docs ✅ (T3). `cmd` readiness remains stubbed and out of scope.
