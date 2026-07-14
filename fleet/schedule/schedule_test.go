package schedule

import (
	"errors"
	"testing"

	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/observe"
)

// gpu builds an observed GPU with free==total (nothing else running).
func gpu(idx, freeMiB int) observe.GPUState {
	return observe.GPUState{Index: idx, TotalMiB: freeMiB, UsedMiB: 0, FreeMiB: freeMiB}
}

func TestPlan_SpreadAcrossGPUs(t *testing.T) {
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"llama": {ImageTag: "t", Replicas: 2, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"a"}, GPU: fleet.GPUSpec{Auto: true},
			}},
		},
	}
	state := observe.FleetState{Hosts: []observe.HostState{
		{Name: "a", GPUs: []observe.GPUState{gpu(0, 24576), gpu(1, 24576)}},
	}}
	got, err := Plan(cfg, state)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	as := got["llama"]
	if len(as) != 2 {
		t.Fatalf("want 2 assignments, got %+v", as)
	}
	// spread + anti-affinity: the two replicas must be on different GPUs.
	if as[0].GPU == as[1].GPU {
		t.Fatalf("replicas colocated: %+v", as)
	}
}

func TestPlan_Pins(t *testing.T) {
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"embed": {ImageTag: "t", Replicas: 2, Placement: fleet.Placement{
				VRAMMin: "6GiB", GPU: fleet.GPUSpec{Pins: []string{"a:0", "a:1"}},
			}},
		},
	}
	state := observe.FleetState{Hosts: []observe.HostState{
		{Name: "a", GPUs: []observe.GPUState{gpu(0, 24576), gpu(1, 24576)}},
	}}
	got, err := Plan(cfg, state)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	as := got["embed"]
	if len(as) != 2 || as[0].Host != "a" || as[0].GPU != 0 || as[1].GPU != 1 {
		t.Fatalf("pins not honoured: %+v", as)
	}
}

func TestPlan_ErrHostExcluded(t *testing.T) {
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"llama": {ImageTag: "t", Replicas: 1, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"a"}, GPU: fleet.GPUSpec{Auto: true},
			}},
		},
	}
	state := observe.FleetState{Hosts: []observe.HostState{
		{Name: "a", Err: "unreachable", GPUs: []observe.GPUState{gpu(0, 24576)}},
	}}
	if _, err := Plan(cfg, state); err == nil {
		t.Fatal("expected error: only host is unschedulable")
	}
}

func TestPlan_Infeasible(t *testing.T) {
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"big": {ImageTag: "t", Replicas: 1, Placement: fleet.Placement{
				VRAMMin: "40GiB", Pool: []string{"a"}, GPU: fleet.GPUSpec{Auto: true},
			}},
		},
	}
	state := observe.FleetState{Hosts: []observe.HostState{
		{Name: "a", GPUs: []observe.GPUState{gpu(0, 24576)}},
	}}
	_, err := Plan(cfg, state)
	var se *ScheduleError
	if !errors.As(err, &se) {
		t.Fatalf("want *ScheduleError, got %v", err)
	}
	if se.Backend != "big" || se.Replica != 0 {
		t.Fatalf("bad ScheduleError fields: %+v", se)
	}
}
