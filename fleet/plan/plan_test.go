package plan

import (
	"testing"

	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/observe"
)

func backendCfg(replicas int, tag string) *fleet.Config {
	return &fleet.Config{Backends: map[string]fleet.Backend{
		"llama": {ImageTag: tag, Replicas: replicas, Placement: fleet.Placement{
			VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true},
		}},
	}}
}

// obs builds a host with GPUs and managed backend-replica containers.
func hostWith(name string, free map[int]int, reps []observe.Container) observe.HostState {
	var gpus []observe.GPUState
	for idx, f := range free {
		gpus = append(gpus, observe.GPUState{Index: idx, TotalMiB: f, FreeMiB: f})
	}
	return observe.HostState{Name: name, GPUs: gpus, Containers: reps}
}

func rep(backend string, replica, gpu int, image string) observe.Container {
	return observe.Container{Name: backend + "-" + itoa(replica), Image: image, Labels: map[string]string{
		observe.LabelManaged: "true", observe.LabelBackend: backend,
		observe.LabelReplica: itoa(replica), observe.LabelGPU: itoa(gpu),
	}}
}
func itoa(i int) string { return string(rune('0' + i)) } // single-digit test helper

func actionsOf(p Plan) []Action {
	var all []Action
	for _, ph := range p.Phases {
		all = append(all, ph.Actions...)
	}
	return all
}

func TestCompute_Noop(t *testing.T) {
	cfg := backendCfg(1, "v2")
	// one replica running on gpu0 with the right tag -> satisfied, empty plan.
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288, 1: 24576}, []observe.Container{rep("llama", 0, 0, "reg/llama:v2")}),
	}}
	p, err := Compute(cfg, st)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(actionsOf(p)) != 0 {
		t.Fatalf("expected empty plan, got %+v", actionsOf(p))
	}
}

func TestCompute_ScaleUpPlaces(t *testing.T) {
	cfg := backendCfg(2, "v2")
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288, 1: 24576}, []observe.Container{rep("llama", 0, 0, "reg/llama:v2")}),
	}}
	p, _ := Compute(cfg, st)
	var place *Action
	for _, ph := range p.Phases {
		for i := range ph.Actions {
			if ph.Actions[i].Kind == PlaceReplica {
				place = &ph.Actions[i]
			}
		}
	}
	if place == nil || place.Backend != "llama" || place.Replica != 1 || place.GPU != 1 {
		t.Fatalf("want place llama/1 on gpu1, got %+v", place)
	}
}

func TestCompute_TagChangeUpdates(t *testing.T) {
	cfg := backendCfg(1, "v3")
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288}, []observe.Container{rep("llama", 0, 0, "reg/llama:v2")}),
	}}
	p, _ := Compute(cfg, st)
	as := actionsOf(p)
	if len(as) != 1 || as[0].Kind != UpdateReplica || as[0].Tag != "v3" || as[0].OldTag != "v2" {
		t.Fatalf("want update v2->v3, got %+v", as)
	}
}

func TestCompute_ScaleDownRemoves(t *testing.T) {
	cfg := backendCfg(1, "v2")
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288, 1: 12288}, []observe.Container{
			rep("llama", 0, 0, "reg/llama:v2"), rep("llama", 1, 1, "reg/llama:v2"),
		}),
	}}
	p, _ := Compute(cfg, st)
	as := actionsOf(p)
	if len(as) != 1 || as[0].Kind != RemoveReplica || as[0].Replica != 1 {
		t.Fatalf("want remove llama/1, got %+v", as)
	}
	// remove must be in the drain phase (last).
	last := p.Phases[len(p.Phases)-1]
	if last.Name != "drain" || len(last.Actions) != 1 {
		t.Fatalf("remove not in drain phase: %+v", p.Phases)
	}
}

func TestCompute_ServiceDeployAndRewire(t *testing.T) {
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"llama": {ImageTag: "v2", Replicas: 1, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
		},
		Services: map[string]fleet.Service{
			"chat": {Host: "h", ImageTag: "s1",
				Uses: []fleet.Use{{Backend: "llama", Wiring: fleet.Wiring{Strategy: "nginx-upstream"}}}},
		},
	}
	// llama/0 already running & satisfied; chat service absent -> deploy + rewire.
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288}, []observe.Container{rep("llama", 0, 0, "reg/llama:v2")}),
	}}
	p, err := Compute(cfg, st)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	var deploy, rewire *Action
	for pi := range p.Phases {
		for ai := range p.Phases[pi].Actions {
			a := &p.Phases[pi].Actions[ai]
			if a.Kind == DeployService {
				deploy = a
			}
			if a.Kind == Rewire {
				rewire = a
			}
		}
	}
	if deploy == nil || deploy.Service != "chat" || deploy.Host != "h" || deploy.Tag != "s1" {
		t.Fatalf("service deploy wrong: %+v", deploy)
	}
	if rewire == nil || rewire.Service != "chat" || rewire.Backend != "llama" || len(rewire.Endpoints) != 1 || rewire.Endpoints[0] != "h" {
		t.Fatalf("rewire wrong: %+v", rewire)
	}
}

func TestCompute_PhaseOrdering(t *testing.T) {
	// A plan with a place (converge), a rewire, and a remove (drain) must order
	// them converge < rewire < drain.
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"llama": {ImageTag: "v2", Replicas: 2, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
			"old": {ImageTag: "v1", Replicas: 0, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
		},
		Services: map[string]fleet.Service{
			"chat": {Host: "h", ImageTag: "s1",
				Uses: []fleet.Use{{Backend: "llama", Wiring: fleet.Wiring{Strategy: "nginx-upstream"}}}},
		},
	}
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288, 1: 24576}, []observe.Container{
			rep("llama", 0, 0, "reg/llama:v2"),
			rep("old", 0, 1, "reg/old:v1"),
		}),
	}}
	p, _ := Compute(cfg, st)
	if len(p.Phases) != 3 || p.Phases[0].Name != "converge" || p.Phases[1].Name != "rewire" || p.Phases[2].Name != "drain" {
		t.Fatalf("phase names/order wrong: %+v", p.Phases)
	}
	if len(p.Phases[1].Actions) == 0 || p.Phases[1].Actions[0].Kind != Rewire {
		t.Fatalf("rewire phase empty: %+v", p.Phases[1])
	}
	if len(p.Phases[2].Actions) == 0 || p.Phases[2].Actions[0].Kind != RemoveReplica {
		t.Fatalf("drain phase should remove old/0: %+v", p.Phases[2])
	}
}

func TestCompute_ErrHostBackendUnplanned(t *testing.T) {
	// The only host is unreachable; a desired replica must NOT be re-placed —
	// instead a warning, and no place action.
	cfg := backendCfg(1, "v2")
	st := observe.FleetState{Hosts: []observe.HostState{
		{Name: "h", Err: "unreachable", GPUs: []observe.GPUState{{Index: 0, TotalMiB: 24576, FreeMiB: 24576}}},
	}}
	p, err := Compute(cfg, st)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(actionsOf(p)) != 0 {
		t.Fatalf("expected no actions for Err-host backend, got %+v", actionsOf(p))
	}
	if len(p.Warnings) == 0 {
		t.Fatalf("expected a warning for the unreachable host")
	}
}

func TestCompute_SteadyStateServiceNoRewire(t *testing.T) {
	// Backend satisfied AND service satisfied -> empty plan (no rewire churn).
	cfg := &fleet.Config{
		Backends: map[string]fleet.Backend{
			"llama": {ImageTag: "v2", Replicas: 1, Placement: fleet.Placement{
				VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
		},
		Services: map[string]fleet.Service{
			"chat": {Host: "h", ImageTag: "s1",
				Uses: []fleet.Use{{Backend: "llama", Wiring: fleet.Wiring{Strategy: "nginx-upstream"}}}},
		},
	}
	svc := observe.Container{Name: "chat", Image: "reg/chat:s1", Labels: map[string]string{
		observe.LabelManaged: "true", observe.LabelService: "chat"}}
	st := observe.FleetState{Hosts: []observe.HostState{
		hostWith("h", map[int]int{0: 12288}, []observe.Container{rep("llama", 0, 0, "reg/llama:v2"), svc}),
	}}
	p, _ := Compute(cfg, st)
	if len(actionsOf(p)) != 0 {
		t.Fatalf("steady state should emit no actions (incl no rewire), got %+v", actionsOf(p))
	}
}
