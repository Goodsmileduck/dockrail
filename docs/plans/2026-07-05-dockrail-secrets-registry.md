# dockrail Secrets + Registry Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `registry` and `secrets.from_env` — already parsed by `config` but ignored by the engine — actually drive `docker login` and secret injection, so dockrail can deploy private-registry images that need runtime secrets.

**Architecture:** Two additions to the deploy path, both host-side and connection-agnostic. (1) Before any `compose pull`, if `registry.server` is set and registry credentials are present in dockrail's *own* environment, run `docker login` on the target via `--password-stdin`. (2) Named secrets in `secrets.from_env` are read from dockrail's own environment at invocation time and written once to a mode-600 remote env-file, which every `compose` command sources. This matches the dogfood project's model: secrets live in the invoking shell / CI environment (`APP_*`, `x-admin-key`), never in committed yaml, and a non-interactive SSH shell would otherwise not have them.

**Tech Stack:** Go 1.26, existing `engine`/`config`/`connection` packages, stdlib `os`. No new dependencies.

## Global Constraints

- Module `github.com/goodsmileduck/dockrail`; Go `1.26.0` / toolchain `go1.26.4`. Local: `GOTOOLCHAIN=auto`.
- No secret value may appear in a command's *arguments* on the target (argv is visible in `ps`). Secrets are sourced from a remote env-file, not passed as `VAR=val docker compose`.
- Missing required secrets MUST fail the deploy before any host mutation (fail-fast), naming the missing variable.
- Registry login is best-effort-conditional: perform it only when `registry.server` is set AND both credential env vars are present; otherwise skip (the host may already be authenticated) and log that it was skipped.
- Registry credentials are read from dockrail's environment via fixed names: `DOCKRAIL_REGISTRY_USER`, `DOCKRAIL_REGISTRY_PASSWORD`. They are never read from yaml.
- All new host commands must be issued through `connection.Connection.Run` so the `Fake` records them and dry-run/MCP preview works.

---

### Task 1: Read and validate secrets from dockrail's environment

**Files:**
- Create: `engine/secrets.go`
- Test: `engine/secrets_test.go`

**Interfaces:**
- Consumes: `config.Config.Secrets.FromEnv []string`.
- Produces: `func collectSecrets(names []string) (map[string]string, error)` — returns name→value for each requested env var, erroring on the first that is unset or empty. Later tasks consume the returned map.

- [ ] **Step 1: Write the failing test**

```go
package engine

import (
	"strings"
	"testing"
)

func TestCollectSecretsErrorsOnMissing(t *testing.T) {
	t.Setenv("APP_API_KEY", "abc")
	_, err := collectSecrets([]string{"APP_API_KEY", "APP_DB_CONNECTION_URL"})
	if err == nil || !strings.Contains(err.Error(), "APP_DB_CONNECTION_URL") {
		t.Fatalf("want missing-var error naming the var, got %v", err)
	}
}

func TestCollectSecretsReturnsValues(t *testing.T) {
	t.Setenv("APP_API_KEY", "abc")
	got, err := collectSecrets([]string{"APP_API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if got["APP_API_KEY"] != "abc" {
		t.Fatalf("wrong value: %v", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./engine/... -run TestCollectSecrets -v`
Expected: FAIL — `collectSecrets` undefined.

- [ ] **Step 3: Implement `engine/secrets.go`**

```go
package engine

import (
	"fmt"
	"os"
)

// collectSecrets reads each named variable from dockrail's own environment
// (the invoking shell / CI job). the dogfood project keeps these in ~/.bashrc/.env; a
// non-interactive SSH shell on the target would not have them, so dockrail
// forwards them explicitly. An unset or empty required secret is fatal.
func collectSecrets(names []string) (map[string]string, error) {
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

- [ ] **Step 4: Run the test**

Run: `GOTOOLCHAIN=auto go test ./engine/... -run TestCollectSecrets -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add engine/secrets.go engine/secrets_test.go
git commit -m "feat: collect deploy secrets from dockrail environment"
```

---

### Task 2: Write secrets to a mode-600 remote env-file and source it in compose commands

**Files:**
- Modify: `engine/secrets.go` (add remote-write helper)
- Modify: `engine/engine.go` (build the compose prefix from the env-file)
- Test: `engine/secrets_test.go`, `engine/engine_test.go`

**Interfaces:**
- Consumes: `collectSecrets`, `connection.Connection.Run`.
- Produces: `func writeSecretsFile(ctx, conn, project string, secrets map[string]string) (composePrefix string, err error)`. When there are no secrets it returns the empty string and writes nothing. When there are, it writes `$HOME/.dockrail/<project>/env` (chmod 600) and returns a prefix like `set -a; . $HOME/.dockrail/demo/env; set +a; ` that later `compose` commands prepend.

- [ ] **Step 1: Write the failing test**

```go
func TestWriteSecretsFileSourcesNotArgv(t *testing.T) {
	f := connection.NewFake()
	prefix, err := writeSecretsFile(context.Background(), f, "demo",
		map[string]string{"APP_API_KEY": "s3cr3t"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prefix, ".dockrail/demo/env") {
		t.Fatalf("prefix must source the env-file, got %q", prefix)
	}
	// The write happens; the secret value must not leak into the prefix that
	// gets prepended to every later command (only the file path may).
	if strings.Contains(prefix, "s3cr3t") {
		t.Fatalf("secret value leaked into command prefix: %q", prefix)
	}
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "chmod 600") {
		t.Fatalf("env-file must be chmod 600:\n%s", joined)
	}
}

func TestWriteSecretsFileEmptyIsNoop(t *testing.T) {
	f := connection.NewFake()
	prefix, err := writeSecretsFile(context.Background(), f, "demo", nil)
	if err != nil || prefix != "" {
		t.Fatalf("empty secrets must be a no-op, got prefix=%q err=%v", prefix, err)
	}
	if len(f.Commands) != 0 {
		t.Fatalf("no commands expected, got %v", f.Commands)
	}
}
```

Add imports (`context`, `connection`) to the test file as needed.

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./engine/... -run TestWriteSecretsFile -v`
Expected: FAIL — `writeSecretsFile` undefined.

- [ ] **Step 3: Implement the remote-write helper in `engine/secrets.go`**

```go
import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/goodsmileduck/dockrail/connection"
)

// writeSecretsFile writes the collected secrets to a mode-600 env-file on the
// target and returns a shell prefix that sources it. Secrets reach the target
// only inside this heredoc write (like state.json), never as command argv on
// later compose invocations. Returns "" and writes nothing for no secrets.
func writeSecretsFile(ctx context.Context, conn connection.Connection, project string, secrets map[string]string) (string, error) {
	if len(secrets) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(secrets))
	for n := range secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		// single-quote the value; escape embedded single quotes for POSIX sh
		v := strings.ReplaceAll(secrets[n], `'`, `'\''`)
		fmt.Fprintf(&b, "export %s='%s'\n", n, v)
	}
	dir := fmt.Sprintf("$HOME/.dockrail/%s", project)
	path := dir + "/env"
	cmd := fmt.Sprintf("mkdir -p %s && umask 177 && cat > %s <<'DDEOF'\n%sDDEOF\nchmod 600 %s",
		dir, path, b.String(), path)
	if _, err := conn.Run(ctx, cmd); err != nil {
		return "", fmt.Errorf("write secrets file: %w", err)
	}
	return fmt.Sprintf("set -a; . %s; set +a; ", path), nil
}
```

- [ ] **Step 4: Thread the prefix into the compose commands in `engine/engine.go`**

In both `Deploy` and `Rollback`, after loading state and before the service loop, collect secrets and write the file once; pass the prefix down to `recreate`. Change `recreate`'s signature to accept the prefix and prepend it to the `compose` string:

```go
// in Deploy, after anchor read:
secrets, err := collectSecrets(e.Cfg.Secrets.FromEnv)
if err != nil {
	return e.recordFailure(ctx, st, fmt.Sprintf("secrets: %v", err), err)
}
prefix, err := writeSecretsFile(ctx, e.Conn, e.Cfg.Project, secrets)
if err != nil {
	return e.recordFailure(ctx, st, fmt.Sprintf("secrets: %v", err), err)
}
// pass prefix into each e.recreate(ctx, name, svc, svc.ImageTag, prefix)
```

In `recreate`, change:

```go
compose := fmt.Sprintf("%sTAG=%s docker compose -f %s", prefix, tag, e.Cfg.Compose)
```

Apply the identical collect+write+pass sequence in `Rollback`.

- [ ] **Step 5: Update existing engine tests for the new `recreate` signature and no-secrets default**

The happy-path config has no `secrets.from_env`, so `prefix` is `""` and all existing milestone assertions still hold. Update any direct `recreate` calls in tests to pass `""`. Run the suite:

Run: `GOTOOLCHAIN=auto go test ./engine/... -v`
Expected: PASS (existing behavior unchanged when no secrets configured).

- [ ] **Step 6: Add a milestone test that secrets are written before pull**

```go
func TestDeployWritesSecretsBeforePull(t *testing.T) {
	t.Setenv("APP_API_KEY", "s3cr3t")
	e, f := engineFixture()
	e.Cfg.Secrets.FromEnv = []string{"APP_API_KEY"}
	f.Stub("state.json", `{"current_tag":"v1"}`, nil)
	if err := e.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.Commands, "\n")
	envIdx := strings.Index(all, ".dockrail/demo/env")
	pullIdx := strings.Index(all, "pull")
	if envIdx < 0 || pullIdx < 0 || envIdx > pullIdx {
		t.Fatalf("env-file must be written before pull:\n%s", all)
	}
	// every compose command must source the env-file
	if !strings.Contains(all, "set -a; . $HOME/.dockrail/demo/env") {
		t.Fatalf("compose commands must source secrets:\n%s", all)
	}
}
```

Run: `GOTOOLCHAIN=auto go test ./engine/... -run TestDeployWritesSecrets -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add engine/
git commit -m "feat: inject deploy secrets via mode-600 remote env-file"
```

---

### Task 3: `docker login` to a private registry before pull

**Files:**
- Create: `engine/registry.go`
- Modify: `engine/engine.go` (call login in Deploy/Rollback before the service loop)
- Test: `engine/registry_test.go`

**Interfaces:**
- Consumes: `config.Config.Registry.Server`, env `DOCKRAIL_REGISTRY_USER`/`DOCKRAIL_REGISTRY_PASSWORD`, `connection.Connection.Run`.
- Produces: `func registryLogin(ctx, conn, reg config.Registry, out io.Writer) error` — logs in when server + both creds are present; no-op (with a logged note) otherwise.

- [ ] **Step 1: Write the failing test**

```go
package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestRegistryLoginRunsWithCreds(t *testing.T) {
	t.Setenv("DOCKRAIL_REGISTRY_USER", "u")
	t.Setenv("DOCKRAIL_REGISTRY_PASSWORD", "p")
	f := connection.NewFake()
	var log bytes.Buffer
	if err := registryLogin(context.Background(), f, config.Registry{Server: "registry.gitlab.com"}, &log); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.Commands, "\n")
	if !strings.Contains(all, "docker login registry.gitlab.com") || !strings.Contains(all, "--password-stdin") {
		t.Fatalf("expected password-stdin login, got:\n%s", all)
	}
	// password must not appear as an argument
	if strings.Contains(all, "-p p") || strings.Contains(all, "--password p") {
		t.Fatalf("password leaked into argv:\n%s", all)
	}
}

func TestRegistryLoginSkipsWithoutCreds(t *testing.T) {
	f := connection.NewFake()
	var log bytes.Buffer
	if err := registryLogin(context.Background(), f, config.Registry{Server: "registry.gitlab.com"}, &log); err != nil {
		t.Fatal(err)
	}
	if len(f.Commands) != 0 {
		t.Fatalf("login must be skipped without creds, got %v", f.Commands)
	}
	if !strings.Contains(log.String(), "skip") {
		t.Fatalf("skip must be logged, got %q", log.String())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=auto go test ./engine/... -run TestRegistryLogin -v`
Expected: FAIL — `registryLogin` undefined.

- [ ] **Step 3: Implement `engine/registry.go`**

```go
package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

// registryLogin authenticates the target to a private registry before pulls.
// It runs only when a server is configured and both credential env vars are
// present in dockrail's environment; otherwise it is a logged no-op (the host
// may already be authenticated). The password is piped via --password-stdin,
// never placed in argv.
func registryLogin(ctx context.Context, conn connection.Connection, reg config.Registry, out io.Writer) error {
	if reg.Server == "" {
		return nil
	}
	user, uok := os.LookupEnv("DOCKRAIL_REGISTRY_USER")
	pass, pok := os.LookupEnv("DOCKRAIL_REGISTRY_PASSWORD")
	if !uok || !pok || user == "" || pass == "" {
		fmt.Fprintf(out, "registry: no DOCKRAIL_REGISTRY_USER/PASSWORD set — skip login, assuming host is authenticated to %s\n", reg.Server)
		return nil
	}
	// escape single quotes in the password for the POSIX printf
	esc := strings.ReplaceAll(pass, `'`, `'\''`)
	cmd := fmt.Sprintf("printf '%%s' '%s' | docker login %s --username %s --password-stdin",
		esc, reg.Server, user)
	if _, err := conn.Run(ctx, cmd); err != nil {
		return fmt.Errorf("docker login %s: %w", reg.Server, err)
	}
	return nil
}
```

> Note: the password appears once inside the piped `printf` string (like the secrets heredoc in Task 2), not as a `docker login` argument. This keeps it out of `docker`'s process argv; the residual exposure is the same as the state/env writes and acceptable for v1.

- [ ] **Step 4: Call login in `Deploy` and `Rollback`**

Immediately after `writeSecretsFile` (before the service loop) in both methods:

```go
if err := registryLogin(ctx, e.Conn, e.Cfg.Registry, e.Out); err != nil {
	return e.recordFailure(ctx, st, fmt.Sprintf("registry login: %v", err), err)
}
```

- [ ] **Step 5: Run the full suite and build**

Run: `GOTOOLCHAIN=auto go test ./... && GOTOOLCHAIN=auto go build ./...`
Expected: PASS. Existing tests unaffected (no `registry.server` in fixtures → login no-op with no commands).

- [ ] **Step 6: Commit**

```bash
git add engine/
git commit -m "feat: docker login to private registry before pull"
```

---

### Task 4: Document secrets + registry

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document the env-var contract**

Add a "Secrets & private registries" section: `secrets.from_env` lists variable names dockrail reads from *its own* environment (CI job / shell) and forwards to the target via a mode-600 env-file every compose command sources — required vars missing → deploy aborts before any change. `registry.server` + `DOCKRAIL_REGISTRY_USER`/`DOCKRAIL_REGISTRY_PASSWORD` trigger a `docker login` before pull; without those creds dockrail assumes the host is already authenticated. Note secret values are written to the target (env-file / login pipe) but never passed as command arguments.

- [ ] **Step 2: Verify and commit**

Run: `GOTOOLCHAIN=auto gofmt -l . && GOTOOLCHAIN=auto go test ./...`
Expected: no gofmt output; PASS.

```bash
git add README.md
git commit -m "docs: document secrets and registry configuration"
```

---

## Self-Review Notes

- **Secret exposure boundary:** values reach the target inside heredoc/pipe writes only, never as argv on `pull`/`up`/`login`. The remaining exposure (the write command itself, and `Fake.Commands` capturing it in dry-run/MCP) is called out as acceptable-for-v1; a hardened follow-up could redact. This is the one deliberate trade-off — flag it in review, don't silently widen it.
- **Fail-fast ordering:** secret collection happens after the lock/anchor but before pull, and a missing secret goes through `recordFailure` so state records the reason and nothing on the host is mutated.
- **Backward compatibility:** every new step is a no-op when `secrets.from_env` is empty and `registry.server` is unset — the existing fixtures exercise exactly that path, so all prior tests must still pass unchanged.
- **Spec coverage:** collect+validate ✅ (T1), inject without argv leak ✅ (T2), private-registry login ✅ (T3), docs ✅ (T4).
