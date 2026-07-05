# dockrail Rollback + Status + Logs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the `rollback`, `status`, and `logs` subcommands (currently stubbed as `not implemented`), completing the operator-facing command surface from the spec.

**Architecture:** The deploy engine already owns the host-side state file (`~/.dockrail/<project>/state.json`) and a tag-parametrized recreate sequence. Rollback reuses that recreate path against the recorded `previous_tag`. Status reads state + queries the live container per service. Logs shells `docker compose logs`. All host interaction goes through the existing `connection.Connection` interface, so all three are unit-testable with the `Fake` connection asserting the emitted command sequence.

**Tech Stack:** Go 1.26, cobra, stdlib `testing`, existing `engine`/`connection`/`config` packages.

## Global Constraints

- Go 1.26 (latest stable); `GOTOOLCHAIN=local` in CI.
- No new third-party dependencies — stdlib + existing modules only.
- Host interaction only through `connection.Connection.Run(ctx, cmd) (string, error)`. Never shell out directly.
- Host state file path is `$HOME/.dockrail/<project>/state.json` with fields `previous_tag`, `current_tag`, `last_failure` (see `engine/state.go`). Do not change this schema.
- The recreate sequence order is fixed: `pull` → load state → `stop` old → `up -d --no-deps` new → readiness probe → finalize state. Rollback reuses this exact order.
- Every command reads `deploy.yml` by default; the path comes from the persistent `--config`/`-c` flag (default `deploy.yml`).
- Commands print human-readable step lines to stdout; errors return non-nil (cobra prints them, `SilenceUsage`/`SilenceErrors` are set on root).
- Match existing test style: table/sequence assertions over `connection.NewFake()` recorded commands.

---

### Task 1: Engine recreate refactor + `Engine.Rollback`

Extract the tag-parametrized recreate sequence out of `deployService` so both Deploy and Rollback share it, then add `Rollback`.

**Files:**
- Modify: `engine/engine.go`
- Test: `engine/engine_test.go` (add rollback tests alongside existing deploy tests)

**Interfaces:**
- Consumes: `connection.Connection`, `config.Config`/`config.Service`, `readiness.New`, `loadState`/`saveState`/`acquireLock` (all existing in `engine`).
- Produces:
  - `func (e *Engine) recreate(ctx context.Context, name string, svc config.Service, tag string) error` — the shared pull→stop→up→probe→finalize sequence for one service at an explicit image tag.
  - `func (e *Engine) Rollback(ctx context.Context) error` — restores `previous_tag` for every service, then swaps `current_tag`/`previous_tag` in host state.

- [ ] **Step 1: Write the failing tests**

Add to `engine/engine_test.go`. These assert (a) rollback with no previous tag errors without mutating, and (b) rollback re-runs the recreate sequence against the previous tag and swaps state. The `Fake` API (see `connection/fake.go`) is: `f := connection.NewFake()`, seed responses with `f.Stub(substr, stdout, err)` (first substring match wins), and inspect recorded commands via the `f.Commands` slice field. State reads contain the substring `state.json`.

```go
func TestRollback_NoPreviousTag(t *testing.T) {
	f := connection.NewFake()
	// state.json exists with only current_tag set (nothing to roll back to)
	f.Stub("state.json", `{"current_tag":"img:v2"}`, nil)
	cfg := &config.Config{
		Project: "proj", Compose: "docker-compose.yml",
		Services: map[string]config.Service{
			"web": {ImageTag: "img:v2",
				Readiness: config.Readiness{Type: "http", Path: "/h", Port: 8080, Timeout: "1s"},
				Cutover:   config.Cutover{Strategy: "recreate"}},
		},
	}
	e := &Engine{Conn: f, Cfg: cfg, Out: &bytes.Buffer{}}
	err := e.Rollback(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no previous") {
		t.Fatalf("want 'no previous' error, got %v", err)
	}
	// No up/stop should have been issued.
	for _, c := range f.Commands {
		if strings.Contains(c, "up -d") || strings.Contains(c, "stop web") {
			t.Fatalf("rollback mutated host despite no previous tag: %q", c)
		}
	}
}

func TestRollback_RestoresPreviousTag(t *testing.T) {
	f := connection.NewFake()
	f.Stub("state.json", `{"previous_tag":"img:v1","current_tag":"img:v2"}`, nil)
	// The http readiness probe and all other unstubbed commands return
	// ("", nil) from the Fake, i.e. success.
	cfg := &config.Config{
		Project: "proj", Compose: "docker-compose.yml",
		Services: map[string]config.Service{
			"web": {ImageTag: "img:v2",
				Readiness: config.Readiness{Type: "http", Path: "/h", Port: 8080, Timeout: "1s"},
				Cutover:   config.Cutover{Strategy: "recreate"}},
		},
	}
	e := &Engine{Conn: f, Cfg: cfg, Out: &bytes.Buffer{}}
	if err := e.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	cmds := strings.Join(f.Commands, "\n")
	// Recreate must target the PREVIOUS tag, not v2.
	if !strings.Contains(cmds, "TAG=img:v1 docker compose -f docker-compose.yml pull web") {
		t.Fatalf("expected pull of previous tag img:v1, got:\n%s", cmds)
	}
	if !strings.Contains(cmds, "TAG=img:v1 docker compose -f docker-compose.yml up -d --no-deps web") {
		t.Fatalf("expected up of previous tag img:v1, got:\n%s", cmds)
	}
	// State must be persisted with current_tag now img:v1 and previous_tag now img:v2.
	var saved string
	for _, c := range f.Commands {
		if strings.Contains(c, "state.json") && strings.Contains(c, "cat >") {
			saved = c
		}
	}
	if !strings.Contains(saved, `"current_tag":"img:v1"`) ||
		!strings.Contains(saved, `"previous_tag":"img:v2"`) {
		t.Fatalf("state not swapped after rollback, got: %q", saved)
	}
}
```

> **Note for implementer:** `bytes` is already imported in `engine_test.go`. The state-read stub uses substring `state.json`; because `Stub` is first-match-wins, keep it as the only stub matching that substring. Read `connection/fake.go` and the existing `engineFixture`/deploy tests before writing.

- [ ] **Step 2: Run tests to verify they fail**

Run: `GOTOOLCHAIN=auto go test ./engine/ -run Rollback -v`
Expected: FAIL — `e.Rollback` undefined.

- [ ] **Step 3: Refactor recreate and implement Rollback**

In `engine/engine.go`, replace the body of `deployService` with a thin wrapper over a new `recreate` method, and add `Rollback`. The `recreate` method is the current `deployService` body with `svc.ImageTag` replaced by the `tag` parameter throughout:

```go
func (e *Engine) deployService(ctx context.Context, name string, svc config.Service) error {
	return e.recreate(ctx, name, svc, svc.ImageTag)
}

// recreate runs the fixed pull→stop→up→probe→finalize sequence for one
// service at an explicit image tag. Deploy uses the service's configured
// tag; Rollback uses the recorded previous tag.
func (e *Engine) recreate(ctx context.Context, name string, svc config.Service, tag string) error {
	if svc.Cutover.Strategy != "recreate" {
		return fmt.Errorf("cutover strategy %q not implemented yet", svc.Cutover.Strategy)
	}
	prober, err := readiness.New(svc.Readiness)
	if err != nil {
		return err
	}
	compose := fmt.Sprintf("TAG=%s docker compose -f %s", tag, e.Cfg.Compose)

	e.logf("step pull: %s tag %s", name, tag)
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("%s pull %s", compose, name)); err != nil {
		return fmt.Errorf("pull: %w", err)
	}

	e.logf("step record-anchor")
	st, err := loadState(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}

	e.logf("step recreate: stop old + start new")
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("%s stop %s", compose, name)); err != nil {
		return fmt.Errorf("stop old: %w", err)
	}
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("%s up -d --no-deps %s", compose, name)); err != nil {
		return fmt.Errorf("start new: %w", err)
	}

	e.logf("step readiness")
	if err := prober.Probe(ctx, e.Conn); err != nil {
		st.LastFailure = fmt.Sprintf("deploy %s tag %s: %v", name, tag, err)
		_ = saveState(ctx, e.Conn, e.Cfg.Project, st)
		return err
	}

	e.logf("step finalize")
	st.PreviousTag, st.CurrentTag, st.LastFailure = st.CurrentTag, tag, ""
	if err := saveState(ctx, e.Conn, e.Cfg.Project, st); err != nil {
		return err
	}
	if _, err := e.Conn.Run(ctx, "docker image prune -f"); err != nil {
		e.logf("warn: prune failed: %v", err)
	}
	e.logf("deployed %s tag %s", name, tag)
	return nil
}

// Rollback restores the previously running image tag recorded in host state,
// re-running the recreate sequence for every service against that tag. It
// takes the deploy lock for its duration. Because host state records a single
// project-level tag pair, all services are restored to the same previous tag.
func (e *Engine) Rollback(ctx context.Context) error {
	e.logf("step lock")
	release, err := acquireLock(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	defer release()

	st, err := loadState(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	if st.PreviousTag == "" {
		return fmt.Errorf("no previous tag recorded for project %q; nothing to roll back to", e.Cfg.Project)
	}
	e.logf("step rollback: restoring tag %s", st.PreviousTag)
	for name, svc := range e.Cfg.Services {
		if err := e.recreate(ctx, name, svc, st.PreviousTag); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOTOOLCHAIN=auto go test ./engine/ -v`
Expected: PASS (existing deploy tests + new rollback tests).

- [ ] **Step 5: Commit**

```bash
git add engine/engine.go engine/engine_test.go
git commit -m "feat(engine): add Rollback via shared recreate sequence"
```

---

### Task 2: `rollback` command wiring

Replace the `rollback` stub in the root command loop with a real command that constructs the engine and calls `Rollback`.

**Files:**
- Modify: `cmd/root.go` (remove `rollback` from the stub loop), `cmd/deploy.go` or a new `cmd/rollback.go` (add `newRollbackCmd`)
- Test: `cmd/rollback_test.go`

**Interfaces:**
- Consumes: `config.Load`, `connection.New`, `engine.Engine{Conn,Cfg,Out}.Rollback`.
- Produces: `func newRollbackCmd() *cobra.Command`.

- [ ] **Step 1: Write the failing test**

Create `cmd/rollback_test.go`. Mirror the existing `cmd/deploy_test.go` structure (how it builds a temp `deploy.yml`, sets `--config`, and captures output). Assert that `rollback` on a config whose host state has a previous tag runs without error and that the command is registered (not the stub).

```go
func TestRollbackCommand_Registered(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"rollback"})
	if err != nil {
		t.Fatalf("find rollback: %v", err)
	}
	if cmd.RunE == nil {
		t.Fatal("rollback has no RunE")
	}
	// Executing the stub returned an error containing "not implemented";
	// the real command must not be the stub.
	if cmd.Short == "rollback" && cmd.Long == "" && cmd.Use == "rollback" {
		// Cannot distinguish by metadata alone; assert via behavior below.
	}
}
```

> **Note for implementer:** The stub was an anonymous `&cobra.Command{}` created in a loop in `cmd/root.go` returning `fmt.Errorf("%s: not implemented", name)`. After wiring, `rollback` must be added via `newRollbackCmd()` like `newDeployCmd()`/`newCheckCmd()`. If `cmd/deploy_test.go` already has a pattern for exercising a command end-to-end over a fake/local connection, extend that pattern here for a behavioral assertion instead of relying on metadata. Read `cmd/deploy.go` and `cmd/deploy_test.go` first and match their approach for building the engine and connection.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./cmd/ -run Rollback -v`
Expected: FAIL — `rollback` still resolves to the stub / behavior mismatch.

- [ ] **Step 3: Implement `newRollbackCmd` and wire it**

In `cmd/root.go`, change the stub loop from `{"rollback", "status", "logs"}` to `{"status", "logs"}` and add `root.AddCommand(newRollbackCmd())`. Add `newRollbackCmd` (place in a new `cmd/rollback.go`), mirroring `newDeployCmd`'s config-load + connection + engine construction:

```go
func newRollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback",
		Short: "restore the previously deployed image tag",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			conn := connection.New(cfg.Target)
			e := &engine.Engine{Conn: conn, Cfg: cfg, Out: cmd.OutOrStdout()}
			return e.Rollback(cmd.Context())
		},
	}
}
```

> Match `newDeployCmd`'s exact construction (imports, how it builds `connection.New(cfg.Target)`, how it passes `Out`). Read `cmd/deploy.go` and copy its shape.

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOTOOLCHAIN=auto go test ./cmd/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/root.go cmd/rollback.go cmd/rollback_test.go
git commit -m "feat(cmd): wire rollback command"
```

---

### Task 3: `Engine.Status` + `status` command

Report, per service: the running image tag (from `docker compose ps`/image inspect), whether it is up, plus the state file's `current_tag`/`previous_tag`/`last_failure`.

**Files:**
- Modify: `engine/engine.go` (add `Status`), `cmd/root.go` (remove `status` from stub loop)
- Create: `cmd/status.go`
- Test: `engine/status_test.go`, `cmd/status_test.go`

**Interfaces:**
- Consumes: `loadState`, `connection.Connection`, `config.Config`.
- Produces:
  - `type ServiceStatus struct { Name, RunningTag string; Up bool }`
  - `type StatusReport struct { CurrentTag, PreviousTag, LastFailure string; Services []ServiceStatus }`
  - `func (e *Engine) Status(ctx context.Context) (StatusReport, error)`
  - `func newStatusCmd() *cobra.Command`

- [ ] **Step 1: Write the failing test**

Create `engine/status_test.go`. Seed the state read and the per-service running-tag query on the `Fake` via `Stub(substr, stdout, err)`, then assert the report fields. Stub order matters — `Stub` is first-match-wins by substring, so register the more specific `ps -q web` and `inspect` stubs and ensure they do not also match `state.json`.

```go
func TestStatus_ReportsStateAndRunningTag(t *testing.T) {
	f := connection.NewFake()
	f.Stub("state.json", `{"previous_tag":"img:v1","current_tag":"img:v2","last_failure":"boom"}`, nil)
	// Running-container id query for service "web".
	f.Stub("ps -q web", "container123\n", nil)
	// Image inspect for that container id.
	f.Stub("inspect", "img:v2\n", nil)
	cfg := &config.Config{
		Project: "proj", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {ImageTag: "img:v2",
			Readiness: config.Readiness{Type: "http"},
			Cutover:   config.Cutover{Strategy: "recreate"}}},
	}
	e := &Engine{Conn: f, Cfg: cfg, Out: &bytes.Buffer{}}
	rep, err := e.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if rep.CurrentTag != "img:v2" || rep.PreviousTag != "img:v1" || rep.LastFailure != "boom" {
		t.Fatalf("state fields wrong: %+v", rep)
	}
	if len(rep.Services) != 1 || rep.Services[0].Name != "web" ||
		rep.Services[0].RunningTag != "img:v2" || !rep.Services[0].Up {
		t.Fatalf("service status wrong: %+v", rep.Services)
	}
}
```

> **Note for implementer:** Decide the exact two-step query (`ps -q` → `docker inspect` image) or a single `docker compose ps --format` call, then make the test assert the exact commands you emit. If `ps -q` returns empty, mark the service `Up=false` with an empty `RunningTag` — add a second test case for that. Keep the query read-only.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./engine/ -run Status -v`
Expected: FAIL — `e.Status` undefined.

- [ ] **Step 3: Implement `Status`**

Add the types and method to `engine/engine.go` (or a new `engine/status.go`). Read state via `loadState`; for each service, query the running container id and inspect its image; assemble `StatusReport`. Sort services by name for deterministic output.

```go
type ServiceStatus struct {
	Name       string
	RunningTag string
	Up         bool
}

type StatusReport struct {
	CurrentTag  string
	PreviousTag string
	LastFailure string
	Services    []ServiceStatus
}

func (e *Engine) Status(ctx context.Context) (StatusReport, error) {
	st, err := loadState(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return StatusReport{}, err
	}
	rep := StatusReport{
		CurrentTag:  st.CurrentTag,
		PreviousTag: st.PreviousTag,
		LastFailure: st.LastFailure,
	}
	names := make([]string, 0, len(e.Cfg.Services))
	for name := range e.Cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ss := ServiceStatus{Name: name}
		cid, err := e.Conn.Run(ctx, fmt.Sprintf(
			"docker compose -f %s ps -q %s", e.Cfg.Compose, name))
		if err != nil {
			return StatusReport{}, fmt.Errorf("status %s: %w", name, err)
		}
		cid = strings.TrimSpace(cid)
		if cid != "" {
			ss.Up = true
			img, err := e.Conn.Run(ctx, fmt.Sprintf(
				"docker inspect --format '{{.Config.Image}}' %s", cid))
			if err != nil {
				return StatusReport{}, fmt.Errorf("status %s inspect: %w", name, err)
			}
			ss.RunningTag = strings.TrimSpace(img)
		}
		rep.Services = append(rep.Services, ss)
	}
	return rep, nil
}
```

Add `"sort"` and `"strings"` to the `engine` imports if not present.

- [ ] **Step 4: Run test to verify it passes**

Run: `GOTOOLCHAIN=auto go test ./engine/ -run Status -v`
Expected: PASS. Add the `ps -q` empty → `Up=false` case and re-run.

- [ ] **Step 5: Wire `status` command**

In `cmd/root.go` remove `status` from the stub loop and `root.AddCommand(newStatusCmd())`. Create `cmd/status.go`:

```go
func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "show deployed and running image tags per service",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			conn := connection.New(cfg.Target)
			e := &engine.Engine{Conn: conn, Cfg: cfg, Out: cmd.OutOrStdout()}
			rep, err := e.Status(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "current_tag:  %s\n", rep.CurrentTag)
			fmt.Fprintf(out, "previous_tag: %s\n", rep.PreviousTag)
			if rep.LastFailure != "" {
				fmt.Fprintf(out, "last_failure: %s\n", rep.LastFailure)
			}
			for _, s := range rep.Services {
				state := "down"
				if s.Up {
					state = "up"
				}
				fmt.Fprintf(out, "  %s: %s (%s)\n", s.Name, s.RunningTag, state)
			}
			return nil
		},
	}
}
```

- [ ] **Step 6: Add a `cmd/status_test.go` smoke test, run cmd tests**

Mirror `cmd/deploy_test.go` for building a temp config; assert `status` is registered with a non-nil `RunE` and runs against a fake/local connection without panicking. Then:

Run: `GOTOOLCHAIN=auto go test ./engine/ ./cmd/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add engine/ cmd/root.go cmd/status.go cmd/status_test.go
git commit -m "feat: add status command (running tag + state per service)"
```

---

### Task 4: `logs` command

Tail service logs via `docker compose logs`.

**Files:**
- Modify: `cmd/root.go` (remove `logs` from stub loop — loop is now empty, delete it)
- Create: `cmd/logs.go`
- Test: `cmd/logs_test.go`

**Interfaces:**
- Consumes: `config.Load`, `connection.New`, `connection.Connection`.
- Produces: `func newLogsCmd() *cobra.Command` accepting `logs <service> [--tail N] [-f]`.

- [ ] **Step 1: Write the failing test**

Create `cmd/logs_test.go`. Assert the command requires exactly one service arg and emits the expected `docker compose logs` command. If exercising end-to-end needs a connection seam the cmd currently lacks (it builds `connection.New` internally), assert instead on argument validation + registration, matching how `cmd/deploy_test.go` handles the same limitation.

```go
func TestLogsCommand_RequiresService(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"logs"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when no service given")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./cmd/ -run Logs -v`
Expected: FAIL — stub accepts any args / no arg validation.

- [ ] **Step 3: Implement `newLogsCmd` and wire it**

Remove the now-empty stub loop from `cmd/root.go` and add `root.AddCommand(newLogsCmd())`. Create `cmd/logs.go`:

```go
func newLogsCmd() *cobra.Command {
	var tail int
	var follow bool
	c := &cobra.Command{
		Use:   "logs <service>",
		Short: "show logs for a service on the target host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			service := args[0]
			if _, ok := cfg.Services[service]; !ok {
				return fmt.Errorf("unknown service %q", service)
			}
			conn := connection.New(cfg.Target)
			logsCmd := fmt.Sprintf("docker compose -f %s logs --tail %d", cfg.Compose, tail)
			if follow {
				logsCmd += " -f"
			}
			logsCmd += " " + service
			out, err := conn.Run(cmd.Context(), logsCmd)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}
	c.Flags().IntVar(&tail, "tail", 100, "number of trailing log lines to show")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "follow log output")
	return c
}
```

> **Note for implementer:** `-f` streaming does not fit the request/response `Connection.Run` seam (which returns after the command exits). For v1, `-f` is accepted but the run returns whatever the transport yields; document this limitation in the command's `Long` help rather than building a streaming path. Do not add a streaming API in this task.

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOTOOLCHAIN=auto go test ./cmd/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/root.go cmd/logs.go cmd/logs_test.go
git commit -m "feat(cmd): add logs command"
```

---

### Task 5: Docs + full gate

Update README/spec status notes to reflect that rollback/status/logs are now implemented, and run the full gate.

**Files:**
- Modify: `README.md` (status blurb), `docs/specs/2026-07-05-dockrail-design.md` (mark build-order item 3 done if it tracks status)

- [ ] **Step 1: Update README status paragraph**

Change the README `> **Status:**` block so `rollback` / `status` / `logs` are listed as implemented, leaving `proxy` cutover and `gpu` placement as the remaining stubs.

- [ ] **Step 2: Run the full gate**

Run:
```bash
GOTOOLCHAIN=auto gofmt -l .        # expect: no output
GOTOOLCHAIN=auto go vet ./...      # expect: no issues
GOTOOLCHAIN=auto go test ./...     # expect: all packages ok
GOTOOLCHAIN=auto go build ./...    # expect: clean
```
Expected: all clean.

- [ ] **Step 3: Commit**

```bash
git add README.md docs/specs/2026-07-05-dockrail-design.md
git commit -m "docs: mark rollback/status/logs implemented"
```

---

## Self-Review Notes

- **Spec coverage:** rollback (spec §Rollback, build-order 3), status (§Status/logs), logs (§Status/logs) all have tasks. Failed-attempt reporting is covered by `status` surfacing `last_failure`.
- **Type consistency:** `recreate(ctx, name, svc, tag)`, `Rollback(ctx)`, `Status(ctx) (StatusReport, error)`, `StatusReport`/`ServiceStatus` used consistently across tasks. Command constructors follow the existing `newDeployCmd`/`newCheckCmd` naming.
- **Known limitation (intentional, documented):** host state stores one project-level tag pair, so rollback restores all services to the same `previous_tag`. Per-service tag history is out of scope for this slice; note it, do not build it.
- **Out of scope:** `proxy` cutover, `gpu` placement, notify/Telegram, true streaming `logs -f`.
