# dockrail Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the dockrail skeleton — CLI, config, Connection, preflight/`check`, and the deploy engine with `recreate` cutover + `http` readiness — so a generic compose service can be deployed end-to-end, including `--dry-run`.

**Architecture:** A cobra CLI drives a deploy engine (state machine) that issues shell commands through a `Connection` interface (local exec or SSH). Readiness/Cutover/Placement are strategy interfaces; this plan implements `http` readiness, `recreate` cutover, and `none` placement. The engine is tested against a fake Connection that records issued commands. Spec: `docs/specs/2026-07-05-dockrail-design.md` (read sections 4–6 and 8 before starting).

**Tech Stack:** Go 1.26 (latest stable), `github.com/spf13/cobra`, `gopkg.in/yaml.v3`, stdlib `testing`.

## Global Constraints

- Language = Go, single static binary, no runtime deps on host (D1). Host commands may assume: `sh`, `docker` (with compose plugin), `cat`, `mkdir`.
- SSH is agentless: shell out to the system `ssh` binary with `ControlMaster` multiplexing (D2). Never embed an SSH library in this plan.
- Never put secret values on a command line (D8).
- Cutover strategies are exactly `recreate` and `proxy` (D11). `proxy` is validated in config but NOT implemented in this plan — the engine returns a clear "not implemented yet" error for it.
- State file on host: `~/.dockrail/<project>/state.json`; lock file: `~/.dockrail/<project>/lock` (spec section 8).
- Package layout per CLAUDE.md: `config`, `connection`, `engine`, `strategy/readiness`, `strategy/cutover`, `strategy/placement`, `cmd`.
- Go style: `gofmt`, `go vet` clean. Table-driven tests.
- Commit after each task (plan execution is the explicit request required by CLAUDE.md).
- Don't use the `§` symbol anywhere; write "section N".

---

### Task 1: Module + CLI skeleton

**Files:**
- Create: `go.mod`, `main.go`, `cmd/root.go`, `cmd/root_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `cmd.NewRootCmd() *cobra.Command` with subcommands `deploy`, `rollback`, `status`, `logs`, `check`, each accepting `-c/--config` (default `deploy.yml`). Subcommands are stubs returning `fmt.Errorf("not implemented")` except as later tasks fill them in.

- [ ] **Step 1: Initialize repo and module**

```bash
cd /home/goodsmileduck/local/personal/dockrail
git init
go mod init github.com/goodsmileduck/dockrail
go get github.com/spf13/cobra@latest gopkg.in/yaml.v3@latest
```

- [ ] **Step 2: Write the failing test**

`cmd/root_test.go`:

```go
package cmd

import "testing"

func TestRootHasSubcommands(t *testing.T) {
	root := NewRootCmd()
	want := []string{"deploy", "rollback", "status", "logs", "check"}
	for _, name := range want {
		found := false
		for _, c := range root.Commands() {
			if c.Name() == name {
				found = true
			}
		}
		if !found {
			t.Errorf("missing subcommand %q", name)
		}
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./cmd/ -run TestRootHasSubcommands -v`
Expected: FAIL (compile error: `NewRootCmd` undefined).

- [ ] **Step 4: Implement**

`cmd/root.go`:

```go
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "dockrail",
		Short:         "Compose-native deployer with health-gated cutover",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringP("config", "c", "deploy.yml", "path to deploy.yml")

	for _, name := range []string{"deploy", "rollback", "status", "logs", "check"} {
		name := name
		root.AddCommand(&cobra.Command{
			Use:   name,
			Short: name,
			RunE: func(cmd *cobra.Command, args []string) error {
				return fmt.Errorf("%s: not implemented", name)
			},
		})
	}
	return root
}
```

`main.go`:

```go
package main

import (
	"fmt"
	"os"

	"github.com/goodsmileduck/dockrail/cmd"
)

func main() {
	if err := cmd.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 5: Verify**

Run: `go test ./cmd/ -v && go vet ./... && go build ./...`
Expected: PASS, no vet errors, binary builds.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "feat: cobra CLI skeleton with command stubs"
```

---

### Task 2: Config parse + validate

**Files:**
- Create: `config/config.go`, `config/config_test.go`

**Interfaces:**
- Consumes: nothing
- Produces:
  - `config.Load(path string) (*Config, error)` — parse + validate in one call.
  - Types (yaml tags as shown):

```go
type Config struct {
	Project  string             `yaml:"project"`
	Compose  string             `yaml:"compose"`
	Registry Registry           `yaml:"registry"`
	Target   Target             `yaml:"target"`
	Secrets  Secrets            `yaml:"secrets"`
	Services map[string]Service `yaml:"services"`
}
type Registry struct {
	Server string `yaml:"server"`
}
type Target struct {
	Host string `yaml:"host"` // "user@host"; empty = local exec
	Port int    `yaml:"port"` // 0 = 22
}
type Secrets struct {
	FromEnv []string `yaml:"from_env"`
}
type Service struct {
	ImageTag  string    `yaml:"image_tag"`
	Model     string    `yaml:"model"`
	Readiness Readiness `yaml:"readiness"`
	Cutover   Cutover   `yaml:"cutover"`
	Placement Placement `yaml:"placement"`
}
type Readiness struct {
	Type    string `yaml:"type"` // http|tcp|vllm|cmd
	Path    string `yaml:"path"`
	Port    int    `yaml:"port"`
	Timeout string `yaml:"timeout"` // Go duration, e.g. "90s"
}
type Cutover struct {
	Strategy string `yaml:"strategy"` // recreate|proxy
	Proxy    string `yaml:"proxy"`    // e.g. nginx-upstream
	Warmup   bool   `yaml:"warmup"`
}
type Placement struct {
	Type        string `yaml:"type"` // ""|none|gpu
	Pool        []int  `yaml:"pool"`
	VRAMMin     string `yaml:"vram_min"`
	OnNoFreeGPU string `yaml:"on_no_free_gpu"` // ""|fail|stop-old-first
}
```

- [ ] **Step 1: Write the failing test**

`config/config_test.go` (table-driven; uses `t.TempDir()` to write YAML fixtures):

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
project: demo
compose: docker-compose.yml
target: { host: deploy@example.com, port: 32 }
services:
  web:
    image_tag: "abc123"
    readiness: { type: http, path: /health, port: 8010, timeout: 90s }
    cutover:   { strategy: recreate }
`

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(write(t, validYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Project != "demo" || cfg.Services["web"].Readiness.Port != 8010 {
		t.Errorf("bad parse: %+v", cfg)
	}
}

func TestLoadInvalid(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"missing project", strings.Replace(validYAML, "project: demo", "", 1), "project"},
		{"missing compose", strings.Replace(validYAML, "compose: docker-compose.yml", "", 1), "compose"},
		{"no services", "project: p\ncompose: c.yml\n", "services"},
		{"bad cutover", strings.Replace(validYAML, "strategy: recreate", "strategy: blue-green", 1), "cutover.strategy"},
		{"bad readiness", strings.Replace(validYAML, "type: http", "type: magic", 1), "readiness.type"},
		{"bad timeout", strings.Replace(validYAML, "timeout: 90s", "timeout: soon", 1), "timeout"},
		{"gpu needs pool", validYAML + "    placement: { type: gpu, vram_min: 20GiB }\n", "pool"},
		{"bad on_no_free_gpu", validYAML + "    placement: { type: gpu, pool: [0], vram_min: 20GiB, on_no_free_gpu: retry }\n", "on_no_free_gpu"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, c.yaml))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./config/ -v`
Expected: FAIL (package/`Load` undefined).

- [ ] **Step 3: Implement**

`config/config.go` — types from the Interfaces block above, plus:

```go
package config

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// (type declarations from the Interfaces block go here, verbatim)

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Project == "" {
		return fmt.Errorf("project is required")
	}
	if c.Compose == "" {
		return fmt.Errorf("compose is required")
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("at least one entry under services is required")
	}
	for name, s := range c.Services {
		if s.ImageTag == "" {
			return fmt.Errorf("services.%s: image_tag is required", name)
		}
		switch s.Readiness.Type {
		case "http", "tcp", "vllm", "cmd":
		default:
			return fmt.Errorf("services.%s: readiness.type must be http|tcp|vllm|cmd, got %q", name, s.Readiness.Type)
		}
		if s.Readiness.Timeout != "" {
			if _, err := time.ParseDuration(s.Readiness.Timeout); err != nil {
				return fmt.Errorf("services.%s: readiness.timeout: %w", name, err)
			}
		}
		switch s.Cutover.Strategy {
		case "recreate", "proxy":
		default:
			return fmt.Errorf("services.%s: cutover.strategy must be recreate|proxy, got %q", name, s.Cutover.Strategy)
		}
		switch s.Placement.Type {
		case "", "none":
		case "gpu":
			if len(s.Placement.Pool) == 0 {
				return fmt.Errorf("services.%s: placement.pool is required for type gpu", name)
			}
			if s.Placement.VRAMMin == "" {
				return fmt.Errorf("services.%s: placement.vram_min is required for type gpu", name)
			}
			switch s.Placement.OnNoFreeGPU {
			case "", "fail", "stop-old-first":
			default:
				return fmt.Errorf("services.%s: placement.on_no_free_gpu must be fail|stop-old-first, got %q", name, s.Placement.OnNoFreeGPU)
			}
		default:
			return fmt.Errorf("services.%s: placement.type must be none|gpu, got %q", name, s.Placement.Type)
		}
	}
	return nil
}

```

- [ ] **Step 4: Run tests, vet, fmt**

Run: `go test ./config/ -v && go vet ./config/ && gofmt -l config/`
Expected: all PASS, no vet output, no unformatted files.

- [ ] **Step 5: Commit**

```bash
git add config/
git commit -m "feat: deploy.yml parsing and validation"
```

---

### Task 3: Connection (local, SSH, fake)

**Files:**
- Create: `connection/connection.go`, `connection/local.go`, `connection/ssh.go`, `connection/fake.go`, `connection/connection_test.go`

**Interfaces:**
- Consumes: `config.Target` (Task 2)
- Produces:

```go
package connection

type Connection interface {
	// Run executes cmd through `sh -c` on the target and returns combined
	// stdout; a non-zero exit returns an error including stderr.
	Run(ctx context.Context, cmd string) (string, error)
}

func New(t config.Target) Connection // Host=="" → Local, else SSH
func NewLocal() *Local
func NewSSH(host string, port int) *SSH
func NewFake() *Fake
// Fake records every command and replies from scripted responses:
type Fake struct{ Commands []string }
func (f *Fake) Stub(substr, stdout string, err error) // first matching substring wins
```

- [ ] **Step 1: Write the failing test**

`connection/connection_test.go`:

```go
package connection

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
)

func TestLocalRun(t *testing.T) {
	out, err := NewLocal().Run(context.Background(), "echo hello")
	if err != nil || strings.TrimSpace(out) != "hello" {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestLocalRunFailureIncludesStderr(t *testing.T) {
	_, err := NewLocal().Run(context.Background(), "echo oops >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("want stderr in error, got %v", err)
	}
}

func TestFakeRecordsAndStubs(t *testing.T) {
	f := NewFake()
	f.Stub("docker compose", "ok", nil)
	out, err := f.Run(context.Background(), "docker compose ps")
	if err != nil || out != "ok" {
		t.Fatalf("got %q, %v", out, err)
	}
	if len(f.Commands) != 1 || f.Commands[0] != "docker compose ps" {
		t.Fatalf("commands not recorded: %v", f.Commands)
	}
}

func TestSSHCommandLine(t *testing.T) {
	s := NewSSH("deploy@example.com", 32)
	args := s.sshArgs("docker ps")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p 32", "deploy@example.com", "ControlMaster=auto", "BatchMode=yes", "docker ps"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh args missing %q: %v", want, args)
		}
	}
}

func TestNewPicksImplementation(t *testing.T) {
	if _, ok := New(config.Target{}).(*Local); !ok {
		t.Error("empty host should give Local")
	}
	if _, ok := New(config.Target{Host: "a@b"}).(*SSH); !ok {
		t.Error("host should give SSH")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./connection/ -v`
Expected: FAIL (types undefined).

- [ ] **Step 3: Implement**

`connection/connection.go`:

```go
package connection

import (
	"context"

	"github.com/goodsmileduck/dockrail/config"
)

type Connection interface {
	Run(ctx context.Context, cmd string) (string, error)
}

func New(t config.Target) Connection {
	if t.Host == "" {
		return NewLocal()
	}
	return NewSSH(t.Host, t.Port)
}
```

`connection/local.go`:

```go
package connection

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

type Local struct{}

func NewLocal() *Local { return &Local{} }

func (l *Local) Run(ctx context.Context, cmd string) (string, error) {
	return runCmd(exec.CommandContext(ctx, "sh", "-c", cmd))
}

func runCmd(c *exec.Cmd) (string, error) {
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.String(), nil
}
```

`connection/ssh.go`:

```go
package connection

import (
	"context"
	"fmt"
	"os/exec"
)

type SSH struct {
	host string
	port int
}

func NewSSH(host string, port int) *SSH {
	if port == 0 {
		port = 22
	}
	return &SSH{host: host, port: port}
}

func (s *SSH) sshArgs(cmd string) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=~/.ssh/dockrail-%r@%h:%p",
		"-o", "ControlPersist=60s",
		"-p", fmt.Sprintf("%d", s.port),
		s.host,
		cmd,
	}
}

func (s *SSH) Run(ctx context.Context, cmd string) (string, error) {
	return runCmd(exec.CommandContext(ctx, "ssh", s.sshArgs(cmd)...))
}
```

`connection/fake.go`:

```go
package connection

import (
	"context"
	"strings"
)

type stub struct {
	substr string
	stdout string
	err    error
}

type Fake struct {
	Commands []string
	stubs    []stub
}

func NewFake() *Fake { return &Fake{} }

func (f *Fake) Stub(substr, stdout string, err error) {
	f.stubs = append(f.stubs, stub{substr, stdout, err})
}

func (f *Fake) Run(_ context.Context, cmd string) (string, error) {
	f.Commands = append(f.Commands, cmd)
	for _, s := range f.stubs {
		if strings.Contains(cmd, s.substr) {
			return s.stdout, s.err
		}
	}
	return "", nil
}
```

- [ ] **Step 4: Run tests, vet, fmt**

Run: `go test ./connection/ -v && go vet ./connection/ && gofmt -l connection/`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add connection/
git commit -m "feat: Connection interface with local, ssh, and fake implementations"
```

---

### Task 4: Preflight + `check` command

**Files:**
- Create: `engine/preflight.go`, `engine/preflight_test.go`
- Modify: `cmd/root.go` (replace the `check` stub)

**Interfaces:**
- Consumes: `config.Config` (Task 2), `connection.Connection` / `connection.Fake` (Task 3)
- Produces: `engine.Preflight(ctx context.Context, conn connection.Connection, cfg *config.Config) []error` — runs every check, returns all failures (empty slice = healthy). Checks in this plan: connectivity (`true`), docker present (`docker version --format ok`), compose plugin (`docker compose version`), compose file exists on target (`test -f <cfg.Compose>`), `nvidia-smi -L` only if any service has `placement.type: gpu`. (Registry-login and nginx checks are added by later plans.)

- [ ] **Step 1: Write the failing test**

`engine/preflight_test.go`:

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

func testCfg(gpu bool) *config.Config {
	svc := config.Service{
		ImageTag:  "t",
		Readiness: config.Readiness{Type: "http"},
		Cutover:   config.Cutover{Strategy: "recreate"},
	}
	if gpu {
		svc.Placement = config.Placement{Type: "gpu", Pool: []int{0}, VRAMMin: "1GiB"}
	}
	return &config.Config{
		Project:  "demo",
		Compose:  "docker-compose.yml",
		Services: map[string]config.Service{"web": svc},
	}
}

func TestPreflightHealthy(t *testing.T) {
	f := connection.NewFake()
	if errs := Preflight(context.Background(), f, testCfg(false)); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
	all := strings.Join(f.Commands, "\n")
	for _, want := range []string{"docker version", "docker compose version", "test -f docker-compose.yml"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing check %q in %v", want, f.Commands)
		}
	}
	if strings.Contains(all, "nvidia-smi") {
		t.Error("nvidia-smi must not run without gpu placement")
	}
}

func TestPreflightGPU(t *testing.T) {
	f := connection.NewFake()
	Preflight(context.Background(), f, testCfg(true))
	if !strings.Contains(strings.Join(f.Commands, "\n"), "nvidia-smi") {
		t.Error("nvidia-smi check missing for gpu placement")
	}
}

func TestPreflightCollectsAllFailures(t *testing.T) {
	f := connection.NewFake()
	f.Stub("docker version", "", errors.New("no docker"))
	f.Stub("test -f", "", errors.New("no file"))
	errs := Preflight(context.Background(), f, testCfg(false))
	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %v", errs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./engine/ -v`
Expected: FAIL (`Preflight` undefined).

- [ ] **Step 3: Implement**

`engine/preflight.go`:

```go
package engine

import (
	"context"
	"fmt"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type check struct {
	name string
	cmd  string
}

func Preflight(ctx context.Context, conn connection.Connection, cfg *config.Config) []error {
	checks := []check{
		{"docker", "docker version --format ok"},
		{"compose plugin", "docker compose version"},
		{"compose file", fmt.Sprintf("test -f %s", cfg.Compose)},
	}
	for _, s := range cfg.Services {
		if s.Placement.Type == "gpu" {
			checks = append(checks, check{"gpu driver", "nvidia-smi -L"})
			break
		}
	}
	var errs []error
	for _, c := range checks {
		if _, err := conn.Run(ctx, c.cmd); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
		}
	}
	return errs
}
```

- [ ] **Step 4: Wire the `check` command**

In `cmd/root.go`, replace the loop-generated `check` stub with a real command (keep the other stubs). Restructure minimally:

```go
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "dockrail",
		Short:         "Compose-native deployer with health-gated cutover",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringP("config", "c", "deploy.yml", "path to deploy.yml")

	for _, name := range []string{"deploy", "rollback", "status", "logs"} {
		name := name
		root.AddCommand(&cobra.Command{
			Use:   name,
			Short: name,
			RunE: func(cmd *cobra.Command, args []string) error {
				return fmt.Errorf("%s: not implemented", name)
			},
		})
	}
	root.AddCommand(newCheckCmd())
	return root
}

func newCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "validate config and target host readiness",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			conn := connection.New(cfg.Target)
			errs := engine.Preflight(cmd.Context(), conn, cfg)
			for _, e := range errs {
				fmt.Fprintln(cmd.ErrOrStderr(), "FAIL:", e)
			}
			if len(errs) > 0 {
				return fmt.Errorf("%d preflight check(s) failed", len(errs))
			}
			fmt.Fprintln(cmd.OutOrStdout(), "all checks passed")
			return nil
		},
	}
}
```

Add imports: `github.com/goodsmileduck/dockrail/config`, `.../connection`, `.../engine`.

- [ ] **Step 5: Run all tests**

Run: `go test ./... && go vet ./... && gofmt -l .`
Expected: PASS everywhere.

- [ ] **Step 6: Commit**

```bash
git add engine/ cmd/
git commit -m "feat: preflight checks and check command"
```

---

### Task 5: Strategy interfaces + http readiness + none placement

**Files:**
- Create: `strategy/readiness/readiness.go`, `strategy/readiness/http.go`, `strategy/readiness/http_test.go`, `strategy/placement/placement.go`

**Interfaces:**
- Consumes: `connection.Connection` (Task 3), `config.Readiness` (Task 2)
- Produces:

```go
package readiness

// Prober checks that a service instance is actually ready.
type Prober interface {
	Probe(ctx context.Context, conn connection.Connection) error
}

// New builds a Prober from config; this plan implements only "http".
func New(r config.Readiness) (Prober, error)

// HTTP probes by running curl ON THE TARGET HOST through conn, retrying
// every 2s until timeout (default 60s).
type HTTP struct{ Path string; Port int; Timeout time.Duration }
```

```go
package placement

type Placer interface {
	// Pick returns extra compose env/args for the new container; "" for none.
	Pick(ctx context.Context, conn connection.Connection) (string, error)
}
func New(p config.Placement) (Placer, error) // ""|"none" → None; "gpu" → error "not implemented" for now
type None struct{}
```

(No cutover package yet: `recreate` is a state-machine shape, not a `Switch/Revert` strategy — the cutover package arrives with `nginx-upstream` in the later plan, per spec section 6.)

- [ ] **Step 1: Write the failing test**

`strategy/readiness/http_test.go`:

```go
package readiness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestNewHTTP(t *testing.T) {
	p, err := New(config.Readiness{Type: "http", Path: "/health", Port: 8010, Timeout: "90s"})
	if err != nil {
		t.Fatal(err)
	}
	h := p.(*HTTP)
	if h.Path != "/health" || h.Port != 8010 || h.Timeout != 90*time.Second {
		t.Errorf("bad fields: %+v", h)
	}
}

func TestNewUnknownType(t *testing.T) {
	if _, err := New(config.Readiness{Type: "vllm"}); err == nil {
		t.Error("vllm not implemented in this plan; want error")
	}
}

func TestHTTPProbeSuccess(t *testing.T) {
	f := connection.NewFake()
	h := &HTTP{Path: "/health", Port: 8010, Timeout: 5 * time.Second}
	if err := h.Probe(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Commands[0], "curl") || !strings.Contains(f.Commands[0], "localhost:8010/health") {
		t.Errorf("unexpected probe command: %v", f.Commands)
	}
}

func TestHTTPProbeTimesOut(t *testing.T) {
	f := connection.NewFake()
	f.Stub("curl", "", errors.New("connection refused"))
	h := &HTTP{Path: "/health", Port: 8010, Timeout: 100 * time.Millisecond, retryEvery: 20 * time.Millisecond}
	err := h.Probe(context.Background(), f)
	if err == nil || !strings.Contains(err.Error(), "readiness") {
		t.Fatalf("want readiness timeout error, got %v", err)
	}
	if len(f.Commands) < 2 {
		t.Errorf("expected retries, got %d attempts", len(f.Commands))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./strategy/... -v`
Expected: FAIL (package undefined).

- [ ] **Step 3: Implement**

`strategy/readiness/readiness.go`:

```go
package readiness

import (
	"context"
	"fmt"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type Prober interface {
	Probe(ctx context.Context, conn connection.Connection) error
}

func New(r config.Readiness) (Prober, error) {
	switch r.Type {
	case "http":
		return newHTTP(r)
	default:
		return nil, fmt.Errorf("readiness type %q not implemented yet", r.Type)
	}
}
```

`strategy/readiness/http.go`:

```go
package readiness

import (
	"context"
	"fmt"
	"time"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type HTTP struct {
	Path       string
	Port       int
	Timeout    time.Duration
	retryEvery time.Duration
}

func newHTTP(r config.Readiness) (*HTTP, error) {
	timeout := 60 * time.Second
	if r.Timeout != "" {
		var err error
		if timeout, err = time.ParseDuration(r.Timeout); err != nil {
			return nil, err
		}
	}
	return &HTTP{Path: r.Path, Port: r.Port, Timeout: timeout, retryEvery: 2 * time.Second}, nil
}

func (h *HTTP) Probe(ctx context.Context, conn connection.Connection) error {
	cmd := fmt.Sprintf("curl -fsS -m 5 http://localhost:%d%s >/dev/null", h.Port, h.Path)
	deadline := time.Now().Add(h.Timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, lastErr = conn.Run(ctx, cmd); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.retryEvery):
		}
	}
	return fmt.Errorf("readiness probe failed after %s: %w", h.Timeout, lastErr)
}
```

`strategy/placement/placement.go`:

```go
package placement

import (
	"context"
	"fmt"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type Placer interface {
	Pick(ctx context.Context, conn connection.Connection) (string, error)
}

func New(p config.Placement) (Placer, error) {
	switch p.Type {
	case "", "none":
		return None{}, nil
	default:
		return nil, fmt.Errorf("placement type %q not implemented yet", p.Type)
	}
}

type None struct{}

func (None) Pick(context.Context, connection.Connection) (string, error) { return "", nil }
```

- [ ] **Step 4: Run tests, vet, fmt**

Run: `go test ./strategy/... -v && go vet ./... && gofmt -l .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add strategy/
git commit -m "feat: readiness and placement strategy interfaces with http and none impls"
```

---

### Task 6: Engine state machine (recreate path) + host state file

**Files:**
- Create: `engine/state.go`, `engine/engine.go`, `engine/engine_test.go`

**Interfaces:**
- Consumes: `connection.Connection`/`Fake`, `config.*`, `readiness.New`, `placement.New`
- Produces:

```go
package engine

type State struct { // JSON, stored at ~/.dockrail/<project>/state.json on the HOST
	PreviousTag  string `json:"previous_tag"`
	CurrentTag   string `json:"current_tag"`
	LastFailure  string `json:"last_failure,omitempty"` // human-readable, empty = last deploy OK
}
func loadState(ctx, conn, project string) (State, error)  // missing file → zero State
func saveState(ctx, conn, project string, s State) error  // mkdir -p + cat > via heredoc
func acquireLock(ctx, conn, project string) (release func(), err error)
// mkdir-based: `mkdir ~/.dockrail/<project>/lock` fails atomically if it
// exists → "another deploy is running" error; release = `rmdir` the dir.

type Engine struct{ Conn connection.Connection; Cfg *config.Config; Out io.Writer }
// Deploy runs the recreate sequence for every service (proxy → error for now):
//  0. preflight
//  1. docker compose -f <compose> pull <svc>            (TAG env var = image_tag)
//  2. read state, record current tag as rollback anchor
//  s. stop OLD:  docker compose -f <compose> stop <svc>
//  s. start NEW: docker compose -f <compose> up -d --no-deps <svc>
//  5. readiness probe; on failure: save state with LastFailure, return error
//  8. save state (PreviousTag=old CurrentTag, CurrentTag=new), prune images
func (e *Engine) Deploy(ctx context.Context) error
```

Every executed step is logged to `Out` as `step <name>: <command-or-result>` — this is the structured log format that `--dry-run` reuses (Task 7).

- [ ] **Step 1: Write the failing test**

`engine/engine_test.go`:

```go
package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func engineFixture() (*Engine, *connection.Fake) {
	f := connection.NewFake()
	cfg := &config.Config{
		Project: "demo",
		Compose: "docker-compose.yml",
		Services: map[string]config.Service{
			"web": {
				ImageTag:  "v2",
				Readiness: config.Readiness{Type: "http", Path: "/health", Port: 8010, Timeout: "1s"},
				Cutover:   config.Cutover{Strategy: "recreate"},
			},
		},
	}
	return &Engine{Conn: f, Cfg: cfg, Out: &bytes.Buffer{}}, f
}

func TestDeployHappyPathCommandOrder(t *testing.T) {
	e, f := engineFixture()
	f.Stub("state.json", `{"current_tag":"v1"}`, nil)
	if err := e.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.Commands, "\n")
	// ordered milestones of the recreate sequence:
	milestones := []string{
		"docker version",                 // preflight
		"pull",                           // step 1
		"state.json",                     // step 2: read anchor
		"stop web",                       // recreate: stop OLD
		"up -d --no-deps web",            // start NEW
		"curl",                           // step 5: probe
		"image prune",                    // step 8
	}
	last := -1
	for _, m := range milestones {
		idx := strings.Index(all, m)
		if idx < 0 {
			t.Fatalf("missing milestone %q in commands:\n%s", m, all)
		}
		if idx < last {
			t.Fatalf("milestone %q out of order in:\n%s", m, all)
		}
		last = idx
	}
	if !strings.Contains(all, `"previous_tag":"v1"`) && !strings.Contains(all, `"previous_tag": "v1"`) {
		t.Errorf("state save must record previous tag v1:\n%s", all)
	}
}

func TestDeployReadinessFailureRecordsAndErrors(t *testing.T) {
	e, f := engineFixture()
	f.Stub("curl", "", errors.New("refused"))
	err := e.Deploy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "readiness") {
		t.Fatalf("want readiness error, got %v", err)
	}
	all := strings.Join(f.Commands, "\n")
	if !strings.Contains(all, "last_failure") {
		t.Error("failure must be persisted to state file")
	}
}

func TestDeployHeldLockFailsFast(t *testing.T) {
	e, f := engineFixture()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	err := e.Deploy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "another deploy") {
		t.Fatalf("want lock error, got %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "pull") {
		t.Error("deploy must not proceed while lock is held")
	}
}

func TestDeployReleasesLock(t *testing.T) {
	e, f := engineFixture()
	if err := e.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"), "rmdir $HOME/.dockrail/demo/lock") {
		t.Error("lock not released after deploy")
	}
}

func TestDeployProxyStrategyNotImplemented(t *testing.T) {
	e, _ := engineFixture()
	svc := e.Cfg.Services["web"]
	svc.Cutover.Strategy = "proxy"
	e.Cfg.Services["web"] = svc
	err := e.Deploy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("want not-implemented error for proxy, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./engine/ -v`
Expected: FAIL (`Engine` undefined). Preflight tests from Task 4 still PASS.

- [ ] **Step 3: Implement state file**

`engine/state.go`:

```go
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goodsmileduck/dockrail/connection"
)

type State struct {
	PreviousTag string `json:"previous_tag"`
	CurrentTag  string `json:"current_tag"`
	LastFailure string `json:"last_failure,omitempty"`
}

func statePath(project string) string {
	return fmt.Sprintf("$HOME/.dockrail/%s/state.json", project)
}

func loadState(ctx context.Context, conn connection.Connection, project string) (State, error) {
	out, err := conn.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null || true", statePath(project)))
	var s State
	if err != nil {
		return s, err
	}
	if strings.TrimSpace(out) == "" {
		return s, nil // first deploy
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return s, fmt.Errorf("corrupt state file: %w", err)
	}
	return s, nil
}

func saveState(ctx context.Context, conn connection.Connection, project string, s State) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	dir := fmt.Sprintf("$HOME/.dockrail/%s", project)
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s <<'DDEOF'\n%s\nDDEOF", dir, statePath(project), raw)
	_, err = conn.Run(ctx, cmd)
	return err
}

func acquireLock(ctx context.Context, conn connection.Connection, project string) (func(), error) {
	lockDir := fmt.Sprintf("$HOME/.dockrail/%s/lock", project)
	mk := fmt.Sprintf("mkdir -p $HOME/.dockrail/%s && mkdir %s", project, lockDir)
	if _, err := conn.Run(ctx, mk); err != nil {
		return nil, fmt.Errorf("another deploy appears to be running (lock %s held): %w", lockDir, err)
	}
	release := func() {
		_, _ = conn.Run(context.Background(), fmt.Sprintf("rmdir %s", lockDir))
	}
	return release, nil
}
```

- [ ] **Step 4: Implement engine**

`engine/engine.go`:

```go
package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/strategy/readiness"
)

type Engine struct {
	Conn connection.Connection
	Cfg  *config.Config
	Out  io.Writer
}

func (e *Engine) logf(format string, a ...any) {
	fmt.Fprintf(e.Out, format+"\n", a...)
}

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
	for name, svc := range e.Cfg.Services {
		if err := e.deployService(ctx, name, svc); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
	}
	return nil
}

func (e *Engine) deployService(ctx context.Context, name string, svc config.Service) error {
	if svc.Cutover.Strategy != "recreate" {
		return fmt.Errorf("cutover strategy %q not implemented yet", svc.Cutover.Strategy)
	}
	prober, err := readiness.New(svc.Readiness)
	if err != nil {
		return err
	}
	compose := fmt.Sprintf("TAG=%s docker compose -f %s", svc.ImageTag, e.Cfg.Compose)

	e.logf("step pull: %s tag %s", name, svc.ImageTag)
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
		st.LastFailure = fmt.Sprintf("deploy %s tag %s: %v", name, svc.ImageTag, err)
		_ = saveState(ctx, e.Conn, e.Cfg.Project, st)
		return err
	}

	e.logf("step finalize")
	st.PreviousTag, st.CurrentTag, st.LastFailure = st.CurrentTag, svc.ImageTag, ""
	if err := saveState(ctx, e.Conn, e.Cfg.Project, st); err != nil {
		return err
	}
	if _, err := e.Conn.Run(ctx, "docker image prune -f"); err != nil {
		e.logf("warn: prune failed: %v", err)
	}
	e.logf("deployed %s tag %s", name, svc.ImageTag)
	return nil
}
```

Note for implementer: `TAG=<tag>` env prefix assumes the compose file references `image: ...:${TAG}`; for pinned tags in deploy.yml the same mechanism works because `image_tag` is resolved config-side. Multi-service state currently stores one tag pair per project (matches single-service dogfood); per-service state arrives with the rollback plan.

- [ ] **Step 5: Run tests, vet, fmt**

Run: `go test ./engine/ -v && go vet ./... && gofmt -l .`
Expected: all three engine tests PASS.

- [ ] **Step 6: Commit**

```bash
git add engine/
git commit -m "feat: deploy engine with recreate sequence and host state file"
```

---

### Task 7: `deploy` command + `--dry-run`

**Files:**
- Create: `cmd/deploy.go`, `cmd/deploy_test.go`
- Modify: `cmd/root.go` (remove `deploy` from the stub loop, add `newDeployCmd()`)

**Interfaces:**
- Consumes: `engine.Engine`, `engine.Preflight`, `config.Load`, `connection.New`
- Produces: `dockrail deploy [-c deploy.yml] [--dry-run]`. Dry-run runs config load + preflight + anchor read against the real connection (read-only), then prints the plan (`plan pull ...`, `plan recreate ...`) WITHOUT executing pull/stop/up/probe/save.

- [ ] **Step 1: Write the failing test**

`cmd/deploy_test.go`:

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

func TestDryRunPrintsPlanWithoutMutating(t *testing.T) {
	f := connection.NewFake()
	cfg := &config.Config{
		Project: "demo", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {
			ImageTag:  "v2",
			Readiness: config.Readiness{Type: "http", Path: "/health", Port: 8010},
			Cutover:   config.Cutover{Strategy: "recreate"},
		}},
	}
	var out bytes.Buffer
	if err := runDeploy(context.Background(), f, cfg, &out, true); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"plan pull web tag v2", "plan recreate web", "plan readiness http :8010/health"} {
		if !strings.Contains(text, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, text)
		}
	}
	for _, c := range f.Commands {
		for _, forbidden := range []string{"pull", "up -d", "stop", "curl", "DDEOF"} {
			if strings.Contains(c, forbidden) {
				t.Errorf("dry-run executed mutating command: %q", c)
			}
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/ -v`
Expected: FAIL (`runDeploy` undefined).

- [ ] **Step 3: Implement**

`cmd/deploy.go`:

```go
package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/engine"
)

func newDeployCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "deploy",
		Short: "deploy the project to the target host",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			conn := connection.New(cfg.Target)
			return runDeploy(cmd.Context(), conn, cfg, cmd.OutOrStdout(), dryRun)
		},
	}
	c.Flags().Bool("dry-run", false, "print the deploy plan without changing anything")
	return c
}

func runDeploy(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer, dryRun bool) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	if !dryRun {
		return e.Deploy(ctx)
	}
	fmt.Fprintln(out, "dry-run: no changes will be made")
	if errs := engine.Preflight(ctx, conn, cfg); len(errs) > 0 {
		return fmt.Errorf("preflight failed: %v", errs)
	}
	for name, svc := range cfg.Services {
		fmt.Fprintf(out, "plan pull %s tag %s\n", name, svc.ImageTag)
		fmt.Fprintf(out, "plan recreate %s (stop old, up -d --no-deps)\n", name)
		fmt.Fprintf(out, "plan readiness %s :%d%s timeout %s\n",
			svc.Readiness.Type, svc.Readiness.Port, svc.Readiness.Path, svc.Readiness.Timeout)
	}
	return nil
}
```

In `cmd/root.go`, remove `"deploy"` from the stub loop and add `root.AddCommand(newDeployCmd())`.

- [ ] **Step 4: Run everything**

Run: `go test ./... -v && go vet ./... && gofmt -l . && go build -o /tmp/claude-1000/-home-goodsmileduck-local-personal-dockrail/b608f664-cc26-4d72-abab-76d5765d9cda/scratchpad/dockrail .`
Expected: all PASS, binary builds.

- [ ] **Step 5: Smoke test end-to-end locally**

Create a scratch compose project (in the scratchpad, not the repo):

```bash
S=/tmp/claude-1000/-home-goodsmileduck-local-personal-dockrail/b608f664-cc26-4d72-abab-76d5765d9cda/scratchpad/smoke
mkdir -p $S && cd $S
cat > docker-compose.yml <<'EOF'
services:
  web:
    image: nginx:${TAG}
    ports: ["18080:80"]
EOF
cat > deploy.yml <<'EOF'
project: smoke
compose: docker-compose.yml
services:
  web:
    image_tag: "alpine"
    readiness: { type: http, path: /, port: 18080, timeout: 30s }
    cutover:   { strategy: recreate }
EOF
/tmp/claude-1000/-home-goodsmileduck-local-personal-dockrail/b608f664-cc26-4d72-abab-76d5765d9cda/scratchpad/dockrail deploy --dry-run
/tmp/claude-1000/-home-goodsmileduck-local-personal-dockrail/b608f664-cc26-4d72-abab-76d5765d9cda/scratchpad/dockrail deploy
curl -fsS localhost:18080 >/dev/null && echo SMOKE-OK
docker compose -f docker-compose.yml down
```

Expected: dry-run prints the plan; real deploy pulls, recreates, probes; `SMOKE-OK`; state file exists at `~/.dockrail/smoke/state.json` with `"current_tag":"alpine"`.

- [ ] **Step 6: Commit**

```bash
git add cmd/
git commit -m "feat: deploy command with --dry-run plan output"
```
