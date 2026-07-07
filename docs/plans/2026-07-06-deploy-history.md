# Deploy History Cluster Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the flat host state file with an append-only deploy history (`history.jsonl`), and build `audit`, `rollback [TAG]`, and `retain_containers` retention with log-tail capture on top of it.

**Architecture:** A new `engine/history.go` owns the record type and all host-file I/O (append via `>>` heredoc, read via `cat`). The engine's Deploy/Rollback/Status derive current/previous tags from history instead of `state.json`. `audit` and `rollback [TAG]` are thin cobra commands over new engine methods. Retention (image + saved-log pruning) and pre-recreate log capture hook into the existing deploy flow.

**Tech Stack:** Go 1.26, cobra, stdlib testing against `connection.Fake` (records issued commands; `Stub(substr, stdout, err)` cans responses).

**Spec:** `docs/specs/2026-07-06-deploy-history-design.md` — read it first.

## Global Constraints

- NEVER `git commit` or `git push` — repo convention; leave all changes in the working tree. "Commit" steps below mean: run `gofmt -l .` (expect no output) and `go vet ./...` and `go test ./...` (expect PASS), then stop.
- All host interaction goes through `connection.Connection.Run(ctx, cmd string)`; anything interpolated into a command must be validated (`safeTag` for tags, `projectRe` for names) — see `engine/engine.go:23`.
- History file: `$HOME/.dockrail/<project>/history.jsonl`. Saved logs: `$HOME/.dockrail/<project>/logs/`.
- Outcome values exactly: `deployed`, `failed@<step>`, `rolled-back`. Successful outcomes = `deployed` and `rolled-back`.
- `retain_containers` default 5 (project-level config key).
- The old `state.json` code is deleted, not migrated (no live hosts).
- Don't use the `§` symbol in any markdown.

---

### Task 1: History record store (`engine/history.go`)

**Files:**
- Create: `engine/history.go`
- Create: `engine/history_test.go`
- Delete (at end of Task 2, not here): `engine/state.go`, `engine/status_test.go` state fixtures

**Interfaces:**
- Produces (later tasks rely on these exact signatures):
  - `type Record struct { TS, Tag string; Services map[string]string; Performer, Outcome string }` (json tags `ts`, `tag`, `services` omitempty, `performer`, `outcome`)
  - `func historyPath(project string) string`
  - `func loadHistory(ctx context.Context, conn connection.Connection, project string) ([]Record, error)`
  - `func appendRecord(ctx context.Context, conn connection.Connection, project string, r Record) error` (fills TS/Performer if empty)
  - `func currentRecord(h []Record) (Record, bool)` — last record with a successful outcome
  - `func previousTag(h []Record) string` — tag of the latest successful record whose tag differs from current's; `""` if none
  - `func lastFailure(h []Record) string` — `"<tag>: <outcome>"` of the last record if it is a failure and comes after the current anchor, else `""`
  - `func performer() string` — `DOCKRAIL_PERFORMER` env if set, else `$USER`

- [ ] **Step 1: Write the failing tests**

```go
// engine/history_test.go
package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

const cannedHistory = `{"ts":"2026-07-06T10:00:00Z","tag":"v1","performer":"ci","outcome":"deployed"}
{"ts":"2026-07-06T11:00:00Z","tag":"v2","performer":"alice","outcome":"failed@readiness"}
{"ts":"2026-07-06T12:00:00Z","tag":"v2","performer":"alice","outcome":"deployed"}
`

func TestLoadHistoryParsesLines(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", cannedHistory, nil)
	h, err := loadHistory(context.Background(), f, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 || h[1].Outcome != "failed@readiness" {
		t.Fatalf("got %+v", h)
	}
}

func TestLoadHistoryEmptyFileIsFirstDeploy(t *testing.T) {
	f := connection.NewFake()
	h, err := loadHistory(context.Background(), f, "proj")
	if err != nil || len(h) != 0 {
		t.Fatalf("h=%v err=%v", h, err)
	}
}

func TestLoadHistoryCorruptLineFails(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", "{not json}\n", nil)
	if _, err := loadHistory(context.Background(), f, "proj"); err == nil {
		t.Fatal("want error on corrupt history")
	}
}

func TestCurrentAndPreviousDerivation(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", cannedHistory, nil)
	h, _ := loadHistory(context.Background(), f, "proj")
	cur, ok := currentRecord(h)
	if !ok || cur.Tag != "v2" {
		t.Fatalf("current = %+v ok=%v", cur, ok)
	}
	if prev := previousTag(h); prev != "v1" {
		t.Fatalf("previous = %q, want v1", prev)
	}
}

func TestPreviousSkipsSameTagAndFailures(t *testing.T) {
	h := []Record{
		{Tag: "v1", Outcome: "deployed"},
		{Tag: "v2", Outcome: "deployed"},
		{Tag: "v1", Outcome: "rolled-back"},
	}
	// current is the rolled-back v1; previous must be v2, not the older v1.
	if prev := previousTag(h); prev != "v2" {
		t.Fatalf("previous = %q, want v2", prev)
	}
}

func TestLastFailureOnlyAfterAnchor(t *testing.T) {
	h := []Record{
		{Tag: "v1", Outcome: "deployed"},
		{Tag: "v2", Outcome: "failed@readiness"},
	}
	if lf := lastFailure(h); !strings.Contains(lf, "v2") {
		t.Fatalf("lastFailure = %q", lf)
	}
	h = append(h, Record{Tag: "v2", Outcome: "deployed"})
	if lf := lastFailure(h); lf != "" {
		t.Fatalf("lastFailure after success = %q, want empty", lf)
	}
}

func TestAppendRecordIssuesAppendAndFillsFields(t *testing.T) {
	t.Setenv("DOCKRAIL_PERFORMER", "ci-bot")
	f := connection.NewFake()
	err := appendRecord(context.Background(), f, "proj", Record{Tag: "v3", Outcome: "deployed"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := f.Commands[len(f.Commands)-1]
	if !strings.Contains(cmd, ">>") || !strings.Contains(cmd, "history.jsonl") {
		t.Fatalf("not an append: %s", cmd)
	}
	if !strings.Contains(cmd, `"performer":"ci-bot"`) || !strings.Contains(cmd, `"ts":"`) {
		t.Fatalf("fields not filled: %s", cmd)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./engine/ -run 'History|CurrentAnd|Previous|LastFailure|AppendRecord' -v`
Expected: FAIL — `undefined: loadHistory` etc.

- [ ] **Step 3: Implement `engine/history.go`**

```go
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goodsmileduck/dockrail/connection"
)

// Record is one line of the append-only per-project deploy history on the
// target host. Outcome is "deployed", "failed@<step>", or "rolled-back"
// (whose Tag is the tag restored).
type Record struct {
	TS        string            `json:"ts"`
	Tag       string            `json:"tag"`
	Services  map[string]string `json:"services,omitempty"`
	Performer string            `json:"performer"`
	Outcome   string            `json:"outcome"`
}

func historyPath(project string) string {
	return fmt.Sprintf("$HOME/.dockrail/%s/history.jsonl", project)
}

func performer() string {
	if p := os.Getenv("DOCKRAIL_PERFORMER"); p != "" {
		return p
	}
	return os.Getenv("USER")
}

func (r Record) success() bool {
	return r.Outcome == "deployed" || r.Outcome == "rolled-back"
}

func loadHistory(ctx context.Context, conn connection.Connection, project string) ([]Record, error) {
	out, err := conn.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null || true", historyPath(project)))
	if err != nil {
		return nil, err
	}
	var h []Record
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("corrupt history line %q: %w", line, err)
		}
		h = append(h, r)
	}
	return h, nil
}

func appendRecord(ctx context.Context, conn connection.Connection, project string, r Record) error {
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339)
	}
	if r.Performer == "" {
		r.Performer = performer()
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	dir := fmt.Sprintf("$HOME/.dockrail/%s", project)
	cmd := fmt.Sprintf("mkdir -p %s && cat >> %s <<'DDEOF'\n%s\nDDEOF", dir, historyPath(project), raw)
	_, err = conn.Run(ctx, cmd)
	return err
}

// currentRecord returns the last successful record — the rollback anchor.
func currentRecord(h []Record) (Record, bool) {
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].success() {
			return h[i], true
		}
	}
	return Record{}, false
}

// previousTag returns the tag of the latest successful record whose tag
// differs from the current anchor's, or "" if there is none.
func previousTag(h []Record) string {
	cur, ok := currentRecord(h)
	if !ok {
		return ""
	}
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].success() && h[i].Tag != cur.Tag {
			return h[i].Tag
		}
	}
	return ""
}

// lastFailure reports the trailing failure, if the most recent record is a
// failed attempt (i.e. nothing succeeded after it).
func lastFailure(h []Record) string {
	if len(h) == 0 {
		return ""
	}
	last := h[len(h)-1]
	if last.success() {
		return ""
	}
	return fmt.Sprintf("%s: %s", last.Tag, last.Outcome)
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./engine/ -run 'History|CurrentAnd|Previous|LastFailure|AppendRecord' -v`
Expected: PASS

- [ ] **Step 5: Verify**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: no gofmt output, vet clean, all tests pass (state.go still present and used — that's fine until Task 2).

---

### Task 2: Wire the engine to history; delete `state.json`

**Files:**
- Modify: `engine/engine.go` (Deploy, Rollback, finalize, recordFailure)
- Modify: `engine/status.go` (derive from history)
- Delete: `engine/state.go` — keep `acquireLock` by moving it to a new `engine/lock.go` verbatim
- Modify: `engine/engine_test.go`, `engine/status_test.go`, `engine/bluegreen_test.go` — replace `state.json` stubs/assertions with `history.jsonl` ones
- Test: existing files above

**Interfaces:**
- Consumes: Task 1's `loadHistory`, `appendRecord`, `currentRecord`, `previousTag`, `lastFailure`, `Record`.
- Produces: `Engine.Deploy` / `Engine.Rollback` now append exactly one history record per attempt; `StatusReport` fields unchanged (still `CurrentTag`, `PreviousTag`, `LastFailure` — now derived). `recordFailure` signature becomes `recordFailure(ctx context.Context, tag, step string, retErr error) error`.

- [ ] **Step 1: Update the tests first.** In `engine/engine_test.go` and `engine/bluegreen_test.go`, replace every `f.Stub("state.json", ...)` with `f.Stub("history.jsonl", ...)` using canned jsonl (see `cannedHistory` in Task 1), and replace assertions that a `state.json` write happened with assertions that the last commands include an append to `history.jsonl` containing `"outcome":"deployed"` (success paths) or `"outcome":"failed@` (failure paths). Add one new test:

```go
func TestDeployAppendsFailureRecord(t *testing.T) {
	f := connection.NewFake()
	f.Stub("pull", "", fmt.Errorf("boom"))
	e := &Engine{Conn: f, Cfg: testCfg(), Out: io.Discard} // reuse the existing test config helper; if none exists, build the minimal Config used by current engine tests
	if err := e.Deploy(context.Background()); err == nil {
		t.Fatal("want deploy error")
	}
	last := f.Commands[len(f.Commands)-1]
	if !strings.Contains(last, "history.jsonl") || !strings.Contains(last, `"outcome":"failed@deploy"`) {
		t.Fatalf("no failure record appended: %s", last)
	}
}
```

(The lock-release `rmdir` may follow the append; assert against the last `history.jsonl` command, not blindly `Commands[len-1]`, if ordering differs.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./engine/ -v`
Expected: FAIL — engine still writes `state.json`.

- [ ] **Step 3: Rewire `engine/engine.go`.** Replace the state-based blocks:

```go
func (e *Engine) Deploy(ctx context.Context) error {
	e.logf("step lock")
	release, err := acquireLock(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	defer release()

	e.logf("step preflight")
	if errs := Preflight(ctx, e.Conn, e.Cfg); len(errs) > 0 {
		return fmt.Errorf("preflight failed: %v", errs)
	}

	secrets, err := collectSecrets(e.Cfg.Secrets.FromEnv)
	if err != nil {
		return e.recordFailure(ctx, "", "secrets", err)
	}
	prefix, err := writeSecretsFile(ctx, e.Conn, e.Cfg.Project, secrets)
	if err != nil {
		return e.recordFailure(ctx, "", "secrets", err)
	}
	if err := registryLogin(ctx, e.Conn, e.Cfg.Registry, e.Out); err != nil {
		return e.recordFailure(ctx, "", "registry", err)
	}

	var deployed string
	ids := map[string]string{}
	for name, svc := range e.Cfg.Services {
		if err := e.cutover(ctx, name, svc, svc.ImageTag, prefix); err != nil {
			return e.recordFailure(ctx, svc.ImageTag, "deploy",
				fmt.Errorf("service %s: %w", name, err))
		}
		deployed = svc.ImageTag
		if cid, err := e.containerID(ctx, name); err == nil && cid != "" {
			ids[name] = cid
		}
	}
	return e.finalize(ctx, deployed, "deployed", ids)
}
```

`Rollback` (no-arg) analogously:

```go
func (e *Engine) Rollback(ctx context.Context) error {
	e.logf("step lock")
	release, err := acquireLock(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	defer release()

	h, err := loadHistory(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	target := previousTag(h)
	if target == "" {
		return fmt.Errorf("no previous tag in history for project %q; nothing to roll back to", e.Cfg.Project)
	}
	return e.deployTag(ctx, target, "rolled-back")
}

// deployTag runs secrets+registry+cutover for every service at an explicit
// tag and appends one history record with the given success outcome. Shared
// by Rollback and (Task 4) RollbackTo.
func (e *Engine) deployTag(ctx context.Context, tag, outcome string) error {
	e.logf("step rollback: restoring tag %s", tag)
	secrets, err := collectSecrets(e.Cfg.Secrets.FromEnv)
	if err != nil {
		return e.recordFailure(ctx, tag, "secrets", err)
	}
	prefix, err := writeSecretsFile(ctx, e.Conn, e.Cfg.Project, secrets)
	if err != nil {
		return e.recordFailure(ctx, tag, "secrets", err)
	}
	if err := registryLogin(ctx, e.Conn, e.Cfg.Registry, e.Out); err != nil {
		return e.recordFailure(ctx, tag, "registry", err)
	}
	ids := map[string]string{}
	for name, svc := range e.Cfg.Services {
		if err := e.cutover(ctx, name, svc, tag, prefix); err != nil {
			return e.recordFailure(ctx, tag, "rollback",
				fmt.Errorf("service %s: %w", name, err))
		}
		if cid, err := e.containerID(ctx, name); err == nil && cid != "" {
			ids[name] = cid
		}
	}
	return e.finalize(ctx, tag, outcome, ids)
}
```

New helpers replacing `finalize`/`recordFailure`, plus `containerID`:

```go
// finalize appends the success record and prunes dangling images. The prune
// is best-effort and never fails the operation.
func (e *Engine) finalize(ctx context.Context, tag, outcome string, ids map[string]string) error {
	e.logf("step finalize")
	if err := appendRecord(ctx, e.Conn, e.Cfg.Project, Record{Tag: tag, Outcome: outcome, Services: ids}); err != nil {
		return err
	}
	if _, err := e.Conn.Run(ctx, "docker image prune -f"); err != nil {
		e.logf("warn: prune failed: %v", err)
	}
	return nil
}

// recordFailure appends a failed@<step> record (best-effort) and returns the
// caller's error. Nothing else is written, so the rollback anchor — the last
// successful record — is untouched.
func (e *Engine) recordFailure(ctx context.Context, tag, step string, retErr error) error {
	_ = appendRecord(ctx, e.Conn, e.Cfg.Project, Record{Tag: tag, Outcome: "failed@" + step})
	return retErr
}

// containerID reports the running container for a service, checking the
// plain compose name first and then the blue/green slot names.
func (e *Engine) containerID(ctx context.Context, name string) (string, error) {
	for _, n := range []string{name, name + "-blue", name + "-green"} {
		out, err := e.Conn.Run(ctx, fmt.Sprintf("docker compose -f %s ps -q %s", e.Cfg.Compose, n))
		if err != nil {
			return "", err
		}
		if s := strings.TrimSpace(out); s != "" {
			return s, nil
		}
	}
	return "", nil
}
```

Add `"strings"` to imports. In `engine/status.go`, replace the `loadState` block:

```go
h, err := loadHistory(ctx, e.Conn, e.Cfg.Project)
if err != nil {
	return StatusReport{}, err
}
rep := StatusReport{LastFailure: lastFailure(h), PreviousTag: previousTag(h)}
if cur, ok := currentRecord(h); ok {
	rep.CurrentTag = cur.Tag
}
```

Move `acquireLock` verbatim from `engine/state.go` into new `engine/lock.go`; delete `engine/state.go` (the `State` type, `statePath`, `loadState`, `saveState`).

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: PASS (fix any remaining `state.json` references the compiler or tests surface — `cmd/` tests stub through the fake too).

- [ ] **Step 5: Verify**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: clean.

---

### Task 3: `audit` command

**Files:**
- Create: `engine/audit.go`, `engine/audit_test.go`
- Create: `cmd/audit.go`, `cmd/audit_test.go`
- Modify: `cmd/root.go` (add `root.AddCommand(newAuditCmd())`)

**Interfaces:**
- Consumes: `loadHistory`, `currentRecord`, `Record`.
- Produces: `func (e *Engine) Audit(ctx context.Context, n int) ([]Record, int, error)` — last `n` records (all if `n<=0` or fewer exist) oldest-first, plus the index of the current anchor within the returned slice (`-1` if absent).

- [ ] **Step 1: Write failing tests**

```go
// engine/audit_test.go
package engine

import (
	"context"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

func TestAuditLimitsAndMarksAnchor(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", cannedHistory, nil) // v1 deployed, v2 failed, v2 deployed
	e := &Engine{Conn: f, Cfg: minimalCfg(), Out: discard()}
	recs, anchor, err := e.Audit(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Outcome != "failed@readiness" {
		t.Fatalf("recs = %+v", recs)
	}
	if anchor != 1 {
		t.Fatalf("anchor = %d, want 1 (the trailing deployed record)", anchor)
	}
}
```

(`minimalCfg()`/`discard()`: reuse whatever helper the existing engine tests use for a minimal `*config.Config` and `io.Discard`; add tiny local helpers if none are shared.)

```go
// cmd/audit_test.go
package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

func TestAuditPrintsTableWithAnchorMark(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl",
		`{"ts":"2026-07-06T10:00:00Z","tag":"v1","performer":"ci","outcome":"deployed"}
{"ts":"2026-07-06T12:00:00Z","tag":"v2","performer":"alice","outcome":"deployed"}
`, nil)
	var buf bytes.Buffer
	if err := runAudit(context.Background(), f, testConfig(), &buf, 20); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "v2") || !strings.Contains(out, "alice") {
		t.Fatalf("missing columns:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "v2") && !strings.Contains(line, "*") {
			t.Fatalf("current anchor line not marked:\n%s", out)
		}
	}
}
```

(`testConfig()`: reuse the existing `cmd` test config helper — `cmd/status_test.go` has one; match its name.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./engine/ ./cmd/ -run Audit -v`
Expected: FAIL — `Audit` / `runAudit` undefined.

- [ ] **Step 3: Implement**

```go
// engine/audit.go
package engine

import "context"

// Audit returns the last n history records (all if n<=0), oldest first, and
// the index within the returned slice of the current anchor (-1 if the
// anchor was truncated away or no success exists).
func (e *Engine) Audit(ctx context.Context, n int) ([]Record, int, error) {
	h, err := loadHistory(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return nil, -1, err
	}
	cur, ok := currentRecord(h)
	if n > 0 && len(h) > n {
		h = h[len(h)-n:]
	}
	anchor := -1
	if ok {
		for i := len(h) - 1; i >= 0; i-- {
			if h[i].TS == cur.TS && h[i].Tag == cur.Tag && h[i].Outcome == cur.Outcome {
				anchor = i
				break
			}
		}
	}
	return h, anchor, nil
}
```

```go
// cmd/audit.go
package cmd

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/engine"
)

func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "print the deploy history recorded on the target host",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			n, _ := cmd.Flags().GetInt("n")
			conn := connection.New(cfg.Target)
			return runAudit(cmd.Context(), conn, cfg, cmd.OutOrStdout(), n)
		},
	}
	cmd.Flags().IntP("n", "n", 20, "number of records to show (0 = all)")
	return cmd
}

func runAudit(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer, n int) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	recs, anchor, err := e.Audit(ctx, n)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		fmt.Fprintln(out, "no deploy history")
		return nil
	}
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "\tTIMESTAMP\tTAG\tPERFORMER\tOUTCOME")
	for i, r := range recs {
		mark := ""
		if i == anchor {
			mark = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", mark, r.TS, r.Tag, r.Performer, r.Outcome)
	}
	return w.Flush()
}
```

Register in `cmd/root.go`: `root.AddCommand(newAuditCmd())`.

- [ ] **Step 4: Run tests**

Run: `go test ./engine/ ./cmd/ -run Audit -v`
Expected: PASS

- [ ] **Step 5: Verify**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: clean.

---

### Task 4: `rollback [TAG]`

**Files:**
- Modify: `engine/engine.go` (add `RollbackTo`)
- Modify: `cmd/rollback.go` (accept optional TAG arg)
- Test: `engine/engine_test.go` (or a new `engine/rollbackto_test.go`), `cmd/rollback_test.go`

**Interfaces:**
- Consumes: Task 2's `deployTag(ctx, tag, outcome)`, `loadHistory`; Task 5 adds `RetainContainers` — until then use `retainWindow(cfg)` below, which defaults to 5.
- Produces: `func (e *Engine) RollbackTo(ctx context.Context, tag string) error`.

- [ ] **Step 1: Write failing tests**

```go
// engine/rollbackto_test.go
package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

const rollbackHistory = `{"ts":"1","tag":"v1","performer":"ci","outcome":"deployed"}
{"ts":"2","tag":"v2","performer":"ci","outcome":"deployed"}
{"ts":"3","tag":"v3","performer":"ci","outcome":"deployed"}
`

func TestRollbackToAcceptsRetainedTag(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", rollbackHistory, nil)
	f.Stub("docker image inspect", "sha256:abc", nil)
	e := &Engine{Conn: f, Cfg: minimalCfg(), Out: discard()}
	if err := e.RollbackTo(context.Background(), "v1"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "TAG=v1") || !strings.Contains(joined, `"outcome":"rolled-back"`) {
		t.Fatalf("no cutover at v1 / no rolled-back record:\n%s", joined)
	}
}

func TestRollbackToRejectsUnknownTag(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", rollbackHistory, nil)
	e := &Engine{Conn: f, Cfg: minimalCfg(), Out: discard()}
	err := e.RollbackTo(context.Background(), "v9")
	if err == nil || !strings.Contains(err.Error(), "v3") {
		t.Fatalf("want error listing candidates, got %v", err)
	}
}

func TestRollbackToRejectsPrunedImage(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", rollbackHistory, nil)
	f.Stub("docker image inspect", "", fmt.Errorf("no such image"))
	e := &Engine{Conn: f, Cfg: minimalCfg(), Out: discard()}
	if err := e.RollbackTo(context.Background(), "v1"); err == nil {
		t.Fatal("want error when image is gone from host")
	}
}
```

`cmd/rollback_test.go`: add a test that `newRollbackCmd()` with arg `["v1"]` reaches `RollbackTo` (assert `TAG=v1` appears in the fake's commands), mirroring the existing rollback cmd test's setup.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./engine/ -run RollbackTo -v`
Expected: FAIL — `RollbackTo` undefined.

- [ ] **Step 3: Implement.** In `engine/engine.go`:

```go
// RollbackTo restores an explicit tag. The tag must appear as a successful
// record within the last retain_containers distinct deployed tags, and its
// image must still be present on the host (retention may have pruned it).
func (e *Engine) RollbackTo(ctx context.Context, tag string) error {
	if !safeTag.MatchString(tag) {
		return fmt.Errorf("unsafe image tag %q", tag)
	}
	e.logf("step lock")
	release, err := acquireLock(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	defer release()

	h, err := loadHistory(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	window := retainedTags(h, retainWindow(e.Cfg))
	found := false
	for _, t := range window {
		if t == tag {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("tag %q is not in the retained history window; candidates: %s",
			tag, strings.Join(window, ", "))
	}
	if err := e.imagePresent(ctx, tag); err != nil {
		return fmt.Errorf("tag %q is in history but its image is gone from the host (pruned?): %w", tag, err)
	}
	return e.deployTag(ctx, tag, "rolled-back")
}

// retainedTags returns the last n distinct successfully-deployed tags,
// newest first.
func retainedTags(h []Record, n int) []string {
	var tags []string
	seen := map[string]bool{}
	for i := len(h) - 1; i >= 0 && len(tags) < n; i-- {
		if h[i].success() && !seen[h[i].Tag] {
			seen[h[i].Tag] = true
			tags = append(tags, h[i].Tag)
		}
	}
	return tags
}

func retainWindow(cfg *config.Config) int {
	if cfg.RetainContainers > 0 { // field lands in Task 5; until then hardcode: return 5
		return cfg.RetainContainers
	}
	return 5
}

// imagePresent checks every service's image at the given tag exists on the
// host, resolving repo names through compose itself.
func (e *Engine) imagePresent(ctx context.Context, tag string) error {
	out, err := e.Conn.Run(ctx, fmt.Sprintf("TAG=%s docker compose -f %s config --images", tag, e.Cfg.Compose))
	if err != nil {
		return err
	}
	for _, img := range strings.Fields(out) {
		if _, err := e.Conn.Run(ctx, fmt.Sprintf("docker image inspect --format '{{.Id}}' %s", img)); err != nil {
			return fmt.Errorf("image %s: %w", img, err)
		}
	}
	return nil
}
```

Note the ordering constraint: if Task 5 hasn't run yet, `retainWindow` cannot reference `cfg.RetainContainers` — write it as `func retainWindow(*config.Config) int { return 5 }` and Task 5 replaces the body. In `cmd/rollback.go`:

```go
func newRollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback [TAG]",
		Short: "restore the previous image tag, or an explicit retained TAG",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			conn := connection.New(cfg.Target)
			tag := ""
			if len(args) == 1 {
				tag = args[0]
			}
			return runRollback(cmd.Context(), conn, cfg, cmd.OutOrStdout(), tag)
		},
	}
}

func runRollback(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer, tag string) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	if tag != "" {
		return e.RollbackTo(ctx, tag)
	}
	return e.Rollback(ctx)
}
```

(Update the existing `runRollback` call sites/tests for the new `tag` parameter.)

- [ ] **Step 4: Run tests**

Run: `go test ./engine/ ./cmd/ -run Rollback -v`
Expected: PASS

- [ ] **Step 5: Verify**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: clean.

---

### Task 5: `retain_containers` — log capture + pruning

**Files:**
- Modify: `config/config.go` (add `RetainContainers` with default + validation)
- Create: `engine/retention.go`, `engine/retention_test.go`
- Modify: `engine/engine.go` (`finalize` calls `e.prune`; `retainWindow` body from Task 4 now reads the field)
- Modify: `engine/engine.go` `recreate` and `engine/bluegreen.go` `proxyCutover` (log capture before slot recreate)
- Test: `config/config_test.go`, `engine/retention_test.go`, existing engine tests

**Interfaces:**
- Consumes: `retainedTags` (Task 4), `loadHistory`, `Record`.
- Produces: `config.Config.RetainContainers int` (yaml `retain_containers`, default 5); `func (e *Engine) captureLogs(ctx context.Context, svcName, composeName string)` (best-effort, no error return); `func (e *Engine) prune(ctx context.Context, h []Record)` (best-effort).

- [ ] **Step 1: Write failing tests**

`config/config_test.go` additions:

```go
func TestRetainContainersDefaultsTo5(t *testing.T) {
	cfg := loadValid(t) // reuse the existing valid-config test helper/fixture
	if cfg.RetainContainers != 5 {
		t.Fatalf("default = %d, want 5", cfg.RetainContainers)
	}
}

func TestRetainContainersRejectsNegative(t *testing.T) {
	// write a fixture with retain_containers: -1 and expect Load to error
}
```

```go
// engine/retention_test.go
package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

func TestCaptureLogsSavesTailOfExistingSlot(t *testing.T) {
	f := connection.NewFake()
	f.Stub("ps -q", "abc123\n", nil)
	f.Stub("docker inspect --format '{{.Config.Image}}'", "registry.example.com/api:v1\n", nil)
	e := &Engine{Conn: f, Cfg: minimalCfg(), Out: discard()}
	e.captureLogs(context.Background(), "api", "api")
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "docker logs --tail 1000 abc123") ||
		!strings.Contains(joined, "/logs/api-v1-") {
		t.Fatalf("log capture missing:\n%s", joined)
	}
}

func TestCaptureLogsNoopWhenSlotEmpty(t *testing.T) {
	f := connection.NewFake() // ps -q returns ""
	e := &Engine{Conn: f, Cfg: minimalCfg(), Out: discard()}
	e.captureLogs(context.Background(), "api", "api")
	for _, c := range f.Commands {
		if strings.Contains(c, "docker logs") {
			t.Fatalf("unexpected log capture: %s", c)
		}
	}
}

func TestPruneRemovesOutOfWindowImagesAndLogs(t *testing.T) {
	f := connection.NewFake()
	f.Stub("config --images", "registry.example.com/api:PLACEHOLDER\n", nil)
	e := &Engine{Conn: f, Cfg: minimalCfg(), Out: discard()} // RetainContainers: 2 in cfg
	e.Cfg.RetainContainers = 2
	h := []Record{
		{Tag: "v1", Outcome: "deployed"},
		{Tag: "v2", Outcome: "deployed"},
		{Tag: "v3", Outcome: "deployed"},
		{Tag: "v4", Outcome: "failed@readiness"}, // failed: exempt, never a prune victim
	}
	e.prune(context.Background(), h)
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "docker image rm") || !strings.Contains(joined, ":v1") {
		t.Fatalf("v1 image not pruned:\n%s", joined)
	}
	if strings.Contains(joined, "rm registry.example.com/api:v2") || strings.Contains(joined, ":v4") {
		t.Fatalf("in-window or failed tag pruned:\n%s", joined)
	}
	if !strings.Contains(joined, "-v1-") || !strings.Contains(joined, "logs/") {
		t.Fatalf("v1 saved logs not pruned:\n%s", joined)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./config/ ./engine/ -run 'Retain|CaptureLogs|Prune' -v`
Expected: FAIL.

- [ ] **Step 3: Implement.** `config/config.go`: add field to `Config`:

```go
RetainContainers int `yaml:"retain_containers"` // rollback window: images + saved log tails kept (default 5)
```

In `Load`, after decode and before `validate`: `if cfg.RetainContainers == 0 { cfg.RetainContainers = 5 }`. In `validate`: `if c.RetainContainers < 0 { return fmt.Errorf("retain_containers must be >= 1") }` (0 became 5 already; explicit negatives rejected). Update Task 4's `retainWindow` body to `return cfg.RetainContainers`.

`engine/retention.go`:

```go
package engine

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// captureLogs saves a tail of the slot container's docker logs to the host
// state dir before compose recreates it (recreation deletes the old
// container's logs). Best-effort: failures are logged, never fatal.
// svcName is the logical service; composeName the compose service whose
// container is about to be replaced (e.g. "api" or "api-blue").
func (e *Engine) captureLogs(ctx context.Context, svcName, composeName string) {
	cid, err := e.Conn.Run(ctx, fmt.Sprintf("docker compose -f %s ps -q %s", e.Cfg.Compose, composeName))
	if err != nil || strings.TrimSpace(cid) == "" {
		return
	}
	cid = strings.TrimSpace(cid)
	img, err := e.Conn.Run(ctx, fmt.Sprintf("docker inspect --format '{{.Config.Image}}' %s", cid))
	if err != nil {
		return
	}
	tag := "unknown"
	if i := strings.LastIndex(strings.TrimSpace(img), ":"); i >= 0 {
		tag = strings.TrimSpace(img)[i+1:]
	}
	if !safeTag.MatchString(tag) {
		tag = "unknown"
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	dir := fmt.Sprintf("$HOME/.dockrail/%s/logs", e.Cfg.Project)
	dst := fmt.Sprintf("%s/%s-%s-%s.log", dir, svcName, tag, ts)
	if _, err := e.Conn.Run(ctx, fmt.Sprintf(
		"mkdir -p %s && docker logs --tail 1000 %s > %s 2>&1 || true", dir, cid, dst)); err != nil {
		e.logf("warn: log capture for %s failed: %v", svcName, err)
	}
}

// prune removes images and saved log files whose tags fall outside the last
// retain_containers distinct successfully-deployed tags. Failed tags are
// never victims (forensics exemption); only unrelated-to-window successful
// tags are removed, and only for images compose itself maps to this project.
// Best-effort throughout.
func (e *Engine) prune(ctx context.Context, h []Record) {
	keep := map[string]bool{}
	for _, t := range retainedTags(h, retainWindow(e.Cfg)) {
		keep[t] = true
	}
	seen := map[string]bool{}
	for _, r := range h {
		if !r.success() || keep[r.Tag] || seen[r.Tag] || !safeTag.MatchString(r.Tag) {
			continue
		}
		seen[r.Tag] = true
		imgs, err := e.Conn.Run(ctx, fmt.Sprintf("TAG=%s docker compose -f %s config --images", r.Tag, e.Cfg.Compose))
		if err != nil {
			e.logf("warn: prune: resolve images for %s: %v", r.Tag, err)
			continue
		}
		for _, img := range strings.Fields(imgs) {
			// config --images echoes the interpolated tag back; only remove
			// refs that actually end in the victim tag.
			if !strings.HasSuffix(img, ":"+r.Tag) {
				continue
			}
			if _, err := e.Conn.Run(ctx, fmt.Sprintf("docker image rm %s || true", img)); err != nil {
				e.logf("warn: prune image %s: %v", img, err)
			}
		}
		if _, err := e.Conn.Run(ctx, fmt.Sprintf(
			"rm -f $HOME/.dockrail/%s/logs/*-%s-*.log", e.Cfg.Project, r.Tag)); err != nil {
			e.logf("warn: prune logs for %s: %v", r.Tag, err)
		}
	}
}
```

Wire in `engine/engine.go` `finalize`, after the success `appendRecord` (reload so the new record counts):

```go
if h, err := loadHistory(ctx, e.Conn, e.Cfg.Project); err == nil {
	e.prune(ctx, h)
} else {
	e.logf("warn: prune skipped: %v", err)
}
```

Hook log capture: in `recreate` (engine.go), immediately before the `stop %s` command: `e.captureLogs(ctx, name, name)`. In `proxyCutover` (bluegreen.go), immediately before each `up -d --no-deps` of `greenSvc` (both sequenced and zero-gap paths): `e.captureLogs(ctx, name, greenSvc)` — the green slot's *old* container is what `up` recreates. Note the `TestPruneRemovesOutOfWindowImagesAndLogs` stub returns a fixed image string; the assertion checks `docker image rm` + `:v1` appear — the `HasSuffix` guard means the stub must be adjusted per victim tag, so instead stub with `f.Stub("TAG=v1 ", "registry.example.com/api:v1\n", nil)` and `f.Stub("TAG=v3 ", "registry.example.com/api:v3\n", nil)` style per-tag stubs (Fake matches on substring; first match wins).

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: PASS (existing recreate/bluegreen tests may need their expected-command sequences updated for the new `ps -q` / capture commands — best-effort capture must not break them when stubs return empty).

- [ ] **Step 5: Verify**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: clean.

---

### Task 6: Docs — amend main spec and CLAUDE.md

**Files:**
- Modify: `docs/specs/2026-07-05-dockrail-design.md` (section 8)
- Modify: `CLAUDE.md` (v1 progress)

**Interfaces:** none (docs only).

- [ ] **Step 1: Amend section 8 of the main design doc.** In the **Retention** bullet, replace "the last `retain_containers` (default 5) stopped OLD containers and their images are kept as rollback targets; older ones are removed" with: "the last `retain_containers` (default 5) successfully-deployed images are kept as rollback targets, each with a captured `docker logs --tail 1000` snapshot saved under `$HOME/.dockrail/<project>/logs/` before its container is recreated; older images and snapshots are removed. (The two-slot model, D12, reuses the `<svc>-blue`/`<svc>-green` container names, so retained *containers* cannot exist without un-managing them from compose — the retained unit is images + log tails.)" Keep the failed-NEW forensics sentence as-is.

- [ ] **Step 2: Amend the Deploy-state bullet** to name the file: "persisted in a per-project append-only `history.jsonl` on the target host".

- [ ] **Step 3: Update CLAUDE.md.** In "v1 progress", move items 1–5's history-related entries: mark deploy history + `audit` + `rollback [TAG]` + `retain_containers` retention (with log-tail capture) as done, with package pointers (`engine/history.go`, `engine/audit.go`, `engine/retention.go`, `cmd/audit.go`). Remaining list shrinks to: `config` + `lock` commands, `--lock-wait`, lifecycle hooks, dogfooding.

- [ ] **Step 4: Verify**

Run: `go test ./...` and re-read both edited docs for the forbidden `§` symbol and stale references to `state.json`.
Expected: tests pass; no `§`; no stale `state.json` mentions outside historical git.
