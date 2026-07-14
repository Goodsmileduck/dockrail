package apply

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
	plan "github.com/goodsmileduck/dockrail/fleet/plan"
)

func execFixture() (*actionExec, *connection.Fake) {
	f := connection.NewFake()
	cfg := &fleet.Config{
		Compose: "docker-compose.yml",
		Backends: map[string]fleet.Backend{
			"llama": {Service: "vllm", ImageTag: "v2", Replicas: 2,
				Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}},
				Readiness: fleet.Readiness{Type: "tcp", Port: 8000}},
		},
	}
	return &actionExec{cfg: cfg, conn: f, out: &bytes.Buffer{}}, f
}

func TestPlace_WritesOverrideAndComposeUp(t *testing.T) {
	x, f := execFixture()
	err := x.place(context.Background(), plan.Action{Kind: plan.PlaceReplica, Backend: "llama", Replica: 0, Host: "h", GPU: 1, Tag: "v2"})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	var sawWrite, sawUp bool
	for _, c := range f.Commands {
		if strings.Contains(c, "DOCKRAILEOF") && strings.Contains(c, "llama-0") {
			sawWrite = true
		}
		if strings.Contains(c, "docker compose") && strings.Contains(c, "up -d") && strings.Contains(c, "llama-0") {
			sawUp = true
		}
	}
	if !sawWrite || !sawUp {
		t.Fatalf("want override write + compose up for llama-0; commands: %v", f.Commands)
	}
}

func TestPlace_RejectsUnsafeTag(t *testing.T) {
	x, _ := execFixture()
	if err := x.place(context.Background(), plan.Action{Kind: plan.PlaceReplica, Backend: "llama", Replica: 0, Host: "h", GPU: 1, Tag: "v2; rm -rf /"}); err == nil {
		t.Fatal("expected unsafe-tag rejection")
	}
}

func TestDeployService_WritesOverrideAndComposeUp(t *testing.T) {
	f := connection.NewFake()
	cfg := &fleet.Config{
		Compose: "docker-compose.yml",
		Services: map[string]fleet.Service{
			"chat": {Service: "chat-api", Host: "h", ImageTag: "v3",
				Readiness: fleet.Readiness{Type: "tcp", Port: 8080}},
		},
	}
	x := &actionExec{cfg: cfg, conn: f, out: &bytes.Buffer{}}
	if err := x.deployService(context.Background(), plan.Action{Kind: plan.DeployService, Service: "chat", Host: "h", Tag: "v3"}); err != nil {
		t.Fatalf("deployService: %v", err)
	}
	var sawWrite, sawUp bool
	for _, c := range f.Commands {
		if strings.Contains(c, "DOCKRAILEOF") && strings.Contains(c, "chat") {
			sawWrite = true
		}
		if strings.Contains(c, "docker compose") && strings.Contains(c, "up -d") && strings.Contains(c, "chat") {
			sawUp = true
		}
	}
	if !sawWrite || !sawUp {
		t.Fatalf("want override write + compose up for chat; commands: %v", f.Commands)
	}
}

func TestDeployService_RejectsUnsafeTag(t *testing.T) {
	f := connection.NewFake()
	cfg := &fleet.Config{
		Compose:  "docker-compose.yml",
		Services: map[string]fleet.Service{"chat": {Service: "chat-api", Host: "h"}},
	}
	x := &actionExec{cfg: cfg, conn: f, out: &bytes.Buffer{}}
	if err := x.deployService(context.Background(), plan.Action{Kind: plan.DeployService, Service: "chat", Host: "h", Tag: "v3; rm -rf /"}); err == nil {
		t.Fatal("expected unsafe-tag rejection")
	}
}

func TestRewire_DefaultsToLogWiring(t *testing.T) {
	var buf bytes.Buffer
	x := &actionExec{cfg: &fleet.Config{}, conn: connection.NewFake(), out: &buf}
	err := x.rewire(context.Background(), plan.Action{Kind: plan.Rewire, Service: "chat", Backend: "llama", Endpoints: []string{"h1", "h2"}})
	if err != nil {
		t.Fatalf("rewire: %v", err)
	}
	if !strings.Contains(buf.String(), "chat") || !strings.Contains(buf.String(), "llama") {
		t.Fatalf("expected a wiring log line naming service+backend, got %q", buf.String())
	}
}

func TestRemove_DockerRm(t *testing.T) {
	x, f := execFixture()
	if err := x.remove(context.Background(), plan.Action{Kind: plan.RemoveReplica, Backend: "llama", Replica: 1, Host: "h", GPU: 0}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	var sawRm bool
	for _, c := range f.Commands {
		if strings.Contains(c, "rm -f llama-1") || strings.Contains(c, "rm -sf llama-1") {
			sawRm = true
		}
	}
	if !sawRm {
		t.Fatalf("want removal of llama-1; commands: %v", f.Commands)
	}
}
