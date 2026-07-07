# Deploy Lock (`--lock-wait`, metadata, `lock` command) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Holder metadata on the deploy lock, a `--lock-wait` flag on `deploy`/`rollback`, a `dockrail lock status|acquire|release` command, and a `lock-wait` action input.

**Architecture:** Spec: `docs/specs/2026-07-07-deploy-lock-design.md`. The existing `mkdir`-based lock in `engine/lock.go` stays the locking primitive; this plan adds an advisory `info.json` inside the lock dir, a polling wrapper `acquireLockWait`, exported `LockStatus`/`LockAcquire`/`LockRelease` engine methods, and cmd/action wiring. All engine work is tested against `connection.Fake` (records commands, substring-stubbed responses).

**Tech Stack:** Go 1.26.4, cobra, `connection.Fake` test pattern.

## Global Constraints

- Module path: `github.com/goodsmileduck/dockrail`.
- No TTL auto-expiry; `lock release` is the only stale-lock remedy.
- Metadata is advisory: its write failure must NOT fail lock acquisition.
- `--lock-wait` default 0 = current fail-fast behavior (backward compatible).
- `lock status` exit codes: 0 free, 1 held, 2 connection/config error.
- Host paths come from `projectDir(project)` (= `$HOME/.dockrail/<project>`) in `engine/history.go`; never hardcode another layout.
- Reuse `performer()` (engine/history.go) for the `by` field.
- Public repo: example.com placeholders only in docs; no `§` symbol in docs.
- `gofmt` clean; run `go vet ./... && go test ./...` before every commit.

---

### Task 1: Lock metadata + holder-aware collision error

**Files:**
- Modify: `engine/lock.go` (rewrite)
- Create: `engine/lock_test.go`
- Modify: `engine/engine.go:30-50,93-124` (pass tag to `acquireLock`; add `lockTag` helper)

**Interfaces:**
- Consumes: `projectDir(project string) string`, `performer() string` (engine/history.go), `connection.Connection`.
- Produces: `acquireLock(ctx context.Context, conn connection.Connection, project, tag string) (func(), error)` (signature gains `tag`); `lockDir(project string) string`; `lockInfoPath(project string) string`; `lockHolderDesc(ctx context.Context, conn connection.Connection, project string) string` (returns `held by <by> since <at> (deploying <tag>)` or `no holder metadata`); `lockTag(cfg *config.Config) string`. Task 2 wraps `acquireLock`; Task 3 uses `lockDir`/`lockInfoPath`/`lockHolderDesc`.

- [ ] **Step 1: Write the failing tests**

Create `engine/lock_test.go`:

```go
package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

func TestAcquireLockWritesMetadataAndReleaseCleansUp(t *testing.T) {
	f := connection.NewFake()
	release, err := acquireLock(context.Background(), f, "demo", "v42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "mkdir $HOME/.dockrail/demo/lock") {
		t.Error("lock dir not created")
	}
	// Metadata is shipped base64-encoded (same idiom as writeSecretsFile);
	// decode the payload and verify the fields.
	re := regexp.MustCompile(`printf %s (\S+) \| base64 -d > \$HOME/\.dockrail/demo/lock/info\.json`)
	m := re.FindStringSubmatch(joined)
	if m == nil {
		t.Fatal("no metadata write command issued")
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatalf("metadata payload is not base64: %v", err)
	}
	var li lockInfo
	if err := json.Unmarshal(raw, &li); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if li.Tag != "v42" || li.By == "" || li.AcquiredAt == "" {
		t.Errorf("bad metadata: %+v", li)
	}
	release()
	joined = strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "rm -f $HOME/.dockrail/demo/lock/info.json && rmdir $HOME/.dockrail/demo/lock") {
		t.Error("release must remove metadata then the lock dir")
	}
}

func TestAcquireLockMetadataWriteFailureDoesNotFailAcquisition(t *testing.T) {
	f := connection.NewFake()
	f.Stub("base64 -d > $HOME/.dockrail/demo/lock/info.json", "", errors.New("disk full"))
	release, err := acquireLock(context.Background(), f, "demo", "v42")
	if err != nil {
		t.Fatalf("metadata is advisory; acquisition must succeed: %v", err)
	}
	release()
}

func TestAcquireLockCollisionReportsHolder(t *testing.T) {
	f := connection.NewFake()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	f.Stub("cat $HOME/.dockrail/demo/lock/info.json",
		`{"acquired_at":"2026-07-07T10:00:00Z","tag":"v41","by":"ci@runner"}`, nil)
	_, err := acquireLock(context.Background(), f, "demo", "v42")
	if err == nil {
		t.Fatal("want collision error")
	}
	for _, want := range []string{"held by ci@runner", "since 2026-07-07T10:00:00Z", "deploying v41"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestAcquireLockCollisionWithoutMetadata(t *testing.T) {
	f := connection.NewFake()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	f.Stub("cat $HOME/.dockrail/demo/lock/info.json", "", errors.New("No such file"))
	_, err := acquireLock(context.Background(), f, "demo", "v42")
	if err == nil || !strings.Contains(err.Error(), "no holder metadata") {
		t.Fatalf("want 'no holder metadata' in error, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./engine/ -run TestAcquireLock -v`
Expected: compile FAIL — `acquireLock` has the old 3-arg signature and `lockInfo` is undefined.

- [ ] **Step 3: Implement**

Rewrite `engine/lock.go`:

```go
package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goodsmileduck/dockrail/connection"
)

// lockInfo is the advisory holder metadata written inside the lock dir. The
// mkdir is the lock; this file only improves error messages and lock status.
// A crash between mkdir and the metadata write leaves a held lock without it
// (reported as "no holder metadata") — accepted, the remedy (lock release)
// is the same.
type lockInfo struct {
	AcquiredAt string `json:"acquired_at"`
	Tag        string `json:"tag,omitempty"`
	By         string `json:"by"`
}

func lockDir(project string) string      { return projectDir(project) + "/lock" }
func lockInfoPath(project string) string { return lockDir(project) + "/info.json" }

// lockHolderDesc describes who holds the lock, from the advisory metadata:
// "held by <by> since <acquired_at> (deploying <tag>)". Locks without
// readable metadata (pre-metadata dirs, crash before the write) degrade to
// "no holder metadata".
func lockHolderDesc(ctx context.Context, conn connection.Connection, project string) string {
	out, err := conn.Run(ctx, fmt.Sprintf("cat %s", lockInfoPath(project)))
	if err != nil {
		return "no holder metadata"
	}
	var li lockInfo
	if json.Unmarshal([]byte(out), &li) != nil || li.By == "" {
		return "no holder metadata"
	}
	desc := fmt.Sprintf("held by %s since %s", li.By, li.AcquiredAt)
	if li.Tag != "" {
		desc += fmt.Sprintf(" (deploying %s)", li.Tag)
	}
	return desc
}

// acquireLock takes the per-project deploy lock (atomic mkdir on the target).
// tag is recorded in the advisory metadata; empty when not deploying a tag
// (rollback before history is read, manual lock acquire). The returned func
// releases the lock.
func acquireLock(ctx context.Context, conn connection.Connection, project, tag string) (func(), error) {
	dir := lockDir(project)
	mk := fmt.Sprintf("mkdir -p %s && mkdir %s", projectDir(project), dir)
	if _, err := conn.Run(ctx, mk); err != nil {
		return nil, fmt.Errorf("another deploy appears to be running: lock %s %s: %w",
			dir, lockHolderDesc(ctx, conn, project), err)
	}
	// Advisory metadata, best effort: transported base64-encoded like
	// writeSecretsFile so the value never hits shell quoting.
	li := lockInfo{AcquiredAt: time.Now().UTC().Format(time.RFC3339), Tag: tag, By: performer()}
	b, _ := json.Marshal(li)
	enc := base64.StdEncoding.EncodeToString(b)
	if _, err := conn.Run(ctx, fmt.Sprintf("printf %%s %s | base64 -d > %s", enc, lockInfoPath(project))); err != nil {
		// The mkdir is the lock; a failed metadata write must not fail the deploy.
		_ = err
	}
	release := func() {
		_, _ = conn.Run(context.Background(),
			fmt.Sprintf("rm -f %s && rmdir %s", lockInfoPath(project), lockDir(project)))
	}
	return release, nil
}
```

Note: `performer()` already exists in `engine/history.go` and returns the
local `user@hostname` string — reuse it, do not add a new helper.

Update the three call sites in `engine/engine.go` (they currently call
`acquireLock(ctx, e.Conn, e.Cfg.Project)`):

- `Deploy` (line ~32): `acquireLock(ctx, e.Conn, e.Cfg.Project, lockTag(e.Cfg))`
- `Rollback` (line ~95) and `RollbackTo` (line ~120): `acquireLock(ctx, e.Conn, e.Cfg.Project, "")` (target tag not yet known at lock time).

Add the helper to `engine/lock.go`:

```go
// lockTag summarizes the tags Deploy is about to roll out, for the lock
// metadata: the distinct service image tags, sorted, comma-joined (services
// usually share one tag, so this is normally just that tag).
func lockTag(cfg *config.Config) string {
	seen := map[string]bool{}
	var tags []string
	for _, s := range cfg.Services {
		if !seen[s.ImageTag] {
			seen[s.ImageTag] = true
			tags = append(tags, s.ImageTag)
		}
	}
	sort.Strings(tags)
	return strings.Join(tags, ",")
}
```

(imports in lock.go grow: `sort`, `strings`, `github.com/goodsmileduck/dockrail/config`.)

- [ ] **Step 4: Run the full suite**

Run: `go vet ./... && go test ./...`
Expected: PASS. The pre-existing lock assertions in `engine/engine_test.go`
(~line 119: mkdir collision stops deploy; ~line 134: release runs `rmdir`)
must still pass — the release command still contains the `rmdir …/lock`
substring they grep for. If the collision test now fails because the error
text changed, the test greps for "lock" generically — verify, and if it
asserts the old exact message, update that assertion to the new message.

- [ ] **Step 5: Commit**

```bash
git add engine/lock.go engine/lock_test.go engine/engine.go engine/engine_test.go
git commit -m "feat(engine): lock holder metadata and holder-aware collision error"
```

---

### Task 2: `acquireLockWait` + `Engine.LockWait`

**Files:**
- Modify: `engine/lock.go` (append)
- Modify: `engine/lock_test.go` (append)
- Modify: `engine/engine.go:15-19` (add field), lock call sites in `Deploy`/`Rollback`/`RollbackTo`

**Interfaces:**
- Consumes: `acquireLock(ctx, conn, project, tag string) (func(), error)`, `lockHolderDesc(ctx, conn, project) string` (Task 1).
- Produces: `acquireLockWait(ctx context.Context, conn connection.Connection, project, tag string, wait time.Duration, out io.Writer) (func(), error)`; `Engine.LockWait time.Duration` field (zero = fail fast); package var `lockPollInterval = 5 * time.Second`. Task 3's cmd layer sets `LockWait`.

- [ ] **Step 1: Write the failing tests**

Append to `engine/lock_test.go`:

```go
// flakyLockConn fails the lock mkdir a fixed number of times, then delegates
// to the embedded Fake. Simulates a lock freeing up mid-wait.
type flakyLockConn struct {
	*connection.Fake
	failures int
}

func (c *flakyLockConn) Run(ctx context.Context, cmd string) (string, error) {
	if strings.Contains(cmd, "mkdir $HOME/.dockrail/demo/lock") && c.failures > 0 {
		c.failures--
		return "", errors.New("File exists")
	}
	return c.Fake.Run(ctx, cmd)
}

func fastPoll(t *testing.T) {
	t.Helper()
	old := lockPollInterval
	lockPollInterval = time.Millisecond
	t.Cleanup(func() { lockPollInterval = old })
}

func TestLockWaitAcquiresWhenLockFrees(t *testing.T) {
	fastPoll(t)
	c := &flakyLockConn{Fake: connection.NewFake(), failures: 2}
	var buf bytes.Buffer
	release, err := acquireLockWait(context.Background(), c, "demo", "v42", time.Second, &buf)
	if err != nil {
		t.Fatalf("lock freed during wait; want success, got %v", err)
	}
	release()
	if !strings.Contains(buf.String(), "waiting for deploy lock") {
		t.Errorf("first collision must print a waiting line, got %q", buf.String())
	}
}

func TestLockWaitTimesOutWithHolderError(t *testing.T) {
	fastPoll(t)
	f := connection.NewFake()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	f.Stub("cat $HOME/.dockrail/demo/lock/info.json",
		`{"acquired_at":"2026-07-07T10:00:00Z","tag":"v41","by":"ci@runner"}`, nil)
	var buf bytes.Buffer
	_, err := acquireLockWait(context.Background(), f, "demo", "v42", 5*time.Millisecond, &buf)
	if err == nil || !strings.Contains(err.Error(), "held by ci@runner") {
		t.Fatalf("want holder error on timeout, got %v", err)
	}
}

func TestLockWaitZeroFailsFast(t *testing.T) {
	f := connection.NewFake()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	var buf bytes.Buffer
	_, err := acquireLockWait(context.Background(), f, "demo", "v42", 0, &buf)
	if err == nil {
		t.Fatal("wait=0 must fail immediately")
	}
	if strings.Contains(buf.String(), "waiting") {
		t.Error("wait=0 must not print a waiting line")
	}
}

func TestLockWaitRespectsContextCancel(t *testing.T) {
	fastPoll(t)
	f := connection.NewFake()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	_, err := acquireLockWait(ctx, f, "demo", "v42", time.Minute, &buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
```

(imports in lock_test.go grow: `bytes`, `time`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./engine/ -run TestLockWait -v`
Expected: compile FAIL — `acquireLockWait` and `lockPollInterval` undefined.

- [ ] **Step 3: Implement**

Append to `engine/lock.go` (imports grow: `io`):

```go
// lockPollInterval is how often acquireLockWait retries. Package var so wait
// tests can run in milliseconds.
var lockPollInterval = 5 * time.Second

// acquireLockWait acquires the deploy lock, retrying for up to wait when it
// is held. wait <= 0 fails fast (acquireLock's behavior). The first collision
// prints one waiting line to out; retries are silent. On timeout the last
// holder-aware collision error is returned. Ctx cancellation aborts the wait.
func acquireLockWait(ctx context.Context, conn connection.Connection, project, tag string, wait time.Duration, out io.Writer) (func(), error) {
	release, err := acquireLock(ctx, conn, project, tag)
	if err == nil || wait <= 0 {
		return release, err
	}
	fmt.Fprintf(out, "waiting for deploy lock (%s)\n", lockHolderDesc(ctx, conn, project))
	deadline := time.Now().Add(wait)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPollInterval):
		}
		release, err = acquireLock(ctx, conn, project, tag)
		if err == nil {
			return release, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
	}
}
```

In `engine/engine.go`:

1. Add the field (with `time` import):

```go
type Engine struct {
	Conn connection.Connection
	Cfg  *config.Config
	Out  io.Writer
	// LockWait is how long Deploy/Rollback wait for the deploy lock before
	// giving up. Zero = fail fast (the default).
	LockWait time.Duration
}
```

2. Replace the three `acquireLock(...)` calls from Task 1 with:
   - `Deploy`: `acquireLockWait(ctx, e.Conn, e.Cfg.Project, lockTag(e.Cfg), e.LockWait, e.Out)`
   - `Rollback` / `RollbackTo`: `acquireLockWait(ctx, e.Conn, e.Cfg.Project, "", e.LockWait, e.Out)`

- [ ] **Step 4: Run the full suite**

Run: `go vet ./... && go test ./...`
Expected: PASS (existing engine tests construct Engine with zero LockWait → fail-fast, unchanged behavior).

- [ ] **Step 5: Commit**

```bash
git add engine/lock.go engine/lock_test.go engine/engine.go
git commit -m "feat(engine): acquireLockWait polling and Engine.LockWait"
```

---

### Task 3: `--lock-wait` flags + `dockrail lock` command

**Files:**
- Modify: `cmd/deploy.go`
- Modify: `cmd/rollback.go`
- Modify: `cmd/root.go` (register `newLockCmd()` in the existing `AddCommand` list)
- Modify: `engine/lock.go` (append exported methods), `engine/lock_test.go` (append)
- Create: `cmd/lock.go`
- Create: `cmd/lock_test.go`

**Interfaces:**
- Consumes: `acquireLock`/`acquireLockWait`/`lockDir`/`lockInfoPath`/`lockHolderDesc` (Tasks 1–2); `Engine.LockWait`.
- Produces: `(*Engine).LockStatus(ctx) (held bool, desc string, err error)`; `(*Engine).LockAcquire(ctx) error`; `(*Engine).LockRelease(ctx) (displacedDesc string, err error)`; cmd helpers `runLockStatus`, `runLockAcquire`, `runLockRelease` (same `(ctx, conn, cfg, out)` shape as `runStatus`). Task 4 documents the CLI surface.

- [ ] **Step 1: Write the failing engine tests**

Append to `engine/lock_test.go`:

```go
func TestLockStatusFree(t *testing.T) {
	f := connection.NewFake() // default output "" -> not "held"
	e := &Engine{Conn: f, Cfg: demoCfg(), Out: &bytes.Buffer{}}
	held, _, err := e.LockStatus(context.Background())
	if err != nil || held {
		t.Fatalf("want free, got held=%v err=%v", held, err)
	}
}

func TestLockStatusHeldWithMetadata(t *testing.T) {
	f := connection.NewFake()
	f.Stub("if test -d $HOME/.dockrail/demo/lock", "held", nil)
	f.Stub("cat $HOME/.dockrail/demo/lock/info.json",
		`{"acquired_at":"2026-07-07T10:00:00Z","tag":"v41","by":"ci@runner"}`, nil)
	e := &Engine{Conn: f, Cfg: demoCfg(), Out: &bytes.Buffer{}}
	held, desc, err := e.LockStatus(context.Background())
	if err != nil || !held {
		t.Fatalf("want held, got held=%v err=%v", held, err)
	}
	if !strings.Contains(desc, "held by ci@runner") {
		t.Errorf("desc = %q", desc)
	}
}

func TestLockReleaseWhenHeld(t *testing.T) {
	f := connection.NewFake()
	f.Stub("if test -d $HOME/.dockrail/demo/lock", "held", nil)
	e := &Engine{Conn: f, Cfg: demoCfg(), Out: &bytes.Buffer{}}
	desc, err := e.LockRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc == "" {
		t.Error("want displaced holder description")
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"),
		"rm -f $HOME/.dockrail/demo/lock/info.json && rmdir $HOME/.dockrail/demo/lock") {
		t.Error("release command not issued")
	}
}

func TestLockReleaseWhenFree(t *testing.T) {
	f := connection.NewFake()
	e := &Engine{Conn: f, Cfg: demoCfg(), Out: &bytes.Buffer{}}
	desc, err := e.LockRelease(context.Background())
	if err != nil || desc != "" {
		t.Fatalf("free lock: want empty desc and nil err, got %q %v", desc, err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "rmdir") {
		t.Error("must not rmdir a free lock")
	}
}
```

`demoCfg` — add this small helper at the top of `engine/lock_test.go` if no
equivalent exists in the engine test files (grep `engine/*_test.go` for an
existing minimal-config constructor first and reuse it if one exists):

```go
func demoCfg() *config.Config {
	return &config.Config{Project: "demo", Compose: "docker-compose.yml"}
}
```

(import `github.com/goodsmileduck/dockrail/config` in lock_test.go.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./engine/ -run TestLock -v`
Expected: compile FAIL — `LockStatus`/`LockRelease` undefined.

- [ ] **Step 3: Implement the engine methods**

Append to `engine/lock.go`:

```go
// LockStatus reports whether the deploy lock is held, with a holder
// description when it is. The probe command always exits 0 so a Run error
// means the connection itself failed, not "lock absent".
func (e *Engine) LockStatus(ctx context.Context) (bool, string, error) {
	out, err := e.Conn.Run(ctx,
		fmt.Sprintf("if test -d %s; then echo held; else echo free; fi", lockDir(e.Cfg.Project)))
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(out) != "held" {
		return false, "", nil
	}
	return true, lockHolderDesc(ctx, e.Conn, e.Cfg.Project), nil
}

// LockAcquire takes the deploy lock without releasing it — a manual freeze
// (e.g. before host maintenance). Cleared by LockRelease.
func (e *Engine) LockAcquire(ctx context.Context) error {
	_, err := acquireLock(ctx, e.Conn, e.Cfg.Project, "")
	return err
}

// LockRelease removes the deploy lock unconditionally and returns the
// displaced holder's description ("" if the lock was not held). It is the
// human override for stale locks; it performs no staleness check, so it can
// displace a live deploy — callers surface who was displaced.
func (e *Engine) LockRelease(ctx context.Context) (string, error) {
	held, desc, err := e.LockStatus(ctx)
	if err != nil {
		return "", err
	}
	if !held {
		return "", nil
	}
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("rm -f %s && rmdir %s",
		lockInfoPath(e.Cfg.Project), lockDir(e.Cfg.Project))); err != nil {
		return "", err
	}
	return desc, nil
}
```

Run: `go test ./engine/ -run TestLock -v` — expected: PASS.

- [ ] **Step 4: Write the failing cmd tests**

Create `cmd/lock_test.go`. First grep `cmd/status_test.go` for the existing
fake-based test pattern and mirror it. The tests:

```go
package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func lockTestCfg() *config.Config {
	return &config.Config{Project: "demo", Compose: "docker-compose.yml"}
}

func TestRunLockStatusFree(t *testing.T) {
	var buf bytes.Buffer
	held, err := runLockStatus(context.Background(), connection.NewFake(), lockTestCfg(), &buf)
	if err != nil || held {
		t.Fatalf("want free, got held=%v err=%v", held, err)
	}
	if !strings.Contains(buf.String(), "free") {
		t.Errorf("output %q", buf.String())
	}
}

func TestRunLockStatusHeld(t *testing.T) {
	f := connection.NewFake()
	f.Stub("if test -d $HOME/.dockrail/demo/lock", "held", nil)
	f.Stub("cat $HOME/.dockrail/demo/lock/info.json",
		`{"acquired_at":"2026-07-07T10:00:00Z","tag":"v41","by":"ci@runner"}`, nil)
	var buf bytes.Buffer
	held, err := runLockStatus(context.Background(), f, lockTestCfg(), &buf)
	if err != nil || !held {
		t.Fatalf("want held, got held=%v err=%v", held, err)
	}
	if !strings.Contains(buf.String(), "held by ci@runner") {
		t.Errorf("output %q", buf.String())
	}
}

func TestRunLockAcquireAndRelease(t *testing.T) {
	var buf bytes.Buffer
	f := connection.NewFake()
	if err := runLockAcquire(context.Background(), f, lockTestCfg(), &buf); err != nil {
		t.Fatal(err)
	}
	// release on a "held" host
	f2 := connection.NewFake()
	f2.Stub("if test -d $HOME/.dockrail/demo/lock", "held", nil)
	if err := runLockRelease(context.Background(), f2, lockTestCfg(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "released") {
		t.Errorf("output %q", buf.String())
	}
}

func TestRunLockReleaseWhenFree(t *testing.T) {
	var buf bytes.Buffer
	if err := runLockRelease(context.Background(), connection.NewFake(), lockTestCfg(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "lock is not held") {
		t.Errorf("output %q", buf.String())
	}
}
```

Run: `go test ./cmd/ -run TestRunLock -v` — expected: compile FAIL.

- [ ] **Step 5: Implement cmd/lock.go and flag wiring**

Create `cmd/lock.go`:

```go
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/engine"
)

func newLockCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lock",
		Short: "inspect or override the deploy lock on the target host",
	}
	c.AddCommand(newLockStatusCmd(), newLockAcquireCmd(), newLockReleaseCmd())
	return c
}

// loadConn is the shared prologue of the lock subcommands.
func loadConn(cmd *cobra.Command) (*config.Config, connection.Connection, error) {
	path, _ := cmd.Flags().GetString("config")
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	return cfg, connection.New(cfg.Target), nil
}

func newLockStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "show whether the deploy lock is held (exit 0 free, 1 held, 2 error)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, conn, err := loadConn(cmd)
			if err == nil {
				var held bool
				held, err = runLockStatus(cmd.Context(), conn, cfg, cmd.OutOrStdout())
				if err == nil {
					if held {
						os.Exit(1)
					}
					return nil
				}
			}
			// Exit 2 for config/connection errors so scripts can tell
			// "held" (1) apart from "could not answer" (2).
			cmd.PrintErrln("Error:", err)
			os.Exit(2)
			return nil // unreachable
		},
	}
}

func newLockAcquireCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "acquire",
		Short: "take the deploy lock manually (freeze deploys)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, conn, err := loadConn(cmd)
			if err != nil {
				return err
			}
			return runLockAcquire(cmd.Context(), conn, cfg, cmd.OutOrStdout())
		},
	}
}

func newLockReleaseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "release",
		Short: "remove the deploy lock unconditionally (human override for stale locks)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, conn, err := loadConn(cmd)
			if err != nil {
				return err
			}
			return runLockRelease(cmd.Context(), conn, cfg, cmd.OutOrStdout())
		},
	}
}

func runLockStatus(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer) (bool, error) {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	held, desc, err := e.LockStatus(ctx)
	if err != nil {
		return false, err
	}
	if held {
		fmt.Fprintln(out, desc)
	} else {
		fmt.Fprintln(out, "free")
	}
	return held, nil
}

func runLockAcquire(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	if err := e.LockAcquire(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "lock acquired; deploys are frozen until 'dockrail lock release'")
	return nil
}

func runLockRelease(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	desc, err := e.LockRelease(ctx)
	if err != nil {
		return err
	}
	if desc == "" {
		fmt.Fprintln(out, "lock is not held")
		return nil
	}
	fmt.Fprintf(out, "released (was %s)\n", desc)
	return nil
}
```

Register in `cmd/root.go`: add `newLockCmd()` to the existing
`root.AddCommand(...)` list.

Wire `--lock-wait` in `cmd/deploy.go`:

```go
// in newDeployCmd RunE, after dryRun:
lockWait, _ := cmd.Flags().GetDuration("lock-wait")
// ...
return runDeploy(cmd.Context(), conn, cfg, cmd.OutOrStdout(), dryRun, lockWait)

// flag registration next to dry-run:
c.Flags().Duration("lock-wait", 0, "wait up to this long for the deploy lock (e.g. 5m); 0 fails immediately")
```

and `runDeploy` gains the parameter, setting the field:

```go
func runDeploy(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer, dryRun bool, lockWait time.Duration) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out, LockWait: lockWait}
	...
```

(`time` import added; update `runDeploy` callers in `cmd/deploy_test.go` to pass `0`.)

Same shape in `cmd/rollback.go` — note it currently registers no flags, so
restructure like deploy: assign the command to a variable, add
`c.Flags().Duration("lock-wait", 0, ...)`, read it in RunE, and thread it
through `runRollback` into `engine.Engine{..., LockWait: lockWait}` (update
`cmd/rollback_test.go` callers to pass `0`).

- [ ] **Step 6: Run the full suite**

Run: `go vet ./... && go test ./...`
Expected: PASS. Also smoke: `go run . lock --help` lists status/acquire/release;
`go run . deploy --help` and `go run . rollback --help` show `--lock-wait`.

- [ ] **Step 7: Commit**

```bash
git add cmd/ engine/lock.go engine/lock_test.go
git commit -m "feat(cmd): lock status/acquire/release and --lock-wait on deploy/rollback"
```

---

### Task 4: Action `lock-wait` input + docs

**Files:**
- Modify: `action/action.yml`
- Modify: `docs/gitops.md`
- Modify: `README.md`
- Modify: `CLAUDE.md`

**Interfaces:**
- Consumes: `--lock-wait` flag (Task 3); action deploy step structure (args array + `extra-args`).
- Produces: nothing consumed by other tasks.

- [ ] **Step 1: Add the action input**

In `action/action.yml`, add after the `dry-run` input:

```yaml
  lock-wait:
    description: "Wait up to this long for the deploy lock (passed as --lock-wait). Empty disables waiting."
    required: false
    default: "5m"
```

In the same file, update the `extra-args` description to drop the stale
reference — replace its current text
`"Extra arguments appended to dockrail deploy (e.g. --lock-wait 5m once available)."`
with `"Extra arguments appended to dockrail deploy."`.

In the Deploy step, after the dry-run branch, add:

```bash
        if [ -n "${{ inputs.lock-wait }}" ]; then
          args+=(--lock-wait "${{ inputs.lock-wait }}")
        fi
```

- [ ] **Step 2: Update docs/gitops.md**

Three edits:

1. GitHub Actions example: add `lock-wait: 5m` under the `with:` block
   (after `dry-run:`), and bump `version: v0.1.0` (and its comment) to
   `version: v0.2.0` — the flag only exists from the next release.
2. GitLab example: change the deploy job's script line
   `- dockrail deploy` to `- dockrail deploy --lock-wait 5m`, and bump the
   `v0.1.0` in the install URL to `v0.2.0`.
3. After the GitLab section, add:

```markdown
## The deploy lock

Concurrent deploys to the same host are serialized by a per-project lock on
the target. `--lock-wait 5m` makes a second deploy wait instead of failing —
use it in CI so back-to-back merges queue up. `dockrail lock status` shows
who holds the lock; `dockrail lock release` clears a stale one (e.g. after a
crashed deploy).

Do **not** wire `dockrail lock release` into automated cleanup: it has no
staleness check, so a script can force-release a lock held by a live, slow
deploy (LLM model warmup can legitimately take many minutes). Releasing is a
human decision.
```

- [ ] **Step 3: Update README.md and CLAUDE.md**

README: in the commands/usage listing (grep for where `rollback` and `audit`
are listed), add `lock` (`status` / `acquire` / `release`) and mention
`deploy --lock-wait`.

CLAUDE.md: in "v1 progress", move lock into **Done** (append to the done
list: `deploy lock with --lock-wait, holder metadata, and lock
status/acquire/release (engine/lock.go, cmd/lock.go)`) and shrink
**Remaining for v1** items 1–2 to: `1. config command.` followed by the
renumbered hooks and dogfood items.

- [ ] **Step 4: Verify**

Run: `go vet ./... && go test ./...` — PASS (guards against accidental code edits).
Check: no `§` in edited docs; only example.com hosts; `grep -n "lock-wait" action/action.yml docs/gitops.md` shows the new input and both CI examples.

- [ ] **Step 5: Commit**

```bash
git add action/action.yml docs/gitops.md README.md CLAUDE.md
git commit -m "feat(action): lock-wait input; document the deploy lock"
```
