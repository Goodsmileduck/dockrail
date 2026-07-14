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

// execFor returns the (cached) executor bound to the action's host, opening the
// connection on first use. Every action carries its Host (the Planner stamps it,
// including Rewire's service host), so the router does not special-case kinds.
func (r *hostRouter) execFor(a plan.Action) (*actionExec, error) {
	if a.Host == "" {
		return nil, fmt.Errorf("router: action %s has no host", a.Kind)
	}
	if x, ok := r.execs[a.Host]; ok {
		return x, nil
	}
	h, ok := r.cfg.Hosts[a.Host]
	if !ok {
		return nil, fmt.Errorf("router: unknown host %q", a.Host)
	}
	conn, err := r.factory(a.Host, h)
	if err != nil {
		return nil, fmt.Errorf("router: connect %q: %w", a.Host, err)
	}
	// wiring is left nil; actionExec.rewire lazily defaults it to LogWiring.
	x := &actionExec{cfg: r.cfg, conn: conn, out: r.out}
	r.execs[a.Host] = x
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
