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

func TestPlan_BinpackConsolidates(t *testing.T) {
	// Two 10GiB replicas, binpack: both should land on ONE 24GiB GPU (least-free
	// that fits), leaving the other GPU empty. Anti-affinity forbids that, so
	// binpack of a SINGLE backend still spreads — use two backends to see it.
	cfg := &fleet.Config{
		Scheduler: fleet.Scheduler{Policy: "binpack"},
		Backends: map[string]fleet.Backend{
			"a1": {ImageTag: "t", Replicas: 1, Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
			"a2": {ImageTag: "t", Replicas: 1, Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
		},
	}
	state := observe.FleetState{Hosts: []observe.HostState{
		{Name: "h", GPUs: []observe.GPUState{gpu(0, 24576), gpu(1, 24576)}},
	}}
	got, err := Plan(cfg, state)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// binpack: a1 takes gpu0; a2 (different backend, anti-affinity N/A) should
	// also pack onto gpu0 (least-free-that-fits) rather than spread to gpu1.
	if got["a1"][0].GPU != 0 || got["a2"][0].GPU != 0 {
		t.Fatalf("binpack did not consolidate: a1=%+v a2=%+v", got["a1"], got["a2"])
	}
}

func TestPlan_PerBackendPolicyOverride(t *testing.T) {
	cfg := &fleet.Config{
		Scheduler: fleet.Scheduler{Policy: "binpack"},
		Backends: map[string]fleet.Backend{
			"s1": {ImageTag: "t", Replicas: 1, Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
			"s2": {ImageTag: "t", Replicas: 1, Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}, Policy: "spread"}},
		},
	}
	state := observe.FleetState{Hosts: []observe.HostState{
		{Name: "h", GPUs: []observe.GPUState{gpu(0, 24576), gpu(1, 24576)}},
	}}
	got, err := Plan(cfg, state)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// s1 binpacks onto gpu0. s2 overrides to spread → most-free is gpu1 (gpu0
	// now has 14576 free, gpu1 has 24576).
	if got["s1"][0].GPU != 0 || got["s2"][0].GPU != 1 {
		t.Fatalf("override wrong: s1=%+v s2=%+v", got["s1"], got["s2"])
	}
}

func TestPlan_Deterministic(t *testing.T) {
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"x": {ImageTag: "t", Replicas: 2, Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
		},
	}
	state := observe.FleetState{Hosts: []observe.HostState{
		{Name: "h", GPUs: []observe.GPUState{gpu(0, 24576), gpu(1, 24576)}},
	}}
	first, _ := Plan(cfg, state)
	for i := 0; i < 20; i++ {
		got, _ := Plan(cfg, state)
		if got["x"][0] != first["x"][0] || got["x"][1] != first["x"][1] {
			t.Fatalf("non-deterministic: %+v vs %+v", got["x"], first["x"])
		}
	}
}

func TestPlan_AntiAffinityExhausted(t *testing.T) {
	// One GPU, one backend wanting 2 replicas: replica 0 takes it, replica 1
	// fits by VRAM but is blocked by anti-affinity -> AntiAffinity ScheduleError.
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"b": {ImageTag: "t", Replicas: 2, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true},
			}},
		},
	}
	state := observe.FleetState{Hosts: []observe.HostState{
		{Name: "h", GPUs: []observe.GPUState{gpu(0, 24576)}},
	}}
	_, err := Plan(cfg, state)
	var se *ScheduleError
	if !errors.As(err, &se) || !se.AntiAffinity {
		t.Fatalf("want AntiAffinity ScheduleError, got %v", err)
	}
}

func TestPlan_FirstFit(t *testing.T) {
	// first-fit picks the first (host,index) that fits even when a later GPU has
	// more free VRAM (which is what spread would pick), proving the policy path.
	cfg := &fleet.Config{
		Scheduler: fleet.Scheduler{Policy: "first-fit"},
		Backends: map[string]fleet.Backend{
			"b": {ImageTag: "t", Replicas: 1, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true},
			}},
		},
	}
	state := observe.FleetState{Hosts: []observe.HostState{
		{Name: "h", GPUs: []observe.GPUState{gpu(0, 20000), gpu(1, 24576)}},
	}}
	got, err := Plan(cfg, state)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got["b"][0].GPU != 0 {
		t.Fatalf("first-fit should pick gpu0, got %+v", got["b"])
	}
}
