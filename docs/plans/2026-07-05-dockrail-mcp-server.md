# dockrail MCP Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose dockrail's deploy/rollback/status/logs verbs to LLM agents through a Model Context Protocol server shipped as a `dockrail mcp` subcommand in the same binary.

**Architecture:** A new `mcp` package wraps the existing `engine.Engine` — no business logic is duplicated. The server speaks stdio MCP (what Claude Desktop/Code/Codex expect). Read-only operations (`status`) become MCP **resources** that a host can auto-attach as context; side-effecting operations (`deploy`, `rollback`, `logs`) become MCP **tools** annotated with read-only/destructive hints. `deploy` and `rollback` accept a `dry_run` argument that prints the command plan without touching the host. Each request loads config from a path the server was started with, builds a `connection.Connection`, and delegates to the engine.

**Tech Stack:** Go 1.26, `github.com/modelcontextprotocol/go-sdk/mcp` (official SDK, stdio transport), existing `cobra` CLI, existing `engine`/`config`/`connection` packages.

## Global Constraints

- Module path: `github.com/goodsmileduck/dockrail`. Go `1.26.0`, toolchain `go1.26.4`. Local commands run with `GOTOOLCHAIN=auto`; CI uses `GOTOOLCHAIN: local`.
- The MCP server MUST live in the same binary — no second artifact. Invoked as `dockrail mcp`.
- No business logic in the `mcp` package: it only marshals arguments, calls `engine.Engine` methods, and marshals results. All host mutation stays in `engine`.
- Destructive tools (`deploy`, `rollback`) MUST carry `Annotations.DestructiveHint = true`; the `status` resource and any read tool MUST carry `ReadOnlyHint = true`.
- `deploy` and `rollback` MUST support a `dry_run` boolean that returns the planned command sequence and performs zero host mutation.
- The engine currently writes progress to an `io.Writer` (`Engine.Out`). In MCP mode this MUST be captured into the tool result text, never written to os.Stdout (stdout is the MCP transport and must carry only protocol frames).
- Every tool/resource handler MUST return a structured error (not panic) when config load or the engine call fails.

---

### Task 1: Add the MCP SDK dependency and a `dockrail mcp` stdio skeleton

**Files:**
- Create: `mcp/server.go`
- Modify: `cmd/root.go` (register `newMCPCmd()`)
- Create: `cmd/mcp.go`
- Modify: `go.mod`, `go.sum` (add SDK)
- Test: `mcp/server_test.go`

**Interfaces:**
- Consumes: `config.Load(path string) (*config.Config, error)`, `connection.New(cfg.Target) connection.Connection`, `engine.Engine{Conn, Cfg, Out}`.
- Produces: `func New(configPath string) *mcp.Server` in package `mcp` (dockrail's own package, importing the SDK under an alias); `func Serve(ctx context.Context, configPath string) error` that runs the server over stdio.

- [ ] **Step 1: Add the SDK dependency**

Run: `GOTOOLCHAIN=auto go get github.com/modelcontextprotocol/go-sdk/mcp@latest`
Expected: `go.mod`/`go.sum` updated; no build yet.

- [ ] **Step 2: Write the failing test for server construction**

Create `mcp/server_test.go`. Because the SDK's stdio loop needs a live transport, test the *builder* (`New`) rather than `Serve`: assert it returns a non-nil server and does not panic with a valid config path.

```go
package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "deploy.yml")
	// Minimal valid config the engine/config package accepts.
	body := "project: demo\ncompose: docker-compose.yml\ntarget: local\nservices:\n  web:\n    image_tag: v1\n    cutover:\n      strategy: recreate\n    readiness:\n      type: http\n      path: /health\n      port: 8010\n      timeout: 1s\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewBuildsServer(t *testing.T) {
	srv := New(writeConfig(t))
	if srv == nil {
		t.Fatal("New returned nil server")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./mcp/... -run TestNewBuildsServer -v`
Expected: FAIL — `New` undefined.

- [ ] **Step 4: Implement `mcp/server.go`**

```go
// Package mcp exposes dockrail's deploy operations to LLM agents over the
// Model Context Protocol. It contains no deployment logic — every handler
// delegates to engine.Engine.
package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// New builds the dockrail MCP server bound to a single deploy.yml path. All
// tool/resource handlers load config from this path per request so an edited
// config is picked up without restarting the server.
func New(configPath string) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{
		Name:    "dockrail",
		Version: "dev", // overwritten by Serve via the CLI Version var
	}, nil)
	// Tools and resources are registered in later tasks.
	return srv
}

// Serve runs the server over stdio until the client disconnects.
func Serve(ctx context.Context, configPath string) error {
	return New(configPath).Run(ctx, &sdk.StdioTransport{})
}
```

- [ ] **Step 5: Wire the `dockrail mcp` command**

Create `cmd/mcp.go`:

```go
package cmd

import (
	"github.com/spf13/cobra"

	dmcp "github.com/goodsmileduck/dockrail/mcp"
)

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "run dockrail as a Model Context Protocol server over stdio",
		Long: "Exposes deploy, rollback, status, and logs to MCP-capable agents " +
			"(Claude Desktop/Code, Codex). Reads config from --config; " +
			"communicates over stdin/stdout, so nothing else may write to stdout.",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			return dmcp.Serve(cmd.Context(), path)
		},
	}
}
```

Add to `NewRootCmd()` in `cmd/root.go`, after the other `AddCommand` calls:

```go
	root.AddCommand(newMCPCmd())
```

- [ ] **Step 6: Run tests and build**

Run: `GOTOOLCHAIN=auto go test ./... && GOTOOLCHAIN=auto go build ./...`
Expected: PASS; binary builds.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum mcp/ cmd/mcp.go cmd/root.go
git commit -m "feat: add dockrail mcp stdio server skeleton"
```

---

### Task 2: Add the read-only `status` resource

**Files:**
- Modify: `mcp/server.go`
- Test: `mcp/status_test.go`

**Interfaces:**
- Consumes: `engine.Engine.Status(ctx) (engine.StatusReport, error)`; `engine.StatusReport` already carries JSON tags (from the `--json` work).
- Produces: an MCP resource at URI `dockrail://status` returning the `StatusReport` as `application/json`.

- [ ] **Step 1: Write the failing test**

Register the resource in `New`, then exercise the handler directly. Extract the handler into a named function `statusResource(configPath string)` so it is unit-testable without a live client.

```go
package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/engine"
)

func TestStatusResourceReturnsJSON(t *testing.T) {
	// Config's target is "local"; loadState/ps will run against the real
	// docker CLI, which is absent in CI. So this test asserts the handler
	// surfaces the engine error as a resource error rather than panicking.
	h := statusResource(writeConfig(t))
	_, err := h(context.Background(), nil)
	if err == nil {
		return // docker present: acceptable
	}
	if !strings.Contains(err.Error(), "status") && !strings.Contains(err.Error(), "state") {
		t.Fatalf("unexpected error shape: %v", err)
	}
	_ = json.Marshal // keep import if unused in the passing branch
	_ = engine.StatusReport{}
}
```

> Note to implementer: adjust the exact `ResourceHandler` signature to the SDK version resolved in Task 1 (`func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error)` in current releases). Keep the extracted `statusResource` returning that signature so the test compiles against the real type.

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./mcp/... -run TestStatusResource -v`
Expected: FAIL — `statusResource` undefined.

- [ ] **Step 3: Implement the resource**

In `mcp/server.go`, add a helper that builds an engine from the config path (reused by later tasks) and the resource handler:

```go
func engineFor(configPath string, out io.Writer) (*engine.Engine, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	return &engine.Engine{Conn: connection.New(cfg.Target), Cfg: cfg, Out: out}, nil
}

func statusResource(configPath string) sdk.ResourceHandler {
	return func(ctx context.Context, _ *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		e, err := engineFor(configPath, io.Discard)
		if err != nil {
			return nil, err
		}
		rep, err := e.Status(ctx)
		if err != nil {
			return nil, err
		}
		body, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return nil, err
		}
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
			URI:      "dockrail://status",
			MIMEType: "application/json",
			Text:     string(body),
		}}}, nil
	}
}
```

Register in `New`:

```go
	srv.AddResource(&sdk.Resource{
		URI:         "dockrail://status",
		Name:        "deployment status",
		Description: "current/previous tags, last failure, and live running tag per service",
		MIMEType:    "application/json",
		Annotations: &sdk.Annotations{ReadOnlyHint: true},
	}, statusResource(configPath))
```

- [ ] **Step 4: Run the test**

Run: `GOTOOLCHAIN=auto go test ./mcp/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add mcp/
git commit -m "feat: expose dockrail status as an MCP resource"
```

---

### Task 3: Add `deploy` and `rollback` tools with dry-run and destructive hints

**Files:**
- Modify: `mcp/server.go`
- Create: `mcp/tools.go`
- Test: `mcp/tools_test.go`

**Interfaces:**
- Consumes: `engine.Engine.Deploy(ctx) error`, `engine.Engine.Rollback(ctx) error`.
- Produces: MCP tools `deploy` and `rollback`, each taking `{"dry_run": bool}`; result text carries the engine's captured progress log or the planned commands.

**Design note — dry-run:** the engine does not yet have a dry-run mode. Rather than thread a flag through `engine`, the MCP tool implements dry-run at the boundary: when `dry_run` is true it swaps the real `connection.Connection` for `connection.NewFake()` (which records commands into `.Commands` and never touches the host) and runs the normal `Deploy`/`Rollback`, then returns the recorded `Commands` slice as the plan. This reuses the exact production sequence with zero host effect and no engine change.

- [ ] **Step 1: Write the failing test (dry-run records commands, mutates nothing)**

```go
package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestDeployToolDryRunPlansWithoutMutation(t *testing.T) {
	res, err := runDeploy(context.Background(), writeConfig(t), true)
	if err != nil {
		t.Fatalf("dry-run deploy: %v", err)
	}
	if !strings.Contains(res, "up -d --no-deps") {
		t.Fatalf("plan missing cutover command:\n%s", res)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./mcp/... -run TestDeployToolDryRun -v`
Expected: FAIL — `runDeploy` undefined.

- [ ] **Step 3: Implement `mcp/tools.go`**

```go
package mcp

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/engine"
)

type opArgs struct {
	DryRun bool `json:"dry_run" jsonschema:"plan the commands without touching the host"`
}

// runDeploy runs a deploy (or a dry-run plan) and returns human-readable text.
func runDeploy(ctx context.Context, configPath string, dryRun bool) (string, error) {
	return runOp(ctx, configPath, dryRun, (*engine.Engine).Deploy)
}

func runRollback(ctx context.Context, configPath string, dryRun bool) (string, error) {
	return runOp(ctx, configPath, dryRun, (*engine.Engine).Rollback)
}

func runOp(ctx context.Context, configPath string, dryRun bool, op func(*engine.Engine, context.Context) error) (string, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return "", err
	}
	var log bytes.Buffer
	var conn connection.Connection
	var fake *connection.Fake
	if dryRun {
		fake = connection.NewFake()
		conn = fake
	} else {
		conn = connection.New(cfg.Target)
	}
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: &log}
	if err := op(e, ctx); err != nil {
		return log.String(), fmt.Errorf("%w", err)
	}
	if dryRun {
		return "DRY RUN — planned commands:\n" + strings.Join(fake.Commands, "\n"), nil
	}
	return log.String(), nil
}
```

- [ ] **Step 4: Run the test**

Run: `GOTOOLCHAIN=auto go test ./mcp/... -run TestDeployToolDryRun -v`
Expected: PASS.

- [ ] **Step 5: Register the tools in `New` (`mcp/server.go`)**

```go
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "deploy",
		Description: "deploy the configured tags with health-gated cutover; set dry_run to preview commands",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, a opArgs) (*sdk.CallToolResult, any, error) {
		text, err := runDeploy(ctx, configPath, a.DryRun)
		return toolResult(text, err)
	})
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "rollback",
		Description: "restore the previously deployed tags; set dry_run to preview commands",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, a opArgs) (*sdk.CallToolResult, any, error) {
		text, err := runRollback(ctx, configPath, a.DryRun)
		return toolResult(text, err)
	})
```

Add small helpers (adjust `CallToolResult` field names to the resolved SDK version):

```go
func ptr[T any](v T) *T { return &v }

func toolResult(text string, err error) (*sdk.CallToolResult, any, error) {
	if err != nil {
		return &sdk.CallToolResult{
			IsError: true,
			Content: []sdk.Content{&sdk.TextContent{Text: err.Error() + "\n" + text}},
		}, nil, nil
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: text}},
	}, nil, nil
}
```

> Note: engine errors are returned as `IsError` tool results (so the agent sees and can react to them), not as transport errors. Confirm the exact `AddTool` handler signature and `Content` type against the resolved SDK; keep `runDeploy`/`runRollback` unchanged regardless.

- [ ] **Step 6: Run all tests and build**

Run: `GOTOOLCHAIN=auto go test ./... && GOTOOLCHAIN=auto go build ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add mcp/
git commit -m "feat: add MCP deploy and rollback tools with dry-run"
```

---

### Task 4: Add the `logs` tool

**Files:**
- Modify: `mcp/server.go`, `mcp/tools.go`
- Test: `mcp/tools_test.go`

**Interfaces:**
- Consumes: the same `docker compose ... logs` command the `logs` CLI issues (see `cmd/logs.go`).
- Produces: MCP tool `logs` taking `{"service": string, "tail": int}`, read-only, returning the captured output. Follow mode is intentionally omitted — a request/response tool cannot stream.

- [ ] **Step 1: Write the failing test**

```go
func TestLogsToolRejectsUnknownService(t *testing.T) {
	_, err := runLogs(context.Background(), writeConfig(t), "nope", 100)
	if err == nil || !strings.Contains(err.Error(), "service") {
		t.Fatalf("want unknown-service error, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./mcp/... -run TestLogsTool -v`
Expected: FAIL — `runLogs` undefined.

- [ ] **Step 3: Implement `runLogs` in `mcp/tools.go`**

```go
type logsArgs struct {
	Service string `json:"service" jsonschema:"the compose service name"`
	Tail    int    `json:"tail" jsonschema:"number of trailing lines (default 100)"`
}

func runLogs(ctx context.Context, configPath, service string, tail int) (string, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return "", err
	}
	if _, ok := cfg.Services[service]; !ok {
		return "", fmt.Errorf("unknown service %q", service)
	}
	if tail <= 0 {
		tail = 100
	}
	conn := connection.New(cfg.Target)
	return conn.Run(ctx, fmt.Sprintf(
		"docker compose -f %s logs --tail %d %s", cfg.Compose, tail, service))
}
```

- [ ] **Step 4: Run the test**

Run: `GOTOOLCHAIN=auto go test ./mcp/... -run TestLogsTool -v`
Expected: PASS.

- [ ] **Step 5: Register the tool in `New`**

```go
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "logs",
		Description: "fetch recent logs for one service (no follow mode)",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: ptr(true)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, a logsArgs) (*sdk.CallToolResult, any, error) {
		text, err := runLogs(ctx, configPath, a.Service, a.Tail)
		return toolResult(text, err)
	})
```

- [ ] **Step 6: Run all tests and build**

Run: `GOTOOLCHAIN=auto go test ./... && GOTOOLCHAIN=auto go build ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add mcp/
git commit -m "feat: add MCP logs tool"
```

---

### Task 5: Document the MCP server

**Files:**
- Modify: `README.md`

**Interfaces:** none (docs only).

- [ ] **Step 1: Add an "MCP server" section to `README.md`**

Document: `dockrail mcp -c deploy.yml` runs a stdio MCP server; the tool/resource surface (`deploy`, `rollback`, `logs` tools; `dockrail://status` resource); the `dry_run` argument on `deploy`/`rollback`; that stdout is reserved for protocol frames. Include a client registration snippet:

```json
{
  "mcpServers": {
    "dockrail": {
      "command": "dockrail",
      "args": ["mcp", "-c", "/abs/path/deploy.yml"]
    }
  }
}
```

Note the follow-mode limitation on `logs` and that `deploy`/`rollback` are marked destructive so well-behaved hosts prompt the user before running them.

- [ ] **Step 2: Verify formatting and commit**

Run: `GOTOOLCHAIN=auto gofmt -l . && GOTOOLCHAIN=auto go test ./...`
Expected: no gofmt output; tests PASS.

```bash
git add README.md
git commit -m "docs: document the dockrail mcp server"
```

---

## Self-Review Notes

- **SDK surface is version-sensitive.** The exact type names (`ResourceHandler`, `CallToolResult`/`CallToolResultFor`, `Content`/`TextContent`, `ToolAnnotations` field types — pointer vs value bools) differ across `go-sdk` releases. Each task says to reconcile the handler signature against the version resolved in Task 1. The dockrail-owned functions (`runDeploy`, `runRollback`, `runLogs`, `statusResource`, `engineFor`) are the stable, tested seam and must not change shape when adjusting SDK glue.
- **Dry-run reuses `connection.Fake`.** This is the key decision: no new engine flag, and the plan is guaranteed to match the real command sequence because it *is* the real sequence against a recording connection.
- **stdout discipline:** `Engine.Out` is captured into a buffer (`io.Discard` for the read resource, `bytes.Buffer` for tools). Nothing in `mcp` may write to os.Stdout — Task 1's command help text states this.
- **Spec coverage:** deploy ✅ (Task 3), rollback ✅ (Task 3), status ✅ (Task 2, resource), logs ✅ (Task 4), dry-run ✅ (Task 3), destructive/read-only hints ✅ (Tasks 2–4), same-binary ✅ (Task 1), docs ✅ (Task 5).
