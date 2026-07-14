package apply

import (
	"context"
	"errors"
	"testing"

	plan "github.com/goodsmileduck/dockrail/fleet/plan"
)

// fakeExec records dispatched actions and can fail a chosen kind.
type fakeExec struct {
	done   []plan.Action
	failOn plan.ActionKind
}

func (f *fakeExec) do(a plan.Action) error {
	if a.Kind == f.failOn {
		return errors.New("boom")
	}
	f.done = append(f.done, a)
	return nil
}
func (f *fakeExec) place(_ context.Context, a plan.Action) error         { return f.do(a) }
func (f *fakeExec) update(_ context.Context, a plan.Action) error        { return f.do(a) }
func (f *fakeExec) remove(_ context.Context, a plan.Action) error        { return f.do(a) }
func (f *fakeExec) deployService(_ context.Context, a plan.Action) error { return f.do(a) }
func (f *fakeExec) updateService(_ context.Context, a plan.Action) error { return f.do(a) }
func (f *fakeExec) rewire(_ context.Context, a plan.Action) error        { return f.do(a) }

func demoPlan() plan.Plan {
	return plan.Plan{Phases: []plan.Phase{
		{Name: "converge", Actions: []plan.Action{{Kind: plan.PlaceReplica, Backend: "llama", Replica: 1}}},
		{Name: "rewire", Actions: []plan.Action{{Kind: plan.Rewire, Service: "chat", Backend: "llama"}}},
		{Name: "drain", Actions: []plan.Action{{Kind: plan.RemoveReplica, Backend: "old", Replica: 0}}},
	}}
}

func TestRun_PhaseOrder(t *testing.T) {
	f := &fakeExec{}
	res, err := run(context.Background(), demoPlan(), f, Options{OnFailure: "hold"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Applied) != 3 || f.done[0].Kind != plan.PlaceReplica || f.done[1].Kind != plan.Rewire || f.done[2].Kind != plan.RemoveReplica {
		t.Fatalf("wrong order/applied: %+v", f.done)
	}
}

func TestRun_HoldStopsAndReportsPending(t *testing.T) {
	f := &fakeExec{failOn: plan.Rewire}
	res, _ := run(context.Background(), demoPlan(), f, Options{OnFailure: "hold"})
	if res.Failed == nil || res.Failed.Kind != plan.Rewire {
		t.Fatalf("want Failed=rewire, got %+v", res.Failed)
	}
	if len(res.Applied) != 1 || len(res.Pending) != 1 || res.Pending[0].Kind != plan.RemoveReplica {
		t.Fatalf("want 1 applied (place) + 1 pending (remove): applied=%+v pending=%+v", res.Applied, res.Pending)
	}
}

func TestRun_ScopeFilters(t *testing.T) {
	f := &fakeExec{}
	res, _ := run(context.Background(), demoPlan(), f, Options{OnFailure: "hold", Scope: "llama"})
	// only actions touching backend/service "llama" execute (place + rewire); the
	// "old" remove is filtered out.
	for _, a := range res.Applied {
		if a.Backend == "old" {
			t.Fatalf("scope should exclude old: %+v", res.Applied)
		}
	}
}
