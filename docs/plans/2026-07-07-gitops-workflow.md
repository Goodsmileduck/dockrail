# GitOps Workflow (Step 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** PR-driven deploys — a `vars:` block in `deploy.yml`, a composite GitHub Action that runs `dockrail deploy` (dry-run plan on PRs, real deploy on merge), and a docs page covering the workflow including a GitLab CI equivalent.

**Architecture:** Spec: `docs/specs/2026-07-07-gitops-workflow-design.md`. No engine changes. Task 1 adds a single-pass text interpolation (`${vars.name}`) in `config.Load` before strict YAML decode. Task 2 adds `action/action.yml` (composite action) plus a smoke-test workflow that builds the binary from source and exercises the action against a local-exec fixture. Task 3 is documentation.

**Tech Stack:** Go 1.26.4, `gopkg.in/yaml.v3`, GitHub composite actions (bash).

## Global Constraints

- Module path: `github.com/goodsmileduck/dockrail`.
- Strict YAML schema stays: `dec.KnownFields(true)` must still reject unknown fields *after* interpolation.
- `vars:` supports ONLY `${vars.name}` references in string values and the `$${` escape. No env lookups, no defaults (`:-`), no recursion (spec section "Config: `vars:` block").
- Secrets never flow through `vars:` — they remain `secrets.from_env` / host `env_file` (D8).
- The repo is public: examples use `example.com` / placeholder names only, never internal hosts.
- Don't use the `§` symbol in docs; write "section N".
- `gofmt` clean; run `go vet ./...` before each commit.

---

### Task 1: `vars:` interpolation in config

**Files:**
- Create: `config/vars.go`
- Create: `config/vars_test.go`
- Modify: `config/config.go` (add `Vars` field to `Config`; call `interpolate` in `Load`)

**Interfaces:**
- Consumes: `config.Load(path string) (*Config, error)` (existing).
- Produces: `interpolate(raw []byte) ([]byte, error)` (package-private); `Config.Vars map[string]string` with yaml tag `vars`. Behavior relied on by Task 2's fixture: `image_tag: "${vars.tag}"` resolves before validation.

- [ ] **Step 1: Write the failing tests**

Create `config/vars_test.go`:

```go
package config

import (
	"strings"
	"testing"
)

const varsYAML = `
project: demo
compose: docker-compose.yml
vars:
  tag: "v42"
  port: "8010"
target: { host: deploy@example.com }
services:
  web:
    image_tag: "${vars.tag}"
    readiness: { type: http, path: /health, port: ${vars.port}, timeout: 90s }
    cutover:   { strategy: recreate }
`

func TestVarsInterpolation(t *testing.T) {
	cfg, err := Load(write(t, varsYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Services["web"].ImageTag; got != "v42" {
		t.Errorf("image_tag = %q, want v42", got)
	}
	if got := cfg.Services["web"].Readiness.Port; got != 8010 {
		t.Errorf("readiness.port = %d, want 8010", got)
	}
}

func TestVarsUndefinedReferenceFails(t *testing.T) {
	yaml := strings.Replace(varsYAML, "${vars.tag}", "${vars.missing}", 1)
	_, err := Load(write(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "vars.missing") {
		t.Fatalf("want undefined-variable error naming vars.missing, got %v", err)
	}
}

func TestVarsEscapeLiteral(t *testing.T) {
	yaml := strings.Replace(varsYAML, `image_tag: "${vars.tag}"`, `image_tag: "$${vars.tag}"`, 1)
	cfg, err := Load(write(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Services["web"].ImageTag; got != "${vars.tag}" {
		t.Errorf("image_tag = %q, want literal ${vars.tag}", got)
	}
}

func TestVarsNoRecursion(t *testing.T) {
	// A var value containing a reference is inserted verbatim, never
	// re-expanded: image_tag must NOT become "8010".
	// (The lenient pre-parse reads tag's raw value "${vars.port}"; the
	// single text pass inserts it into image_tag without re-scanning.)
	yaml := strings.Replace(varsYAML, `tag: "v42"`, `tag: "${vars.port}"`, 1)
	cfg, err := Load(write(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Services["web"].ImageTag; got != "${vars.port}" {
		t.Errorf("image_tag = %q, want un-expanded ${vars.port}", got)
	}
}

func TestVarsValueWithNewlineRejected(t *testing.T) {
	yaml := strings.Replace(varsYAML, `tag: "v42"`, "tag: \"v42\\ninjected: true\"", 1)
	_, err := Load(write(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "newline") {
		t.Fatalf("want newline rejection, got %v", err)
	}
}

func TestVarsStrictSchemaStillApplies(t *testing.T) {
	// A typo'd field is rejected even when the file uses vars.
	yaml := strings.Replace(varsYAML, "image_tag:", "image_tags:", 1)
	_, err := Load(write(t, yaml))
	if err == nil {
		t.Fatal("want unknown-field error, got nil")
	}
}

func TestNoVarsBlockStillWorks(t *testing.T) {
	// Existing configs without vars are untouched (regression guard).
	if _, err := Load(write(t, validYAML)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./config/ -run TestVars -v`
Expected: FAIL — `TestVarsInterpolation` fails parsing (`${vars.tag}` is not valid for the schema / unknown field `vars`).

- [ ] **Step 3: Implement interpolation**

Create `config/vars.go`:

```go
package config

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// varRefRe matches either the escape sequence $${ or a variable reference
// ${vars.name}. Anything else (including other ${...} forms) is left alone —
// deploy.yml has no env interpolation by design; compose handles its own.
var varRefRe = regexp.MustCompile(`\$\$\{|\$\{vars\.([A-Za-z0-9_-]+)\}`)

// interpolate performs a single substitution pass over the raw deploy.yml
// text: ${vars.name} is replaced with the value from the top-level vars:
// block, $${ yields a literal ${. Values are inserted verbatim — never
// re-scanned — so there is no recursion. Referencing an undefined variable
// is a hard error; declaring an unused one is fine.
func interpolate(raw []byte) ([]byte, error) {
	// Lenient pre-parse: pull out just the vars block. The strict,
	// KnownFields decode happens later in Load on the substituted text.
	var head struct {
		Vars map[string]string `yaml:"vars"`
	}
	if err := yaml.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("parse vars: %w", err)
	}
	// Values are spliced into YAML source text; a newline could inject
	// sibling keys, so reject it outright.
	for k, v := range head.Vars {
		if strings.Contains(v, "\n") {
			return nil, fmt.Errorf("vars.%s: value must not contain a newline", k)
		}
	}
	var missing []string
	out := varRefRe.ReplaceAllStringFunc(string(raw), func(m string) string {
		if m == "$${" {
			return "${"
		}
		name := varRefRe.FindStringSubmatch(m)[1]
		v, ok := head.Vars[name]
		if !ok {
			missing = append(missing, "vars."+name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("undefined variable(s): %s", strings.Join(missing, ", "))
	}
	return []byte(out), nil
}
```

Modify `config/config.go`:

1. Add the field to `Config` (after `Project`):

```go
	Vars     map[string]string  `yaml:"vars"`
```

2. In `Load`, after `os.ReadFile` succeeds and before the decoder is built:

```go
	raw, err = interpolate(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
```

- [ ] **Step 4: Run the full test suite**

Run: `go vet ./... && go test ./...`
Expected: PASS (all packages, including the pre-existing config tests).

- [ ] **Step 5: Commit**

```bash
git add config/vars.go config/vars_test.go config/config.go
git commit -m "feat(config): add vars: block with \${vars.name} interpolation"
```

---

### Task 2: `dockrail-deploy` composite GitHub Action + smoke test

**Files:**
- Create: `action/action.yml`
- Create: `action/testdata/deploy.yml`
- Create: `action/testdata/compose.yml`
- Create: `.github/workflows/action-smoke.yml`

**Interfaces:**
- Consumes: `dockrail deploy -c <path> [--dry-run]` CLI (existing); release asset names `dockrail-linux-amd64` + `checksums.txt` (from `.github/workflows/release.yml`); Task 1's `${vars.name}` interpolation (fixture uses it).
- Produces: action inputs `version`, `config`, `ssh-key`, `known-hosts`, `dry-run`, `extra-args`, `binary-path`. Referenced by Task 3's docs.

- [ ] **Step 1: Write the fixture config and compose file**

Create `action/testdata/compose.yml`:

```yaml
services:
  web:
    image: registry.example.com/demo/web:${TAG:-latest}
    ports:
      - "8080:8080"
```

Create `action/testdata/deploy.yml` (exercises vars + local exec — no `target.host` means local):

```yaml
project: action-smoke
compose: action/testdata/compose.yml
vars:
  tag: "v1"
services:
  web:
    image_tag: "${vars.tag}"
    readiness: { type: http, path: /health, port: 8080, timeout: 30s }
    cutover: { strategy: recreate }
```

- [ ] **Step 2: Write the composite action**

Create `action/action.yml`:

```yaml
name: "dockrail deploy"
description: "Deploy a Docker Compose project with dockrail (health-gated, zero-downtime cutover)"
inputs:
  version:
    description: "dockrail release tag to install (e.g. v0.3.0). Ignored when binary-path is set."
    required: false
    default: ""
  binary-path:
    description: "Path to a pre-built dockrail binary (skips download; used for testing)."
    required: false
    default: ""
  config:
    description: "Path to deploy.yml"
    required: false
    default: "deploy.yml"
  ssh-key:
    description: "Private SSH key for the target host (pass a secret). Omit for local-exec targets."
    required: false
    default: ""
  known-hosts:
    description: "known_hosts entry line(s) for the target host."
    required: false
    default: ""
  dry-run:
    description: "Run deploy --dry-run and publish the plan to the step summary (use on pull requests)."
    required: false
    default: "false"
  extra-args:
    description: "Extra arguments appended to dockrail deploy (e.g. --lock-wait 5m once available)."
    required: false
    default: ""
runs:
  using: composite
  steps:
    - name: Install dockrail
      shell: bash
      run: |
        set -euo pipefail
        if [ -n "${{ inputs.binary-path }}" ]; then
          install -m 0755 "${{ inputs.binary-path }}" "$RUNNER_TEMP/dockrail"
          exit 0
        fi
        if [ -z "${{ inputs.version }}" ]; then
          echo "::error::either 'version' or 'binary-path' input is required" >&2
          exit 1
        fi
        base="https://github.com/goodsmileduck/dockrail/releases/download/${{ inputs.version }}"
        cd "$RUNNER_TEMP"
        curl -fsSLO "${base}/dockrail-linux-amd64"
        curl -fsSLO "${base}/checksums.txt"
        grep ' dockrail-linux-amd64$' checksums.txt | sha256sum -c -
        chmod 0755 dockrail-linux-amd64
        mv dockrail-linux-amd64 dockrail
    - name: Set up SSH
      if: inputs.ssh-key != ''
      shell: bash
      run: |
        set -euo pipefail
        mkdir -p ~/.ssh && chmod 700 ~/.ssh
        printf '%s\n' "${{ inputs.ssh-key }}" > ~/.ssh/dockrail_key
        chmod 600 ~/.ssh/dockrail_key
        printf '%s\n' "${{ inputs.known-hosts }}" >> ~/.ssh/known_hosts
        chmod 644 ~/.ssh/known_hosts
        eval "$(ssh-agent -s)" > /dev/null
        echo "SSH_AUTH_SOCK=$SSH_AUTH_SOCK" >> "$GITHUB_ENV"
        echo "SSH_AGENT_PID=$SSH_AGENT_PID" >> "$GITHUB_ENV"
        ssh-add ~/.ssh/dockrail_key
    - name: Deploy
      shell: bash
      run: |
        set -euo pipefail
        args=(-c "${{ inputs.config }}")
        if [ "${{ inputs.dry-run }}" = "true" ]; then
          args+=(--dry-run)
        fi
        if [ -n "${{ inputs.extra-args }}" ]; then
          # shellcheck disable=SC2206 -- intentional word splitting of extra args
          args+=(${{ inputs.extra-args }})
        fi
        "$RUNNER_TEMP/dockrail" deploy "${args[@]}" | tee -a "$GITHUB_STEP_SUMMARY"
```

- [ ] **Step 3: Write the smoke-test workflow**

Builds the binary from source (so PRs test current code, not a stale release) and runs the action in dry-run against the local-exec fixture. GitHub runners have docker + the compose plugin, so preflight (`docker version`, `docker compose version`, `test -f <compose>`) passes.

Create `.github/workflows/action-smoke.yml`:

```yaml
name: action-smoke

on:
  pull_request:
    paths:
      - "action/**"
      - ".github/workflows/action-smoke.yml"
      - "**.go"
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  smoke:
    runs-on: ubuntu-latest
    env:
      GOTOOLCHAIN: local
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v6
        with:
          go-version: "1.26.4"
          check-latest: false
      - name: Build dockrail
        run: go build -o /tmp/dockrail .
      - name: Run action (dry-run, local exec)
        uses: ./action
        with:
          binary-path: /tmp/dockrail
          config: action/testdata/deploy.yml
          dry-run: "true"
```

- [ ] **Step 4: Verify locally**

Run:

```bash
go build -o /tmp/dockrail . && /tmp/dockrail deploy -c action/testdata/deploy.yml --dry-run
```

Expected output (order of plan lines may vary):

```
dry-run: no changes will be made
plan pull web tag v1
plan recreate web (stop old, up -d --no-deps)
plan readiness http :8080/health timeout 30s
```

If `actionlint` is installed, also run: `actionlint action/action.yml .github/workflows/action-smoke.yml` — expected: no findings. (If not installed, skip; CI is the backstop.)

- [ ] **Step 5: Commit**

```bash
git add action/ .github/workflows/action-smoke.yml
git commit -m "feat(action): composite dockrail-deploy GitHub Action with smoke test"
```

---

### Task 3: "GitOps-style workflow" docs page

**Files:**
- Create: `docs/gitops.md`
- Modify: `README.md` (add one link line to the docs page in whatever section lists docs; if none exists, add under a `## Docs` heading)

**Interfaces:**
- Consumes: action inputs from Task 2 (`version`, `config`, `ssh-key`, `known-hosts`, `dry-run`, `extra-args`); `vars:` semantics from Task 1.
- Produces: nothing consumed by other tasks.

- [ ] **Step 1: Write the docs page**

Create `docs/gitops.md` with exactly these sections (prose can be tightened, content requirements are fixed; all examples use `example.com` placeholders):

````markdown
# GitOps-style workflow

dockrail fits a PR-driven deploy flow: git holds the desired state
(`deploy.yml`, including each service's `image_tag`), changes go through
pull requests, and CI runs `dockrail deploy` on merge.

## The flow

1. CI builds and pushes `registry.example.com/app:v42` (dockrail does not
   build images).
2. Open a PR editing `deploy.yml`: `image_tag: v41` -> `image_tag: v42`.
3. On the PR, CI runs `dockrail deploy --dry-run` so review sees the plan.
4. Merge to main. CI runs `dockrail deploy` — health-gated cutover; the old
   version serves until the new one is proven ready.
5. If readiness fails, dockrail rolls back automatically and the CI run goes
   red. Git still points at v42 — the red run is your alarm. Fix forward or
   open a revert PR.

**Rollback = a revert PR.** `dockrail rollback` remains available for
emergencies, but then git lags reality until a revert PR lands.

**Git vs `dockrail audit`:** git history is what was *desired*; `dockrail
audit` is what actually *happened* on the host (including auto-rollbacks).

## GitHub Actions

```yaml
name: deploy
on:
  pull_request:
    paths: [deploy.yml]
  push:
    branches: [main]
    paths: [deploy.yml]

concurrency: deploy-prod

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: goodsmileduck/dockrail/action@main
        with:
          version: v0.3.0            # pin a dockrail release
          config: deploy.yml
          ssh-key: ${{ secrets.DEPLOY_SSH_KEY }}
          known-hosts: ${{ secrets.DEPLOY_KNOWN_HOSTS }}
          dry-run: ${{ github.event_name == 'pull_request' }}
```

## GitLab CI

The workflow is git convention, not GitHub coupling — the same flow on
GitLab (MRs instead of PRs):

```yaml
.install_dockrail: &install_dockrail
  - curl -fsSLo /usr/local/bin/dockrail
    "https://github.com/goodsmileduck/dockrail/releases/download/v0.3.0/dockrail-linux-amd64"
  - chmod +x /usr/local/bin/dockrail
  - mkdir -p ~/.ssh && chmod 700 ~/.ssh
  - printf '%s\n' "$DEPLOY_SSH_KEY" > ~/.ssh/id_ed25519 && chmod 600 ~/.ssh/id_ed25519
  - printf '%s\n' "$DEPLOY_KNOWN_HOSTS" >> ~/.ssh/known_hosts

plan:
  stage: test
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
      changes: [deploy.yml]
  script:
    - *install_dockrail
    - dockrail deploy --dry-run

deploy:
  stage: deploy
  resource_group: deploy-prod
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
      changes: [deploy.yml]
  script:
    - *install_dockrail
    - dockrail deploy
```

Gitea/Forgejo runners are GitHub-Actions compatible; the action above
generally works as-is.

## Repo layouts

- **Same repo as the app** (default): `deploy.yml` next to your compose
  file; path-filter CI to it.
- **Separate deploy repo**: a small repo holding `deploy.yml` (one dir per
  host/env if you like). Same CI job, just in that repo. No dockrail
  configuration differs between the two.

## Variables

`deploy.yml` supports a `vars:` block to avoid repetition while keeping the
file the complete, PR-reviewable truth:

```yaml
vars:
  registry: registry.example.com/team
  tag: "v42"
services:
  api:
    image_tag: "${vars.tag}"
```

Rules: only `${vars.name}` references, values only (not keys), no
environment lookups, no defaults, no nesting. `$${` escapes a literal
`${`. Referencing an undefined variable fails loudly. Secrets never go in
`vars:` — use `secrets.from_env` with a host `env_file`.

## What this is not (yet)

CI still pushes over SSH in this flow. A pull-based reconciler
(`dockrail reconcile` on the host, read-only git access, no inbound
credentials) is planned — see the design spec's deferred section.
````

- [ ] **Step 2: Link from README**

Add to `README.md` (in the existing docs/links section, or under a new `## Docs` heading):

```markdown
- [GitOps-style workflow](docs/gitops.md) — PR-driven deploys with GitHub Actions or GitLab CI
```

- [ ] **Step 3: Verify**

Run: `go vet ./... && go test ./...`
Expected: PASS (docs-only change; this guards against accidental code edits).
Manually check: every host/registry name in `docs/gitops.md` and `action/testdata/` is an `example.com` placeholder.

- [ ] **Step 4: Commit**

```bash
git add docs/gitops.md README.md
git commit -m "docs: GitOps-style workflow guide (GitHub Actions + GitLab CI)"
```
