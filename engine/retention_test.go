package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func minimalCfg() *config.Config {
	return &config.Config{
		Project:          "demo",
		Compose:          "docker-compose.yml",
		RetainContainers: 5,
		Services: map[string]config.Service{
			"api": {
				ImageTag:  "v1",
				Readiness: config.Readiness{Type: "http", Path: "/health", Port: 8000, Timeout: "1s"},
				Cutover:   config.Cutover{Strategy: "recreate"},
			},
		},
	}
}

func TestCaptureLogsSavesTailOfExistingSlot(t *testing.T) {
	f := connection.NewFake()
	f.Stub("ps -q", "abc123\n", nil)
	f.Stub("docker inspect --format '{{.Config.Image}}'", "registry.example.com/api:v1\n", nil)
	e := &Engine{Conn: f, Cfg: minimalCfg(), Out: discard()}
	e.captureLogs(context.Background(), "api", "api")
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "docker logs --tail 1000 abc123") ||
		!strings.Contains(joined, "/logs/api-v1-") {
		t.Fatalf("log capture missing:\n%s", joined)
	}
}

func TestCaptureLogsNoopWhenSlotEmpty(t *testing.T) {
	f := connection.NewFake() // ps -q returns ""
	e := &Engine{Conn: f, Cfg: minimalCfg(), Out: discard()}
	e.captureLogs(context.Background(), "api", "api")
	for _, c := range f.Commands {
		if strings.Contains(c, "docker logs") {
			t.Fatalf("unexpected log capture: %s", c)
		}
	}
}

func TestPruneRemovesOutOfWindowImagesAndLogs(t *testing.T) {
	f := connection.NewFake()
	f.Stub("TAG=v1 ", "registry.example.com/api:v1\n", nil)
	e := &Engine{Conn: f, Cfg: minimalCfg(), Out: discard()}
	e.Cfg.RetainContainers = 2
	h := []Record{
		{Tag: "v1", Outcome: "deployed"},
		{Tag: "v2", Outcome: "deployed"},
		{Tag: "v3", Outcome: "deployed"},
		{Tag: "v4", Outcome: "failed@readiness"}, // failed: exempt, never a prune victim
	}
	e.prune(context.Background(), h)
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "docker image rm") || !strings.Contains(joined, ":v1") {
		t.Fatalf("v1 image not pruned:\n%s", joined)
	}
	if strings.Contains(joined, "rm registry.example.com/api:v2") || strings.Contains(joined, ":v4") {
		t.Fatalf("in-window or failed tag pruned:\n%s", joined)
	}
	if !strings.Contains(joined, "-v1-") || !strings.Contains(joined, "logs/") {
		t.Fatalf("v1 saved logs not pruned:\n%s", joined)
	}
}
