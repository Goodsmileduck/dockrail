// Package apply executes a Planner plan across the fleet via generated
// per-replica compose overrides, health-gated per the fleet serving invariant.
package apply

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/override"
	plan "github.com/goodsmileduck/dockrail/fleet/plan"
	"github.com/goodsmileduck/dockrail/strategy/readiness"
)

// safeTag restricts image tags interpolated into shell/compose commands.
var safeTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Endpoint/Wiring stubs — LogWiring default added in wiring.go (Task 3)
type Endpoint struct {
	Host string
	Port int
}

type Wiring interface {
	Apply(ctx context.Context, service, backend string, endpoints []Endpoint) error
}

type actionExec struct {
	cfg    *fleet.Config
	conn   connection.Connection
	out    io.Writer
	wiring Wiring
}

func (x *actionExec) logf(format string, a ...any) {
	if x.out != nil {
		fmt.Fprintf(x.out, format+"\n", a...)
	}
}

// overridePath is the host path for a generated override, a sibling of the base
// compose so its relative `extends: {file: <base>}` resolves.
func (x *actionExec) overridePath(name string) string {
	dir := filepath.Dir(x.cfg.Compose)
	return filepath.Join(dir, ".dockrail-"+name+".override.yml")
}

// baseName is the compose path as referenced from the override's directory
// (same dir), i.e. its basename.
func (x *actionExec) baseName() string { return filepath.Base(x.cfg.Compose) }

func (x *actionExec) writeFile(ctx context.Context, path, content string) error {
	cmd := fmt.Sprintf("cat > %s <<'DOCKRAILEOF'\n%s\nDOCKRAILEOF\n", path, content)
	_, err := x.conn.Run(ctx, cmd)
	return err
}

func (x *actionExec) place(ctx context.Context, a plan.Action) error {
	b, ok := x.cfg.Backends[a.Backend]
	if !ok {
		return fmt.Errorf("place: unknown backend %q", a.Backend)
	}
	if !safeTag.MatchString(a.Tag) {
		return fmt.Errorf("place %s/%d: unsafe image tag %q", a.Backend, a.Replica, a.Tag)
	}
	name := fmt.Sprintf("%s-%d", a.Backend, a.Replica)
	ov := x.overridePath(name)
	body, _ := override.Replica(x.baseName(), b.Service, a.Backend, a.Replica, a.GPU, a.Tag)
	if err := x.writeFile(ctx, ov, body); err != nil {
		return fmt.Errorf("place %s: write override: %w", name, err)
	}
	x.logf("place %s on %s:%d (%s)", name, a.Host, a.GPU, a.Tag)
	if err := x.composeUp(ctx, a.Tag, ov, name); err != nil {
		return fmt.Errorf("place %s: compose up: %w", name, err)
	}
	return x.probe(ctx, b.Readiness, b.Model, name)
}

// composeUp launches (or recreates) a single generated service. Both -f files
// are required: the base supplies top-level networks/volumes that `extends`
// does not copy; `--no-deps` starts only the target, not the template or deps.
func (x *actionExec) composeUp(ctx context.Context, tag, override, target string) error {
	up := fmt.Sprintf("TAG=%s docker compose -f %s -f %s up -d --no-deps %s", tag, x.cfg.Compose, override, target)
	_, err := x.conn.Run(ctx, up)
	return err
}

// update recreates the replica with the new tag. compose up -d recreates the
// container when its image changed; the backend's other replicas keep serving.
func (x *actionExec) update(ctx context.Context, a plan.Action) error { return x.place(ctx, a) }

func (x *actionExec) deployService(ctx context.Context, a plan.Action) error {
	s, ok := x.cfg.Services[a.Service]
	if !ok {
		return fmt.Errorf("deploy: unknown service %q", a.Service)
	}
	if a.Tag != "" && !safeTag.MatchString(a.Tag) {
		return fmt.Errorf("deploy %s: unsafe tag %q", a.Service, a.Tag)
	}
	ov := x.overridePath(a.Service)
	body, _ := override.Service(x.baseName(), s.Service, a.Service, a.Tag)
	if err := x.writeFile(ctx, ov, body); err != nil {
		return fmt.Errorf("deploy %s: write override: %w", a.Service, err)
	}
	x.logf("deploy service %s on %s (%s)", a.Service, a.Host, a.Tag)
	if err := x.composeUp(ctx, a.Tag, ov, a.Service); err != nil {
		return fmt.Errorf("deploy %s: compose up: %w", a.Service, err)
	}
	return x.probe(ctx, s.Readiness, "", a.Service)
}

func (x *actionExec) updateService(ctx context.Context, a plan.Action) error {
	return x.deployService(ctx, a)
}

func (x *actionExec) rewire(ctx context.Context, a plan.Action) error {
	eps := make([]Endpoint, 0, len(a.Endpoints))
	for _, h := range a.Endpoints {
		eps = append(eps, Endpoint{Host: h}) // port derivation = sub-spec 5
	}
	if x.wiring == nil {
		x.wiring = LogWiring{Out: x.out}
	}
	return x.wiring.Apply(ctx, a.Service, a.Backend, eps)
}

func (x *actionExec) remove(ctx context.Context, a plan.Action) error {
	name := fmt.Sprintf("%s-%d", a.Backend, a.Replica)
	x.logf("remove %s on %s:%d", name, a.Host, a.GPU)
	// Force-remove the single container by its name (docker rm -f stops it first).
	if _, err := x.conn.Run(ctx, fmt.Sprintf("docker rm -f %s", name)); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// probe runs the readiness strategy for a placed replica or deployed service. A
// probe failure keeps the container for forensics (it is not torn down).
func (x *actionExec) probe(ctx context.Context, r fleet.Readiness, model, who string) error {
	prober, err := readiness.New(config.Readiness{
		Type: r.Type, Path: r.Path, Port: r.Port, Timeout: r.Timeout,
	}, model)
	if err != nil {
		return fmt.Errorf("%s: readiness config: %w", who, err)
	}
	if err := prober.Probe(ctx, x.conn); err != nil {
		return fmt.Errorf("%s: readiness failed (container kept for inspection): %w", who, err)
	}
	return nil
}
