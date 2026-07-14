package apply

import (
	"context"
	"errors"
	"testing"

	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/observe"
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

func TestApply_EmptyPlanNoop(t *testing.T) {
	cfg := &fleet.Config{
		Hosts: map[string]fleet.Host{"h": {SSH: "user@h", GPUs: []int{0}}},
		Backends: map[string]fleet.Backend{
			"llama": {
				Service:  "llama",
				ImageTag: "v2",
				Replicas: 1,
				Placement: fleet.Placement{
					VRAMMin: "10GiB",
					GPU:     fleet.GPUSpec{Auto: true},
					Pool:    []string{"h"},
				},
			},
		},
	}
	observed := observe.FleetState{Hosts: []observe.HostState{{
		Name: "h",
		Containers: []observe.Container{{
			Name:  "llama-0",
			Image: "registry/llama:v2",
			Labels: map[string]string{
				observe.LabelManaged: "true",
				observe.LabelBackend: "llama",
				observe.LabelReplica: "0",
				observe.LabelGPU:     "0",
			},
		}},
		GPUs: []observe.GPUState{{Index: 0, TotalMiB: 24576, UsedMiB: 12288, FreeMiB: 12288}},
	}}}
	f := &fakeExec{}

	res, err := Apply(context.Background(), cfg, observed, f, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("expected no applied actions, got %+v", res.Applied)
	}
	if len(f.done) != 0 {
		t.Fatalf("expected executor not to run, got %+v", f.done)
	}
}

func TestApply_SurfacesWarnings(t *testing.T) {
	cfg := &fleet.Config{
		Hosts: map[string]fleet.Host{"h": {SSH: "user@h", GPUs: []int{0}}},
		Backends: map[string]fleet.Backend{
			"llama": {
				Service:  "llama",
				ImageTag: "v2",
				Replicas: 1,
				Placement: fleet.Placement{
					VRAMMin: "10GiB",
					GPU:     fleet.GPUSpec{Auto: true},
					Pool:    []string{"h"},
				},
			},
		},
	}
	observed := observe.FleetState{Hosts: []observe.HostState{{
		Name: "h",
		Err:  "unreachable",
		GPUs: []observe.GPUState{{Index: 0, TotalMiB: 24576, FreeMiB: 24576}},
	}}}
	f := &fakeExec{}

	res, err := Apply(context.Background(), cfg, observed, f, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected warning for unreachable host")
	}
	if len(f.done) != 0 {
		t.Fatalf("expected no action on unreachable host, got %+v", f.done)
	}
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
