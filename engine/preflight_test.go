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

func proxyCfg() *config.Config {
	return &config.Config{
		Compose: "docker-compose.yml",
		Services: map[string]config.Service{
			"web": {Cutover: config.Cutover{Strategy: "proxy", Proxy: "nginx"}},
		},
	}
}

func TestPreflightFlagsSharedColorPort(t *testing.T) {
	f := connection.NewFake()
	f.Stub("config --format json",
		`{"services":{"web-blue":{"ports":[{"published":"8080","target":8080}]},`+
			`"web-green":{"ports":[{"published":"8080","target":8080}]}}}`, nil)
	errs := Preflight(context.Background(), f, proxyCfg())
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "8080") || !strings.Contains(joined, "web-green") {
		t.Fatalf("want a shared-port collision error mentioning 8080, got: %s", joined)
	}
}

func TestPreflightAllowsUnpublishedColors(t *testing.T) {
	f := connection.NewFake()
	f.Stub("config --format json",
		`{"services":{"web-blue":{"ports":[]},"web-green":{"ports":[]}}}`, nil)
	for _, e := range Preflight(context.Background(), f, proxyCfg()) {
		if strings.Contains(e.Error(), "host port") {
			t.Fatalf("unpublished colors must not trip the collision guard: %v", e)
		}
	}
}
