package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func bgService() config.Service {
	return config.Service{
		ImageTag:  "v2",
		Readiness: config.Readiness{Type: "http", Path: "/health", Port: 8000, Timeout: "1s"},
		Cutover:   config.Cutover{Strategy: "proxy", Proxy: "nginx"},
		Placement: config.Placement{Type: "gpu", Pool: []int{0, 1}, VRAMMin: "18GiB", OnNoFreeGPU: "stop-old-first"},
	}
}

func bgCfg() *config.Config {
	return &config.Config{
		Project: "demo",
		Compose: "docker-compose.yml",
		Services: map[string]config.Service{
			"web": bgService(),
		},
	}
}

func discard() *bytes.Buffer {
	return &bytes.Buffer{}
}

// Free slot -> green starts, flip, blue stops. Blue never stopped before flip.
func TestProxyCutoverFreeSlotIsZeroGap(t *testing.T) {
	f := connection.NewFake()
	f.Stub("query-gpu", "0, 2000\n1, 40960\n", nil) // GPU1 free
	f.Stub("ps -q web-blue", "cid-blue\n", nil)     // blue active
	e := &Engine{Conn: f, Cfg: bgCfg(), Out: discard()}
	if err := e.proxyCutover(context.Background(), "web", bgService(), "v2", ""); err != nil {
		t.Fatalf("cutover: %v", err)
	}
	all := strings.Join(f.Commands, "\n")
	upGreen := strings.Index(all, "up -d --no-deps web-green")
	flip := strings.Index(all, "web.conf")
	stopBlue := strings.Index(all, "stop web-blue")
	if !(upGreen >= 0 && flip > upGreen && stopBlue > flip) {
		t.Fatalf("order must be up-green -> flip -> stop-blue:\n%s", all)
	}
	if strings.Contains(all, "DOCKRAIL_GPU=1") == false {
		t.Fatalf("green must be pinned to the free GPU:\n%s", all)
	}
}

// No free slot + stop-old-first -> blue stopped, green up, flip. Gap accepted.
func TestProxyCutoverStopOldFirst(t *testing.T) {
	f := connection.NewFake()
	f.Stub("query-gpu", "0, 1000\n1, 1000\n", nil) // none free
	f.Stub("ps -q web-blue", "cid-blue\n", nil)
	e := &Engine{Conn: f, Cfg: bgCfg(), Out: discard()}
	if err := e.proxyCutover(context.Background(), "web", bgService(), "v2", ""); err != nil {
		t.Fatalf("cutover: %v", err)
	}
	all := strings.Join(f.Commands, "\n")
	stopBlue := strings.Index(all, "stop web-blue")
	upGreen := strings.Index(all, "up -d --no-deps web-green")
	if !(stopBlue >= 0 && upGreen > stopBlue) {
		t.Fatalf("stop-old-first: blue must stop before green starts:\n%s", all)
	}
}

// No free slot + fail -> abort, nothing mutated.
func TestProxyCutoverFailBranch(t *testing.T) {
	f := connection.NewFake()
	f.Stub("query-gpu", "0, 1000\n", nil)
	f.Stub("ps -q web-blue", "cid-blue\n", nil)
	svc := bgService()
	svc.Placement.Pool = []int{0}
	svc.Placement.OnNoFreeGPU = "fail"
	e := &Engine{Conn: f, Cfg: bgCfg(), Out: discard()}
	err := e.proxyCutover(context.Background(), "web", svc, "v2", "")
	if err == nil || !strings.Contains(err.Error(), "no free GPU") {
		t.Fatalf("want no-free-GPU abort, got %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "up -d") {
		t.Fatal("fail branch must not start anything")
	}
	_ = errors.Is
}

// stop-old-first green fails readiness -> auto-rollback restarts blue.
func TestProxyCutoverAutoRollback(t *testing.T) {
	f := connection.NewFake()
	f.Stub("query-gpu", "0, 1000\n", nil)
	f.Stub("ps -q web-blue", "cid-blue\n", nil)
	f.Stub("curl", "", errors.New("green not ready")) // readiness fails
	svc := bgService()
	svc.Placement.Pool = []int{0}
	e := &Engine{Conn: f, Cfg: bgCfg(), Out: discard()}
	err := e.proxyCutover(context.Background(), "web", svc, "v2", "")
	if err == nil {
		t.Fatal("want cutover error")
	}
	all := strings.Join(f.Commands, "\n")
	// blue must be brought back up after green failed
	if !strings.Contains(all, "up -d --no-deps web-blue") {
		t.Fatalf("auto-rollback must restart blue:\n%s", all)
	}
}
