package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func testCfg(gpu bool) *config.Config {
	svc := config.Service{
		ImageTag:  "t",
		Readiness: config.Readiness{Type: "http"},
		Cutover:   config.Cutover{Strategy: "recreate"},
	}
	if gpu {
		svc.Placement = config.Placement{Type: "gpu", Pool: []int{0}, VRAMMin: "1GiB"}
	}
	return &config.Config{
		Project:  "demo",
		Compose:  "docker-compose.yml",
		Services: map[string]config.Service{"web": svc},
	}
}

func TestPreflightHealthy(t *testing.T) {
	f := connection.NewFake()
	if errs := Preflight(context.Background(), f, testCfg(false)); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
	all := strings.Join(f.Commands, "\n")
	for _, want := range []string{"docker version", "docker compose version", "test -f docker-compose.yml"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing check %q in %v", want, f.Commands)
		}
	}
	if strings.Contains(all, "nvidia-smi") {
		t.Error("nvidia-smi must not run without gpu placement")
	}
}

func TestPreflightGPU(t *testing.T) {
	f := connection.NewFake()
	Preflight(context.Background(), f, testCfg(true))
	if !strings.Contains(strings.Join(f.Commands, "\n"), "nvidia-smi") {
		t.Error("nvidia-smi check missing for gpu placement")
	}
}

func TestPreflightCollectsAllFailures(t *testing.T) {
	f := connection.NewFake()
	f.Stub("docker version", "", errors.New("no docker"))
	f.Stub("test -f", "", errors.New("no file"))
	errs := Preflight(context.Background(), f, testCfg(false))
	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %v", errs)
	}
}
