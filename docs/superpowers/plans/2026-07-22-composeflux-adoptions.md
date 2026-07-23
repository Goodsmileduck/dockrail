# ComposeFlux-Inspired Adoptions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Adopt four features surveyed from [composeflux](https://github.com/veerendra2/composeflux) into dockrail: (1) rendered-config hash + `dockrail.config-hash` label for no-op skip and non-tag change detection, (2) secrets-manager provider adapters (env + Infisical), (3) read-only image-digest drift detection in `check`, (4) client-side compose file validation via `compose-go`.

**Architecture:** All four features respect dockrail's locked decisions — everything that touches the host goes through the agentless `Connection` (shell over SSH), never a Docker socket. The hash feature has two halves: the v2 fleet path stamps a `dockrail.config-hash` container label (read by the Observer, diffed by the Planner) and the v1 engine records a project config hash in the deploy history for a `deploy` no-op skip. Secrets providers are a new interface-bounded `secrets` package; delivery stays D8 (mode-600 env_file). Drift detection and compose validation are read-only additions to `check`.

**Tech Stack:** Go 1.26, cobra, `gopkg.in/yaml.v3`. One new dependency: `github.com/compose-spec/compose-go/v2` (pure Go, no Docker daemon). Infisical uses stdlib `net/http` — no SDK. Bitwarden is **deliberately deferred**: its Go SDK wraps a C library (cgo), which conflicts with D1 (single static binary, 6-target cross-compile matrix).

## Global Constraints

- D1: single static binary — no cgo dependencies, ever.
- D2: agentless — host interaction only via `connection.Connection.Run(ctx, cmd)`; never a local Docker client for remote state.
- D8: secrets reach the host only via the mode-600 env_file heredoc in `engine/secrets.go`; never on argv.
- D10: dockrail deploys, it does not build; drift detection is **read-only** (no auto-redeploy).
- Strings interpolated into host shell commands must be validated (`safeTag` / `projectRe` pattern) — every new command follows this.
- Tests use `connection.NewFake()` (records `Commands []string`, `Stub(substr, stdout, err)` responds by substring match).
- Gate before every commit: `go test ./... && go vet ./... && gofmt -l .` (gofmt must print nothing).
- Do not use the `§` symbol in any markdown; write "section N".
- Public repo: examples use `example.com` / placeholder names only.
- Commits are part of executing this plan (user-approved); never `git push`.

## Part order and independence

Each part is independently shippable; Part 1 must precede nothing but is listed first because Part 2's engine hash reuses no code from it (they are independent — Part 1 is local validation, Part 2 hashes remote content). Suggested order: 1 → 2 → 3 → 4, but any order works.

---

## Part 1 — Client-side compose validation (`compose-go`)

New package `composecheck` loads the **local** compose file with `compose-go` (interpolation + normalization, no daemon) and verifies every service `deploy.yml` references actually exists in it — including the `-blue`/`-green` pair for `proxy` cutover. Wired into `dockrail check`. If the compose file does not exist locally (it may only exist on the target), validation is skipped with a note — preflight's remote `test -f` check is unchanged.

### Task 1: `composecheck` package

**Files:**
- Create: `composecheck/composecheck.go`
- Test: `composecheck/composecheck_test.go`
- Modify: `go.mod` (via `go get`)

**Interfaces:**
- Produces: `composecheck.Validate(ctx context.Context, cfg *config.Config) []error` — nil/empty means the compose file parsed and all referenced services exist. Also `composecheck.Load(ctx context.Context, path, project string) (*types.Project, error)` (exported for future rendered-YAML use).

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/compose-spec/compose-go/v2@latest
```

Note: check the resolved API before writing code — in compose-go v2 the loader entry points are `cli.NewProjectOptions(...)` + `cli.ProjectFromOptions(ctx, opts)` (or `opts.LoadProject(ctx)` on newer minors). Use whichever the resolved version exposes; the tests below pin the behavior, not the API.

- [ ] **Step 2: Write the failing test**

```go
package composecheck

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
)

func writeCompose(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func cfgFor(t *testing.T, compose string, svcs map[string]config.Service) *config.Config {
	t.Helper()
	return &config.Config{Project: "demo", Compose: compose, Services: svcs}
}

func TestValidate_AllServicesPresent(t *testing.T) {
	p := writeCompose(t, `
services:
  web:
    image: ghcr.io/example/app:${TAG:-latest}
`)
	cfg := cfgFor(t, p, map[string]config.Service{
		"web": {ImageTag: "v1", Cutover: config.Cutover{Strategy: "recreate"}},
	})
	if errs := Validate(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidate_MissingService(t *testing.T) {
	p := writeCompose(t, `
services:
  other:
    image: ghcr.io/example/app:v1
`)
	cfg := cfgFor(t, p, map[string]config.Service{
		"web": {ImageTag: "v1", Cutover: config.Cutover{Strategy: "recreate"}},
	})
	errs := Validate(context.Background(), cfg)
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
}

func TestValidate_ProxyNeedsBlueGreen(t *testing.T) {
	p := writeCompose(t, `
services:
  api-blue:
    image: ghcr.io/example/api:v1
`)
	cfg := cfgFor(t, p, map[string]config.Service{
		"api": {ImageTag: "v1", Cutover: config.Cutover{Strategy: "proxy", Proxy: "nginx"}},
	})
	errs := Validate(context.Background(), cfg)
	if len(errs) != 1 { // api-green missing
		t.Fatalf("want 1 error (api-green missing), got %v", errs)
	}
}

func TestValidate_BadComposeYAML(t *testing.T) {
	p := writeCompose(t, "services: [not: a: mapping\n")
	cfg := cfgFor(t, p, map[string]config.Service{
		"web": {ImageTag: "v1"},
	})
	if errs := Validate(context.Background(), cfg); len(errs) == 0 {
		t.Fatal("want parse error, got none")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./composecheck/ -v`
Expected: FAIL — `undefined: Validate`

- [ ] **Step 4: Implement**

```go
// Package composecheck validates the local compose file against deploy.yml
// references using compose-go — parse + interpolate only, no Docker daemon,
// so it works anywhere dockrail runs (laptop, CI). The target host's copy of
// the compose file is still checked separately by engine.Preflight.
package composecheck

import (
	"context"
	"fmt"
	"sort"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/goodsmileduck/dockrail/config"
)

// Load parses and interpolates the compose file at path. TAG and DOCKRAIL_GPU
// get placeholder values because dockrail injects them at deploy time; other
// ${VAR} references resolve from the invoking environment, same as compose
// itself would.
func Load(ctx context.Context, path, project string) (*types.Project, error) {
	opts, err := cli.NewProjectOptions(
		[]string{path},
		cli.WithName(project),
		cli.WithOsEnv,
		cli.WithEnv([]string{"TAG=dockrail-validate", "DOCKRAIL_GPU=0"}),
		cli.WithInterpolation(true),
		cli.WithNormalization(true),
	)
	if err != nil {
		return nil, err
	}
	return cli.ProjectFromOptions(ctx, opts)
}

// Validate checks that every service deploy.yml references exists in the
// compose file: <name> for recreate cutover, <name>-blue and <name>-green for
// proxy cutover. A parse failure is returned as the single error.
func Validate(ctx context.Context, cfg *config.Config) []error {
	project, err := Load(ctx, cfg.Compose, cfg.Project)
	if err != nil {
		return []error{fmt.Errorf("compose %s: %w", cfg.Compose, err)}
	}
	present := map[string]bool{}
	for name := range project.Services {
		present[name] = true
	}
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		svc := cfg.Services[name]
		want := []string{name}
		if svc.Cutover.Strategy == "proxy" {
			want = []string{name + "-blue", name + "-green"}
		}
		for _, w := range want {
			if !present[w] {
				errs = append(errs, fmt.Errorf("services.%s: compose file %s has no service %q", name, cfg.Compose, w))
			}
		}
	}
	return errs
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./composecheck/ -v`
Expected: PASS (all 4). If `cli.ProjectFromOptions` doesn't exist in the resolved version, switch to `opts.LoadProject(ctx)` — tests define correctness.

- [ ] **Step 6: Gate and commit**

```bash
go test ./... && go vet ./... && gofmt -l .
git add composecheck/ go.mod go.sum
git commit -m "feat(check): local compose validation via compose-go"
```

### Task 2: Wire into `dockrail check`

**Files:**
- Modify: `cmd/root.go` (`newCheckCmd`)
- Test: `cmd/root_test.go` (extend) or new `cmd/check_test.go`

**Interfaces:**
- Consumes: `composecheck.Validate(ctx, cfg) []error` from Task 1.

- [ ] **Step 1: Write the failing test**

In `cmd/check_test.go` — build a config whose compose file exists locally but lacks a referenced service, run the check command with a fake-friendly setup. `check` uses `loadConn`, which opens a real connection from `deploy.yml`; use a local target (empty `host:`) so `connection.New` returns local exec, and stub nothing — preflight commands will run against the local shell, so pick preflight-satisfiable content or assert only on the compose-validation failure text:

```go
package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCheck_ReportsMissingComposeService(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "docker-compose.yml")
	os.WriteFile(compose, []byte("services:\n  other:\n    image: example.com/app:v1\n"), 0o644)
	deploy := filepath.Join(dir, "deploy.yml")
	os.WriteFile(deploy, []byte(`
project: demo
compose: `+compose+`
services:
  web:
    image_tag: example.com/app:v1
    readiness: {type: tcp, port: 8080}
    cutover: {strategy: recreate}
`), 0o644)

	root := NewRootCmd()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"check", "-c", deploy})
	err := root.Execute()
	if err == nil {
		t.Fatal("want check to fail on missing compose service")
	}
	if !bytes.Contains(errb.Bytes(), []byte(`no service "web"`)) {
		t.Fatalf("stderr missing compose validation error: %s", errb.String())
	}
}
```

(If existing cmd tests already have a harness for running commands with fake connections, reuse that idiom instead — match the file's established style.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/ -run TestCheck_ReportsMissingComposeService -v`
Expected: FAIL — check passes today without compose validation (or fails only on preflight; assert message text).

- [ ] **Step 3: Implement in `newCheckCmd`**

After loading config, before or after preflight (keep preflight first to match current output order), add:

```go
// Local compose validation: only when the file exists here; the target
// host's copy is preflight's job.
if _, statErr := os.Stat(cfg.Compose); statErr == nil {
	for _, e := range composecheck.Validate(cmd.Context(), cfg) {
		errs = append(errs, e)
	}
} else {
	fmt.Fprintln(cmd.OutOrStdout(), "note: compose file not present locally; skipping local compose validation")
}
```

Fold this into the existing `errs` handling so the "N preflight check(s) failed" summary covers both (rename the message to `"%d check(s) failed"`). Add imports `os` and `github.com/goodsmileduck/dockrail/composecheck`.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/ -v`
Expected: PASS, including pre-existing check tests (update any that assert the old "preflight check(s) failed" wording).

- [ ] **Step 5: Update README + commit**

README "Usage" section, `check` line becomes: `dockrail check  # validate config + compose file + probe target host readiness`.

```bash
go test ./... && go vet ./... && gofmt -l .
git add cmd/ README.md
git commit -m "feat(check): validate deploy.yml service refs against local compose file"
```

---

## Part 2 — Config hash: no-op skip + non-tag change detection

Composeflux's core trick: hash the rendered desired state, stamp it on what's running, skip when equal, redeploy when different even if the tag is unchanged. Two halves:

- **Fleet (v2, this branch):** a `dockrail.config-hash` label stamped in the generated overrides, read by the Observer, diffed by the Planner. Catches "same tag, changed placement/wiring/template".
- **Engine (v1):** a `config_hash` field on history records; `deploy` skips when the last successful record's hash matches the freshly computed one. `deploy --force` bypasses.

Known limitation (same as composeflux): secret **values** are not part of the hash (they're collected after the skip decision, and hashing them would leak timing/equality information into history). Changing only a secret requires `--force`. Document this.

### Task 3: `fleet/override` package with hash-stamped overrides

**Files:**
- Create: `fleet/override/override.go` (move `replicaOverride`/`serviceOverride` out of `fleet/apply/override.go`, exported)
- Create: `fleet/override/override_test.go` (absorb `fleet/apply/override_test.go` cases)
- Delete: `fleet/apply/override.go`, `fleet/apply/override_test.go`
- Modify: `fleet/apply/exec.go` (call sites: `place`, `deployService` — thread the action's image into the new signatures)
- Modify: `fleet/observe/observe.go` (add `LabelConfigHash` const only — psQuery column comes in Task 4)

**Interfaces:**
- Produces:
  - `override.Hash(parts ...string) string` — `"sha256:" + hex` over parts joined with `"\x1f"`. Deterministic, order-sensitive.
  - `override.Replica(base, template, backend string, replica, gpu int, tag string) (body, hash string)` — the override YAML now includes `dockrail.config-hash: "<hash>"` in its labels block; hash = `Hash(tag, base, template, backend, strconv.Itoa(replica), strconv.Itoa(gpu))`. `tag` is `plan.Action.Tag` — the Action carries a tag, not a full image ref (verified against `fleet/plan/plan.go`).
  - `override.Service(base, template, service, tag string) (body, hash string)` — hash = `Hash(tag, base, template, service)`.
  - `observe.LabelConfigHash = "dockrail.config-hash"`.
- Consumes: `observe.Label*` constants (existing).

The hash is computed over the **input tuple**, not the YAML text — that avoids the circularity of a hash label inside the hashed body, and the tuple is exactly what shapes the override. When the wiring driver later bakes endpoint env into overrides, endpoints join the tuple (the variadic `Hash` makes that a call-site change).

- [ ] **Step 1: Write the failing tests**

`fleet/override/override_test.go`:

```go
package override

import (
	"strings"
	"testing"
)

func TestHash_DeterministicAndOrderSensitive(t *testing.T) {
	a := Hash("img:v1", "compose.yml", "vllm", "b1", "0", "1")
	b := Hash("img:v1", "compose.yml", "vllm", "b1", "0", "1")
	c := Hash("img:v1", "compose.yml", "vllm", "b1", "1", "0")
	if a != b {
		t.Fatal("hash not deterministic")
	}
	if a == c {
		t.Fatal("hash ignored argument order")
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("want sha256: prefix, got %s", a)
	}
}

func TestReplica_StampsConfigHashLabel(t *testing.T) {
	body, hash := Replica("compose.yml", "vllm", "b1", 0, 1, "example.com/vllm:v2")
	if !strings.Contains(body, `dockrail.config-hash: "`+hash+`"`) {
		t.Fatalf("override missing config-hash label:\n%s", body)
	}
	// existing identity labels must survive the move
	for _, want := range []string{"dockrail.managed", "dockrail.backend: b1", `dockrail.replica: "0"`, `dockrail.gpu: "1"`, `device_ids: ["1"]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("override missing %q:\n%s", want, body)
		}
	}
}

func TestService_StampsConfigHashLabel(t *testing.T) {
	body, hash := Service("compose.yml", "api-tpl", "api", "example.com/api:v3")
	if !strings.Contains(body, `dockrail.config-hash: "`+hash+`"`) {
		t.Fatalf("override missing config-hash label:\n%s", body)
	}
}
```

Port every assertion from the current `fleet/apply/override_test.go` into this file too (same expectations, new function names).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./fleet/override/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement `fleet/override/override.go`**

Move the two template functions from `fleet/apply/override.go` verbatim, then: export as `Replica`/`Service`, append `image string` parameter, compute `hash := Hash(...)` per the Interfaces block, add one label line to each template (`%s: "%s"` with `observe.LabelConfigHash, hash`), and return `(body, hash)`.

```go
// Package override renders the per-replica / per-service compose overrides
// and the config hash stamped into them. It is shared by apply (which writes
// the override and runs compose) and plan (which computes the same desired
// hash to diff against the observed dockrail.config-hash label).
package override

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Hash returns "sha256:<hex>" over parts joined with an unprintable
// separator. Order-sensitive by design: the tuple is (image, base, template,
// identity..., placement...), and any reordering is a different config.
func Hash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "sha256:" + hex.EncodeToString(sum[:])
}
```

(plus the two moved-and-extended render functions — keep their doc comments, adjust for the new label line and return values).

- [ ] **Step 4: Update `fleet/apply/exec.go` call sites**

`place` (exec.go:70, `replicaOverride(x.baseName(), b.Service, a.Backend, a.Replica, a.GPU)`) and `deployService` (exec.go:102, `serviceOverride(x.baseName(), s.Service, a.Service)`) become `override.Replica(x.baseName(), b.Service, a.Backend, a.Replica, a.GPU, a.Tag)` and `override.Service(x.baseName(), s.Service, a.Service, a.Tag)`, using only the body return for `writeFile`. Delete `fleet/apply/override.go` and its test file.

- [ ] **Step 5: Run full fleet tests**

Run: `go test ./fleet/... -v`
Expected: PASS. `fleet/apply` tests that asserted override content now assert against overrides containing the extra label — update golden strings where needed.

- [ ] **Step 6: Gate and commit**

```bash
go test ./... && go vet ./... && gofmt -l .
git add fleet/
git commit -m "refactor(fleet): shared override package with dockrail.config-hash stamping"
```

### Task 4: Observer reads `dockrail.config-hash`

**Files:**
- Modify: `fleet/observe/observe.go` — add `LabelConfigHash` to the label consts (if not added in Task 3), append `{{.Label "dockrail.config-hash"}}` as a new trailing `\t` column in `psQuery`, append `LabelConfigHash` to `labelCols`.
- Test: `fleet/observe/observe_test.go`, `fleet/observe/psquery_shell_test.go`

**Interfaces:**
- Produces: observed `Container.Labels[observe.LabelConfigHash]` populated when the container carries the label.

- [ ] **Step 1: Write the failing test** — extend the existing observe parsing test with a fixture line carrying 8 tab-separated fields (name, image, managed, backend, replica, gpu, service, config-hash) and assert `Labels[LabelConfigHash]` round-trips. Respect the file's existing fixture style.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./fleet/observe/ -v`
Expected: FAIL — parser only reads 7 columns.

- [ ] **Step 3: Implement** — one column appended to the `psQuery` raw string (KEEP it a single backtick raw string — the `TestPSQuery_TemplateSurvivesShell` guard checks for literal `\t`), one entry appended to `labelCols`. Column order in `psQuery` and `labelCols` must stay aligned.

- [ ] **Step 4: Run tests**

Run: `go test ./fleet/observe/ -v`
Expected: PASS including the shell-survival guard test.

- [ ] **Step 5: Commit**

```bash
go test ./... && go vet ./... && gofmt -l .
git add fleet/observe/
git commit -m "feat(observe): read dockrail.config-hash label"
```

### Task 5: Planner diffs the hash

**Files:**
- Modify: `fleet/plan/plan.go` (`Compute` — the replica/service diff branches around lines 100–120 where labels are decoded)
- Test: `fleet/plan/plan_test.go`

**Interfaces:**
- Consumes: `override.Hash(...)` (Task 3), `observe.LabelConfigHash` (Task 4).
- Produces: `Compute` emits an update action when an observed replica/service matches on placement and tag but its `dockrail.config-hash` label differs from the desired hash; emits nothing when hash matches too. **Back-compat:** an observed container with an empty/absent hash label (deployed before this change) is treated as matching — no spurious fleet-wide redeploy on first run after upgrade; the hash gets stamped at the next real change.

- [ ] **Step 1: Write the failing tests** — in `fleet/plan/plan_test.go`, following the file's fixture style:
  - `TestCompute_HashDrift_EmitsUpdate`: observed replica with correct image/gpu/replica labels but `dockrail.config-hash: "sha256:stale"` → expect exactly one update action for it.
  - `TestCompute_HashMatch_NoAction`: observed hash equals `override.Hash(<same tuple the planner computes>)` → expect no action for that replica.
  - `TestCompute_MissingHashLabel_NoAction`: no hash label → no action (back-compat).

The desired-hash tuple in `Compute` must be **identical** to the one `apply` passes to `override.Replica`/`override.Service` (image, base compose filename, template, identity, placement). Extract a small helper in `plan.go` if the tuple assembly would otherwise be duplicated, e.g.:

```go
func desiredReplicaHash(base, template, backend string, replica, gpu int, tag string) string {
	return override.Hash(tag, base, template, backend, strconv.Itoa(replica), strconv.Itoa(gpu))
}
```

and have Task 3's `override.Replica` internally use the same argument order (it already does — this helper just mirrors it; keep a comment on both sides pointing at each other).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./fleet/plan/ -run TestCompute_Hash -v`
Expected: FAIL — planner currently ignores the label.

- [ ] **Step 3: Implement** — in the branch where an observed replica/service is matched against desired and currently produces no action, add the hash comparison per the Produces contract.

- [ ] **Step 4: Run tests**

Run: `go test ./fleet/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go test ./... && go vet ./... && gofmt -l .
git add fleet/plan/
git commit -m "feat(plan): diff dockrail.config-hash to catch non-tag config changes"
```

### Task 6: v1 engine no-op skip via history hash

**Files:**
- Modify: `engine/history.go` (`Record` struct: add `ConfigHash string \`json:"config_hash,omitempty"\``)
- Create: `engine/confighash.go` + `engine/confighash_test.go`
- Modify: `engine/engine.go` (`Engine` struct: add `Force bool` and unexported `configHash string`; `Deploy`: compute + skip; `finalize`: include hash in the success record)
- Modify: `cmd/deploy.go` (`--force` flag threaded through `runDeploy`)
- Test: `engine/engine_test.go` (skip-path cases), `cmd/deploy_test.go` (flag parse)

**Interfaces:**
- Produces: `(*Engine).desiredHash(ctx) (string, error)` — `"sha256:..."` over: remote compose file digest (`sha256sum -- <compose>` via Connection), then for each service in sorted name order: name + its marshaled `config.Service`. Rollback paths do **not** set a hash (their records keep `ConfigHash` empty), so a deploy after a rollback never skips.
- Consumes: `currentRecord(h []Record)`, `Record.success()` (existing, `engine/history.go`).

- [ ] **Step 1: Write the failing tests**

`engine/confighash_test.go`:

```go
package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func hashEngine(t *testing.T, tag string) (*Engine, *connection.Fake) {
	t.Helper()
	fake := connection.NewFake()
	fake.Stub("sha256sum", "abc123  docker-compose.yml\n", nil)
	cfg := &config.Config{Project: "demo", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {ImageTag: tag}}}
	return &Engine{Conn: fake, Cfg: cfg, Out: &strings.Builder{}}, fake
}

func TestDesiredHash_StableForSameInputs(t *testing.T) {
	e1, _ := hashEngine(t, "v1")
	e2, _ := hashEngine(t, "v1")
	h1, err := e1.desiredHash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := e2.desiredHash(context.Background())
	if h1 != h2 {
		t.Fatalf("hash unstable: %s vs %s", h1, h2)
	}
}

func TestDesiredHash_ChangesWithTag(t *testing.T) {
	e1, _ := hashEngine(t, "v1")
	e2, _ := hashEngine(t, "v2")
	h1, _ := e1.desiredHash(context.Background())
	h2, _ := e2.desiredHash(context.Background())
	if h1 == h2 {
		t.Fatal("hash must change when image_tag changes")
	}
}

func TestDesiredHash_ChangesWithRemoteCompose(t *testing.T) {
	e1, _ := hashEngine(t, "v1")
	e2, f2 := hashEngine(t, "v1")
	f2.stubs = nil // reset: different remote compose digest
	f2.Stub("sha256sum", "def456  docker-compose.yml\n", nil)
	h1, _ := e1.desiredHash(context.Background())
	h2, _ := e2.desiredHash(context.Background())
	if h1 == h2 {
		t.Fatal("hash must change when remote compose file changes")
	}
}
```

(`f2.stubs` is unexported — add a `ResetStubs()` method to `connection.Fake` in the same commit, or simply build the second fake fresh with the different stub; prefer the latter, adjust the test accordingly.)

In `engine/engine_test.go`, add a Deploy skip test following that file's existing Deploy-test fixture idiom: history stubbed so the last record is a success whose `config_hash` equals what `desiredHash` will compute; assert Deploy returns nil and **no** `docker compose ... up` command was issued (inspect `fake.Commands`); then set `Force: true` and assert compose commands do run.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./engine/ -run TestDesiredHash -v`
Expected: FAIL — `desiredHash` undefined.

- [ ] **Step 3: Implement `engine/confighash.go`**

```go
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// desiredHash fingerprints the deploy's inputs: the compose file as it exists
// on the target (dockrail runs compose against that copy, so that is the copy
// that matters) plus every service's deploy.yml stanza (tag, readiness,
// cutover, placement) in sorted name order. Secret VALUES are deliberately
// excluded — they are collected after the skip decision and hashing them
// would record value-equality in history; changing only a secret requires
// deploy --force.
func (e *Engine) desiredHash(ctx context.Context) (string, error) {
	out, err := e.Conn.Run(ctx, fmt.Sprintf("sha256sum -- %s", e.Cfg.Compose))
	if err != nil {
		return "", fmt.Errorf("hash compose file: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("hash compose file: empty sha256sum output")
	}
	parts := []string{fields[0]}
	names := make([]string, 0, len(e.Cfg.Services))
	for n := range e.Cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		svc := e.Cfg.Services[n]
		b, err := yaml.Marshal(svc)
		if err != nil {
			return "", err
		}
		parts = append(parts, n, string(b))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
```

- [ ] **Step 4: Wire into Deploy / finalize / cmd**

In `Deploy` (engine.go), after `loadHistory` succeeds and before `runServices`:

```go
hash, err := e.desiredHash(ctx)
if err != nil {
	return err
}
if !e.Force {
	if cur, ok := currentRecord(h); ok && cur.success() && cur.ConfigHash != "" && cur.ConfigHash == hash {
		e.logf("no changes since last deploy (config hash match); skipping — use --force to redeploy")
		return nil
	}
}
e.configHash = hash
```

In `finalize`, the record becomes `Record{Tag: tag, Outcome: outcome, Services: ids, ConfigHash: e.configHash}` — rollback paths never set `e.configHash`... except `runServices` is shared. Set `e.configHash` only in `Deploy`; zero-value threading means rollback records get `""`, which is exactly the "never skip after rollback" semantic. Reset is unnecessary (an Engine is per-invocation).

In `cmd/deploy.go`: add `c.Flags().Bool("force", false, "deploy even when the config hash matches the last successful deploy")`, read it in `RunE`, add a `force bool` parameter to `runDeploy`, set `Engine{... Force: force}`.

- [ ] **Step 5: Run tests**

Run: `go test ./engine/ ./cmd/ -v`
Expected: PASS, including existing deploy tests (they have no prior success record with a hash, so behavior is unchanged for them).

- [ ] **Step 6: Update README + commit**

README Usage block: add `dockrail deploy --force      # redeploy even if nothing changed since last deploy`. Add one sentence to the Configuration section: "`deploy` skips when nothing changed since the last successful deploy (compose file on target + deploy.yml service config + tag, recorded as `config_hash` in history); secret-only changes need `--force`."

```bash
go test ./... && go vet ./... && gofmt -l .
git add engine/ cmd/ README.md
git commit -m "feat(deploy): no-op skip via config hash in deploy history, --force override"
```

---

## Part 3 — Image-digest drift detection (read-only)

Detects mutable-tag drift: the tag configured in `deploy.yml` no longer points at the same digest in the registry as the image on the host. Agentless — both digests are read **on the target** through the Connection (`docker image inspect` for local, `docker buildx imagetools inspect` for remote; buildx ships with Docker 23+). Advisory only: `check --images` prints findings and exits 0 (D10 — dockrail never redeploys on its own).

### Task 7: `engine.ImageDrift`

**Files:**
- Create: `engine/drift.go`
- Test: `engine/drift_test.go`

**Interfaces:**
- Produces:

```go
type Drift struct {
	Service string
	Image   string
	Local   string // local RepoDigest digest ("" if image absent on host)
	Remote  string // registry manifest digest ("" if lookup failed)
	Note    string // "drift", "image not present on host", "remote lookup failed: <err>"
}
func ImageDrift(ctx context.Context, conn connection.Connection, cfg *config.Config) []Drift
```

Returns one entry per service **with a finding**; an up-to-date service produces no entry. Digest-pinned images (`@sha256:` in the ref) are skipped entirely — immutable by definition. Images failing `safeTag` are skipped (config validation already prevents them; belt-and-braces before shell interpolation).

- [ ] **Step 1: Write the failing tests**

```go
package engine

import (
	"context"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func driftCfg(tag string) *config.Config {
	return &config.Config{Project: "demo", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {ImageTag: tag}}}
}

func TestImageDrift_UpToDate_NoFinding(t *testing.T) {
	fake := connection.NewFake()
	fake.Stub("docker image inspect", "example.com/app@sha256:aaa\n", nil)
	fake.Stub("imagetools inspect", "sha256:aaa\n", nil)
	got := ImageDrift(context.Background(), fake, driftCfg("example.com/app:prod"))
	if len(got) != 0 {
		t.Fatalf("want no findings, got %+v", got)
	}
}

func TestImageDrift_DigestMismatch_ReportsDrift(t *testing.T) {
	fake := connection.NewFake()
	fake.Stub("docker image inspect", "example.com/app@sha256:aaa\n", nil)
	fake.Stub("imagetools inspect", "sha256:bbb\n", nil)
	got := ImageDrift(context.Background(), fake, driftCfg("example.com/app:prod"))
	if len(got) != 1 || got[0].Note != "drift" || got[0].Remote != "sha256:bbb" {
		t.Fatalf("want one drift finding, got %+v", got)
	}
}

func TestImageDrift_PinnedImageSkipped(t *testing.T) {
	fake := connection.NewFake()
	got := ImageDrift(context.Background(), fake, driftCfg("example.com/app@sha256:aaa"))
	if len(got) != 0 || len(fake.Commands) != 0 {
		t.Fatalf("pinned image must be skipped without host commands, got %+v / %v", got, fake.Commands)
	}
}

func TestImageDrift_RemoteLookupFails_ReportsNote(t *testing.T) {
	fake := connection.NewFake()
	fake.Stub("docker image inspect", "example.com/app@sha256:aaa\n", nil)
	fake.Stub("imagetools inspect", "", context.DeadlineExceeded)
	got := ImageDrift(context.Background(), fake, driftCfg("example.com/app:prod"))
	if len(got) != 1 || got[0].Note == "drift" {
		t.Fatalf("want lookup-failure note, got %+v", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./engine/ -run TestImageDrift -v`
Expected: FAIL — `ImageDrift` undefined.

- [ ] **Step 3: Implement `engine/drift.go`**

```go
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type Drift struct {
	Service, Image, Local, Remote, Note string
}

// ImageDrift compares, per service, the digest of the image on the target
// host against what the tag currently resolves to in the registry. Both
// lookups run on the target (agentless; the host's docker creds apply).
// Advisory only — dockrail never redeploys on drift (D10).
func ImageDrift(ctx context.Context, conn connection.Connection, cfg *config.Config) []Drift {
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Drift
	for _, name := range names {
		img := cfg.Services[name].ImageTag
		if strings.Contains(img, "@sha256:") || !safeTag.MatchString(img) {
			continue // pinned = immutable; unsafe = already rejected by config validation
		}
		local, err := conn.Run(ctx, fmt.Sprintf("docker image inspect --format '{{join .RepoDigests \"\\n\"}}' %s", img))
		if err != nil {
			out = append(out, Drift{Service: name, Image: img, Note: "image not present on host"})
			continue
		}
		remote, err := conn.Run(ctx, fmt.Sprintf("docker buildx imagetools inspect --format '{{.Manifest.Digest}}' %s", img))
		if err != nil {
			out = append(out, Drift{Service: name, Image: img, Local: digestOf(local), Note: fmt.Sprintf("remote lookup failed: %v", err)})
			continue
		}
		remoteDigest := strings.TrimSpace(remote)
		if matchesDigest(local, remoteDigest) {
			continue
		}
		out = append(out, Drift{Service: name, Image: img, Local: digestOf(local), Remote: remoteDigest, Note: "drift"})
	}
	return out
}

// digestOf returns the first digest part of a RepoDigests listing.
func digestOf(repoDigests string) string {
	for _, line := range strings.Split(strings.TrimSpace(repoDigests), "\n") {
		if _, d, ok := strings.Cut(line, "@"); ok {
			return d
		}
	}
	return ""
}

// matchesDigest reports whether any local RepoDigest line ends in the remote
// digest (local lines are "name@sha256:...").
func matchesDigest(repoDigests, remote string) bool {
	if remote == "" {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(repoDigests), "\n") {
		if _, d, ok := strings.Cut(line, "@"); ok && d == remote {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./engine/ -run TestImageDrift -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go test ./... && go vet ./... && gofmt -l .
git add engine/drift.go engine/drift_test.go
git commit -m "feat(engine): read-only image digest drift detection"
```

### Task 8: `check --images` flag

**Files:**
- Modify: `cmd/root.go` (`newCheckCmd`: add flag + reporting)
- Test: `cmd/check_test.go` (extend)

**Interfaces:**
- Consumes: `engine.ImageDrift(ctx, conn, cfg) []Drift` (Task 7).

- [ ] **Step 1: Write the failing test** — a cmd-level test asserting that `check --images` output contains a `DRIFT` line when digests mismatch is awkward against a real local connection; instead test the report formatting as a small helper:

```go
func TestFormatDrift(t *testing.T) {
	d := engine.Drift{Service: "web", Image: "example.com/app:prod",
		Local: "sha256:aaa", Remote: "sha256:bbb", Note: "drift"}
	got := formatDrift(d)
	want := "DRIFT web example.com/app:prod host=sha256:aaa registry=sha256:bbb"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./cmd/ -run TestFormatDrift -v` → FAIL, `formatDrift` undefined.

- [ ] **Step 3: Implement** — in `newCheckCmd`, register `Flags().Bool("images", false, "also compare host image digests against the registry (advisory)")`; when set and the preflight/validation gate passed, run `ImageDrift` and print per finding:

```go
func formatDrift(d engine.Drift) string {
	if d.Note != "drift" {
		return fmt.Sprintf("WARN %s %s: %s", d.Service, d.Image, d.Note)
	}
	return fmt.Sprintf("DRIFT %s %s host=%s registry=%s", d.Service, d.Image, d.Local, d.Remote)
}
```

Findings do not change the exit code (advisory; the command still fails on preflight/validation errors as before). With no findings print `images: all digests match`.

- [ ] **Step 4: Run tests** — `go test ./cmd/ -v` → PASS.

- [ ] **Step 5: Update README + commit**

README Usage block: `dockrail check --images     # also report tag-vs-registry digest drift (read-only)`.

```bash
go test ./... && go vet ./... && gofmt -l .
git add cmd/ README.md
git commit -m "feat(check): --images digest drift report"
```

---

## Part 4 — Secrets provider adapters (env + Infisical)

A `secrets` package with a `Provider` interface; the current env-reading logic becomes the `env` provider, and an `infisical` provider fetches via Infisical's REST API using stdlib HTTP (universal-auth login → raw secrets list, multi-path last-wins like composeflux). Delivery is untouched: whatever the provider returns still goes through `writeSecretsFile` (D8). **Bitwarden deferred:** its Go SDK is cgo (conflicts with D1); revisit if a pure-Go client appears.

### Task 9: `secrets` package with `env` provider

**Files:**
- Create: `secrets/secrets.go` (interface + factory + env provider)
- Test: `secrets/secrets_test.go`
- Modify: `config/config.go` (`Secrets` struct + validation), `engine/engine.go` (`runServices` swap), `engine/secrets.go` (remove `collectSecrets`, keep `writeSecretsFile`)
- Test: `config/config_test.go`, `engine/secrets_test.go` (move collectSecrets cases to the new package)

**Interfaces:**
- Produces:

```go
package secrets
type Provider interface {
	// Fetch returns a value for every requested name; any missing or empty
	// name is an error (same contract collectSecrets had).
	Fetch(ctx context.Context, names []string) (map[string]string, error)
}
func New(provider string) (Provider, error) // "" or "env" → Env{}; "infisical" → NewInfisical() (Task 10); anything else → error
type Env struct{}
```

- Config gains `secrets.provider: env|infisical` (default `env`); `from_env` keeps its name and remains "the list of required secret names" for every provider (documented; renaming it would break existing deploy.yml files).

- [ ] **Step 1: Write the failing tests**

`secrets/secrets_test.go` — port the existing `collectSecrets` tests (present/missing/empty env var) against `Env{}.Fetch`, plus:

```go
func TestNew_UnknownProvider(t *testing.T) {
	if _, err := New("vault"); err == nil {
		t.Fatal("want error for unknown provider")
	}
}
func TestNew_DefaultIsEnv(t *testing.T) {
	p, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(Env); !ok {
		t.Fatalf("want Env provider, got %T", p)
	}
}
```

`config/config_test.go` — a config with `secrets: {provider: bogus}` fails validation with a message naming the allowed values; `provider: infisical` passes parse.

- [ ] **Step 2: Run to verify failure** — `go test ./secrets/ ./config/ -v` → FAIL.

- [ ] **Step 3: Implement**

`secrets/secrets.go`:

```go
// Package secrets resolves secret values from a configured provider. The
// engine remains responsible for delivery (mode-600 env_file on the target,
// decision D8); providers only fetch values into memory.
package secrets

import (
	"context"
	"fmt"
	"os"
)

type Provider interface {
	Fetch(ctx context.Context, names []string) (map[string]string, error)
}

func New(provider string) (Provider, error) {
	switch provider {
	case "", "env":
		return Env{}, nil
	case "infisical":
		return NewInfisical()
	default:
		return nil, fmt.Errorf("secrets.provider %q: must be env or infisical", provider)
	}
}

// Env reads each name from dockrail's own environment (invoking shell / CI
// job) — the v1 behavior, now as the default provider.
type Env struct{}

func (Env) Fetch(_ context.Context, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, n := range names {
		v, ok := os.LookupEnv(n)
		if !ok || v == "" {
			return nil, fmt.Errorf("required secret %q is not set in dockrail's environment", n)
		}
		out[n] = v
	}
	return out, nil
}
```

(Until Task 10 lands, stub `func NewInfisical() (Provider, error) { return nil, fmt.Errorf("infisical provider: not yet implemented") }` in this file so `New` compiles; Task 10 replaces it.)

`config/config.go`:

```go
type Secrets struct {
	Provider string   `yaml:"provider"` // ""|env|infisical
	FromEnv  []string `yaml:"from_env"` // required secret names, any provider
}
```

with validation `provider` ∈ {"", "env", "infisical"}.

`engine/engine.go` `runServices`: replace `collectSecrets(e.Cfg.Secrets.FromEnv)` with:

```go
prov, err := secrets.New(e.Cfg.Secrets.Provider)
if err != nil {
	return e.recordFailure(ctx, failTag, "secrets", err)
}
vals, err := prov.Fetch(ctx, e.Cfg.Secrets.FromEnv)
if err != nil {
	return e.recordFailure(ctx, failTag, "secrets", err)
}
```

Delete `collectSecrets` from `engine/secrets.go` and its direct tests (now covered in the secrets package); keep `writeSecretsFile` untouched.

- [ ] **Step 4: Run tests** — `go test ./... -v` → PASS (engine deploy tests unaffected: default provider preserves behavior exactly).

- [ ] **Step 5: Commit**

```bash
go test ./... && go vet ./... && gofmt -l .
git add secrets/ config/ engine/
git commit -m "feat(secrets): provider interface with env as default provider"
```

### Task 10: Infisical provider (stdlib HTTP)

**Files:**
- Create: `secrets/infisical.go`
- Test: `secrets/infisical_test.go` (httptest.Server)

**Interfaces:**
- Produces: `NewInfisical() (Provider, error)` — reads `INFISICAL_CLIENT_ID`, `INFISICAL_CLIENT_SECRET`, `INFISICAL_PROJECT_ID`, `INFISICAL_ENVIRONMENT` (all required → error naming the missing var), `INFISICAL_SITE_URL` (default `https://app.infisical.com`), `INFISICAL_SECRET_PATH` (default `/`; comma-separated paths, later paths win on key collision — composeflux semantics). Credentials come from dockrail's environment, never deploy.yml.
- API calls (verify against current Infisical docs at implementation time; these are the documented v3 endpoints):
  1. `POST {site}/api/v1/auth/universal-auth/login` JSON `{"clientId","clientSecret"}` → `{"accessToken"}`
  2. Per path: `GET {site}/api/v3/secrets/raw?workspaceId={project}&environment={env}&secretPath={path}` with `Authorization: Bearer` → `{"secrets":[{"secretKey","secretValue"}]}`

- [ ] **Step 1: Write the failing tests**

```go
package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func infisicalServer(t *testing.T, perPath map[string]map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/universal-auth/login":
			json.NewEncoder(w).Encode(map[string]string{"accessToken": "tok"})
		case "/api/v3/secrets/raw":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var list []map[string]string
			for k, v := range perPath[r.URL.Query().Get("secretPath")] {
				list = append(list, map[string]string{"secretKey": k, "secretValue": v})
			}
			json.NewEncoder(w).Encode(map[string]any{"secrets": list})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func setInfisicalEnv(t *testing.T, site, paths string) {
	t.Helper()
	t.Setenv("INFISICAL_CLIENT_ID", "cid")
	t.Setenv("INFISICAL_CLIENT_SECRET", "cs")
	t.Setenv("INFISICAL_PROJECT_ID", "proj")
	t.Setenv("INFISICAL_ENVIRONMENT", "prod")
	t.Setenv("INFISICAL_SITE_URL", site)
	t.Setenv("INFISICAL_SECRET_PATH", paths)
}

func TestInfisical_FetchAndLastPathWins(t *testing.T) {
	srv := infisicalServer(t, map[string]map[string]string{
		"/":     {"DB_URL": "root-value", "API_KEY": "k1"},
		"/apps": {"DB_URL": "apps-value"},
	})
	defer srv.Close()
	setInfisicalEnv(t, srv.URL, "/,/apps")
	p, err := NewInfisical()
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Fetch(context.Background(), []string{"DB_URL", "API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if got["DB_URL"] != "apps-value" || got["API_KEY"] != "k1" {
		t.Fatalf("wrong values: %v", got)
	}
}

func TestInfisical_MissingNameIsError(t *testing.T) {
	srv := infisicalServer(t, map[string]map[string]string{"/": {}})
	defer srv.Close()
	setInfisicalEnv(t, srv.URL, "/")
	p, _ := NewInfisical()
	if _, err := p.Fetch(context.Background(), []string{"NOPE"}); err == nil {
		t.Fatal("want error for missing secret name")
	}
}

func TestNewInfisical_MissingCredentials(t *testing.T) {
	t.Setenv("INFISICAL_CLIENT_ID", "")
	if _, err := NewInfisical(); err == nil {
		t.Fatal("want error when INFISICAL_CLIENT_ID unset")
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./secrets/ -run TestInfisical -v` → FAIL (stub `NewInfisical` errors unconditionally).

- [ ] **Step 3: Implement `secrets/infisical.go`** (replaces the Task 9 stub)

```go
package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Infisical fetches secrets from Infisical via its REST API using machine
// identity (universal auth). Configured entirely from dockrail's own
// environment — credentials never appear in deploy.yml. stdlib HTTP only:
// no SDK dependency, keeps D1's static-binary/cross-compile guarantee.
type Infisical struct {
	site, clientID, clientSecret, projectID, environment string
	paths                                                []string
	httpc                                                *http.Client
}

func NewInfisical() (Provider, error) {
	required := map[string]string{
		"INFISICAL_CLIENT_ID":     os.Getenv("INFISICAL_CLIENT_ID"),
		"INFISICAL_CLIENT_SECRET": os.Getenv("INFISICAL_CLIENT_SECRET"),
		"INFISICAL_PROJECT_ID":    os.Getenv("INFISICAL_PROJECT_ID"),
		"INFISICAL_ENVIRONMENT":   os.Getenv("INFISICAL_ENVIRONMENT"),
	}
	for name, v := range required {
		if v == "" {
			return nil, fmt.Errorf("infisical provider: %s is not set in dockrail's environment", name)
		}
	}
	site := os.Getenv("INFISICAL_SITE_URL")
	if site == "" {
		site = "https://app.infisical.com"
	}
	pathSpec := os.Getenv("INFISICAL_SECRET_PATH")
	if pathSpec == "" {
		pathSpec = "/"
	}
	var paths []string
	for _, p := range strings.Split(pathSpec, ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return &Infisical{
		site:         strings.TrimRight(site, "/"),
		clientID:     required["INFISICAL_CLIENT_ID"],
		clientSecret: required["INFISICAL_CLIENT_SECRET"],
		projectID:    required["INFISICAL_PROJECT_ID"],
		environment:  required["INFISICAL_ENVIRONMENT"],
		paths:        paths,
		httpc:        &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (i *Infisical) Fetch(ctx context.Context, names []string) (map[string]string, error) {
	token, err := i.login(ctx)
	if err != nil {
		return nil, fmt.Errorf("infisical login: %w", err)
	}
	all := map[string]string{} // later paths overwrite earlier ones (last wins)
	for _, p := range i.paths {
		kv, err := i.listPath(ctx, token, p)
		if err != nil {
			return nil, fmt.Errorf("infisical path %s: %w", p, err)
		}
		for k, v := range kv {
			all[k] = v
		}
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		v, ok := all[n]
		if !ok || v == "" {
			return nil, fmt.Errorf("required secret %q not found in infisical (env %s, paths %s)", n, i.environment, strings.Join(i.paths, ","))
		}
		out[n] = v
	}
	return out, nil
}

func (i *Infisical) login(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{"clientId": i.clientID, "clientSecret": i.clientSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.site+"/api/v1/auth/universal-auth/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var resp struct {
		AccessToken string `json:"accessToken"`
	}
	if err := i.do(req, &resp); err != nil {
		return "", err
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("empty access token")
	}
	return resp.AccessToken, nil
}

func (i *Infisical) listPath(ctx context.Context, token, path string) (map[string]string, error) {
	q := url.Values{"workspaceId": {i.projectID}, "environment": {i.environment}, "secretPath": {path}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.site+"/api/v3/secrets/raw?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	var resp struct {
		Secrets []struct {
			SecretKey   string `json:"secretKey"`
			SecretValue string `json:"secretValue"`
		} `json:"secrets"`
	}
	if err := i.do(req, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(resp.Secrets))
	for _, s := range resp.Secrets {
		out[s.SecretKey] = s.SecretValue
	}
	return out, nil
}

func (i *Infisical) do(req *http.Request, into any) error {
	res, err := i.httpc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		// never echo the response body — it could restate credentials
		return fmt.Errorf("%s: HTTP %d", req.URL.Path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(into)
}
```

- [ ] **Step 4: Run tests** — `go test ./secrets/ -v` → PASS.

- [ ] **Step 5: Update README + commit**

README "Secrets & private registries": add a paragraph — `secrets.provider: infisical` fetches the `from_env`-listed names from Infisical (machine identity via `INFISICAL_*` env vars; `INFISICAL_SECRET_PATH` accepts comma-separated paths, last wins); delivery to the host is unchanged (mode-600 env-file). Note Bitwarden is deferred (cgo SDK vs static binary).

```bash
go test ./... && go vet ./... && gofmt -l .
git add secrets/ README.md
git commit -m "feat(secrets): infisical provider via REST (stdlib http, no SDK)"
```

---

## Deferred / consciously skipped

- **Bitwarden provider** — official Go SDK is cgo-backed; conflicts with D1. Revisit on a pure-Go client.
- **Auto-redeploy on image update** (composeflux `IMAGE_UPDATE_SCHEDULE`) — violates D10 posture; drift stays advisory.
- **Hashing secret values into the config hash** — excluded by design (see Task 6 doc comment); `--force` is the escape hatch.
- **`dockrail.deployed-at` label** — needs wall-clock at override render time and duplicates history's TS; skipped (YAGNI).
- **Daemon/pull GitOps mode** — see the fleet design decision V1 (pure planner, no control loop); tracked as a separate discussion, not this plan.

## Spec traceability

| Adoption item | Tasks |
|---|---|
| 1. Rendered-config hash + labels | 3, 4, 5 (fleet), 6 (v1 engine) |
| 2. Secrets adapters | 9, 10 |
| 3. Image-digest drift | 7, 8 |
| 4. compose-go client-side validation | 1, 2 |
