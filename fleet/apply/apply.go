package apply

import (
	"context"
	"fmt"

	plan "github.com/goodsmileduck/dockrail/fleet/plan"
)

type actionExecutor interface {
	place(context.Context, plan.Action) error
	update(context.Context, plan.Action) error
	remove(context.Context, plan.Action) error
	deployService(context.Context, plan.Action) error
	updateService(context.Context, plan.Action) error
	rewire(context.Context, plan.Action) error
}

var _ actionExecutor = (*actionExec)(nil)

type Options struct {
	OnFailure string // "hold" (default) | "rollback"
	Scope     string // "" = whole fleet; else a backend/service name
}

type Result struct {
	Applied  []plan.Action
	Failed   *plan.Action
	Pending  []plan.Action
	Warnings []string
}

func dispatch(ctx context.Context, x actionExecutor, a plan.Action) error {
	switch a.Kind {
	case plan.PlaceReplica:
		return x.place(ctx, a)
	case plan.UpdateReplica:
		return x.update(ctx, a)
	case plan.RemoveReplica:
		return x.remove(ctx, a)
	case plan.DeployService:
		return x.deployService(ctx, a)
	case plan.UpdateService:
		return x.updateService(ctx, a)
	case plan.Rewire:
		return x.rewire(ctx, a)
	}
	return fmt.Errorf("unknown action kind %q", a.Kind)
}

func inScope(a plan.Action, scope string) bool {
	if scope == "" {
		return true
	}
	return a.Backend == scope || a.Service == scope
}

// run executes the plan's phases in order. On failure with OnFailure=="rollback"
// it reverses the applied actions (best effort); otherwise (hold) it stops,
// recording the failed action and the not-yet-run actions as Pending.
func run(ctx context.Context, p plan.Plan, x actionExecutor, opts Options) (Result, error) {
	res := Result{Warnings: p.Warnings}
	// Flatten phase-ordered, scope-filtered actions.
	var actions []plan.Action
	for _, ph := range p.Phases {
		for _, a := range ph.Actions {
			if inScope(a, opts.Scope) {
				actions = append(actions, a)
			}
		}
	}
	for i, a := range actions {
		if err := dispatch(ctx, x, a); err != nil {
			fa := a
			res.Failed = &fa
			res.Pending = append(res.Pending, actions[i+1:]...)
			if opts.OnFailure == "rollback" {
				rollback(ctx, x, res.Applied)
			}
			return res, fmt.Errorf("apply: action %s failed: %w", a.Kind, err)
		}
		res.Applied = append(res.Applied, a)
	}
	return res, nil
}

// rollback reverses applied actions in reverse order (best effort): a placed
// replica is removed; a removed one cannot be reliably restored, so it is
// logged as unrecoverable via the executor's own logging on failure.
func rollback(ctx context.Context, x actionExecutor, applied []plan.Action) {
	for i := len(applied) - 1; i >= 0; i-- {
		a := applied[i]
		switch a.Kind {
		case plan.PlaceReplica, plan.UpdateReplica:
			_ = x.remove(ctx, a)
		}
	}
}
