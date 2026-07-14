package apply

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/observe"
	plan "github.com/goodsmileduck/dockrail/fleet/plan"
)

func routerFixture() (*hostRouter, map[string]*connection.Fake) {
	fakes := map[string]*connection.Fake{}
	cfg := &fleet.Config{
		Compose: "docker-compose.yml",
		Hosts:   map[string]fleet.Host{"h1": {}, "h2": {}},
		Backends: map[string]fleet.Backend{
			"llama": {Service: "vllm", ImageTag: "v2", Replicas: 1,
				Placement: fleet.Placement{VRAMMin: "10GiB", GPU: fleet.GPUSpec{Auto: true}, Pool: []string{"h1"}},
				Readiness: fleet.Readiness{Type: "tcp", Port: 8000}},
		},
		Services: map[string]fleet.Service{
			"chat": {Service: "chat-api", Host: "h2",
				Readiness: fleet.Readiness{Type: "tcp", Port: 8080}},
		},
	}
	factory := func(name string, _ fleet.Host) (connection.Connection, error) {
		f := connection.NewFake()
		fakes[name] = f
		return f, nil
	}
	return newHostRouter(cfg, factory, &bytes.Buffer{}), fakes
}

func TestRouter_RoutesReplicaByActionHost(t *testing.T) {
	r, fakes := routerFixture()
	if err := r.place(context.Background(), plan.Action{Kind: plan.PlaceReplica, Backend: "llama", Replica: 0, Host: "h1", GPU: 0, Tag: "v2"}); err != nil {
		t.Fatalf("place: %v", err)
	}
	if _, ok := fakes["h1"]; !ok {
		t.Fatalf("expected a connection built for h1; built: %v", keys(fakes))
	}
	if _, ok := fakes["h2"]; ok {
		t.Fatalf("did not expect a connection for h2 on an h1 action")
	}
}

func TestRouter_RewireRoutesByHost(t *testing.T) {
	r, fakes := routerFixture()
	// The Planner stamps the service host onto Rewire; the router routes by it.
	if err := r.rewire(context.Background(), plan.Action{Kind: plan.Rewire, Service: "chat", Backend: "llama", Host: "h2", Endpoints: []string{"h1"}}); err != nil {
		t.Fatalf("rewire: %v", err)
	}
	if _, ok := fakes["h2"]; !ok {
		t.Fatalf("expected rewire to build a connection for the stamped host h2; built: %v", keys(fakes))
	}
}

func TestRouter_CachesExecPerHost(t *testing.T) {
	r, fakes := routerFixture()
	ctx := context.Background()
	_ = r.place(ctx, plan.Action{Kind: plan.PlaceReplica, Backend: "llama", Replica: 0, Host: "h1", GPU: 0, Tag: "v2"})
	_ = r.remove(ctx, plan.Action{Kind: plan.RemoveReplica, Backend: "llama", Replica: 0, Host: "h1", GPU: 0})
	if len(fakes) != 1 {
		t.Fatalf("expected exactly one connection reused for h1, built: %v", keys(fakes))
	}
	// Both the compose up and the rm went to the same fake.
	f := fakes["h1"]
	var sawUp, sawRm bool
	for _, c := range f.Commands {
		if strings.Contains(c, "up -d") {
			sawUp = true
		}
		if strings.Contains(c, "rm -f llama-0") {
			sawRm = true
		}
	}
	if !sawUp || !sawRm {
		t.Fatalf("expected place+remove on the same h1 connection; commands: %v", f.Commands)
	}
}

func TestRouter_UnknownHostErrors(t *testing.T) {
	r, _ := routerFixture()
	err := r.place(context.Background(), plan.Action{Kind: plan.PlaceReplica, Backend: "llama", Replica: 0, Host: "ghost", GPU: 0, Tag: "v2"})
	if err == nil || !strings.Contains(err.Error(), "unknown host") {
		t.Fatalf("expected unknown-host error, got %v", err)
	}
}

func TestRunFleet_EmptyPlanNoop(t *testing.T) {
	// Desired == observed: one satisfied replica, so RunFleet issues no mutating
	// commands (no compose up, no rm) — the computed plan is empty.
	fakes := map[string]*connection.Fake{}
	cfg := &fleet.Config{
		Compose: "docker-compose.yml",
		Hosts:   map[string]fleet.Host{"h1": {GPUs: []int{0}}},
		Backends: map[string]fleet.Backend{
			"llama": {Service: "vllm", ImageTag: "v2", Replicas: 1,
				Placement: fleet.Placement{VRAMMin: "10GiB", GPU: fleet.GPUSpec{Auto: true}, Pool: []string{"h1"}},
				Readiness: fleet.Readiness{Type: "tcp", Port: 8000}},
		},
	}
	factory := func(name string, _ fleet.Host) (connection.Connection, error) {
		f := connection.NewFake()
		fakes[name] = f
		return f, nil
	}
	observed := observe.FleetState{Hosts: []observe.HostState{{
		Name: "h1",
		Containers: []observe.Container{{
			Name:  "llama-0",
			Image: "registry/vllm:v2",
			Labels: map[string]string{
				observe.LabelManaged: "true",
				observe.LabelBackend: "llama",
				observe.LabelReplica: "0",
				observe.LabelGPU:     "0",
			},
		}},
		GPUs: []observe.GPUState{{Index: 0, TotalMiB: 24576, UsedMiB: 12288, FreeMiB: 12288}},
	}}}
	res, err := RunFleet(context.Background(), cfg, observed, factory, &bytes.Buffer{}, Options{})
	if err != nil {
		t.Fatalf("RunFleet: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("expected no applied actions, got %+v", res.Applied)
	}
	for name, f := range fakes {
		for _, c := range f.Commands {
			if strings.Contains(c, "up -d") || strings.Contains(c, "rm -f") {
				t.Fatalf("expected no mutating command on %s, got %q", name, c)
			}
		}
	}
}

func keys(m map[string]*connection.Fake) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
