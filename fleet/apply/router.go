package apply

import (
	"context"
	"fmt"
	"io"

	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/observe"
	plan "github.com/goodsmileduck/dockrail/fleet/plan"
)

// ConnFactory builds a connection for a host — the same shape as
// observe.ConnFactory, reused so the command layer injects one factory for both
// observing and applying.
type ConnFactory = observe.ConnFactory

// hostRouter dispatches each action to an actionExec bound to the action's host,
// building one exec per distinct host lazily via the factory. It satisfies
// actionExecutor, so Apply drives a whole fleet through a single router while
// actionExec and actionExecutor stay unexported.
type hostRouter struct {
	cfg     *fleet.Config
	factory ConnFactory
	out     io.Writer
	execs   map[string]*actionExec
}

var _ actionExecutor = (*hostRouter)(nil)

func newHostRouter(cfg *fleet.Config, factory ConnFactory, out io.Writer) *hostRouter {
	return &hostRouter{cfg: cfg, factory: factory, out: out, execs: map[string]*actionExec{}}
}

// hostFor resolves the host an action executes on. Replica and service actions
// carry Host; a Rewire does not, so it routes to the service's configured host
// (where the proxy/env-list lives).
func (r *hostRouter) hostFor(a plan.Action) string {
	if a.Host != "" {
		return a.Host
	}
	if s, ok := r.cfg.Services[a.Service]; ok {
		return s.Host
	}
	return ""
}

// execFor returns the (cached) executor bound to the action's host, opening the
// connection on first use.
func (r *hostRouter) execFor(a plan.Action) (*actionExec, error) {
	host := r.hostFor(a)
	if host == "" {
		return nil, fmt.Errorf("router: action %s has no resolvable host", a.Kind)
	}
	if x, ok := r.execs[host]; ok {
		return x, nil
	}
	h, ok := r.cfg.Hosts[host]
	if !ok {
		return nil, fmt.Errorf("router: unknown host %q", host)
	}
	conn, err := r.factory(host, h)
	if err != nil {
		return nil, fmt.Errorf("router: connect %q: %w", host, err)
	}
	x := &actionExec{cfg: r.cfg, conn: conn, out: r.out, wiring: LogWiring{Out: r.out}}
	r.execs[host] = x
	return x, nil
}

func (r *hostRouter) place(ctx context.Context, a plan.Action) error {
	x, err := r.execFor(a)
	if err != nil {
		return err
	}
	return x.place(ctx, a)
}

func (r *hostRouter) update(ctx context.Context, a plan.Action) error {
	x, err := r.execFor(a)
	if err != nil {
		return err
	}
	return x.update(ctx, a)
}

func (r *hostRouter) remove(ctx context.Context, a plan.Action) error {
	x, err := r.execFor(a)
	if err != nil {
		return err
	}
	return x.remove(ctx, a)
}

func (r *hostRouter) deployService(ctx context.Context, a plan.Action) error {
	x, err := r.execFor(a)
	if err != nil {
		return err
	}
	return x.deployService(ctx, a)
}

func (r *hostRouter) updateService(ctx context.Context, a plan.Action) error {
	x, err := r.execFor(a)
	if err != nil {
		return err
	}
	return x.updateService(ctx, a)
}

func (r *hostRouter) rewire(ctx context.Context, a plan.Action) error {
	x, err := r.execFor(a)
	if err != nil {
		return err
	}
	return x.rewire(ctx, a)
}

// RunFleet builds a per-host router over factory and applies the plan for the
// observed fleet state. It is the exported entrypoint the command uses;
// actionExec / actionExecutor remain package-private.
func RunFleet(ctx context.Context, cfg *fleet.Config, observed observe.FleetState,
	factory ConnFactory, out io.Writer, opts Options) (Result, error) {
	return Apply(ctx, cfg, observed, newHostRouter(cfg, factory, out), opts)
}
