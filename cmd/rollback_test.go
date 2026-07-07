package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestRollbackCommandRegistered(t *testing.T) {
	root := NewRootCmd()
	c, _, err := root.Find([]string{"rollback"})
	if err != nil {
		t.Fatalf("find rollback: %v", err)
	}
	if c.RunE == nil {
		t.Fatal("rollback has no RunE")
	}
}

func TestRunRollbackRestoresPreviousTag(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", `{"ts":"2026-07-06T10:00:00Z","tag":"v1","performer":"ci","outcome":"deployed"}
{"ts":"2026-07-06T11:00:00Z","tag":"v2","performer":"ci","outcome":"deployed"}
`, nil)
	cfg := &config.Config{
		Project: "demo", Compose: "docker-compose.yml", RetainContainers: 5,
		Services: map[string]config.Service{"web": {
			ImageTag:  "v2",
			Readiness: config.Readiness{Type: "http", Path: "/health", Port: 8010, Timeout: "1s"},
			Cutover:   config.Cutover{Strategy: "recreate"},
		}},
	}
	var out bytes.Buffer
	if err := runRollback(context.Background(), f, cfg, &out, "", 0); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"),
		"TAG=v1 docker compose -f docker-compose.yml up -d --no-deps web") {
		t.Fatalf("rollback did not recreate at previous tag v1:\n%s", strings.Join(f.Commands, "\n"))
	}
}

func TestRunRollbackToExplicitTag(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", `{"ts":"2026-07-06T10:00:00Z","tag":"v1","performer":"ci","outcome":"deployed"}
{"ts":"2026-07-06T11:00:00Z","tag":"v2","performer":"ci","outcome":"deployed"}
`, nil)
	f.Stub("docker image inspect", "sha256:abc", nil)
	cfg := &config.Config{
		Project: "demo", Compose: "docker-compose.yml", RetainContainers: 5,
		Services: map[string]config.Service{"web": {
			ImageTag:  "v2",
			Readiness: config.Readiness{Type: "http", Path: "/health", Port: 8010, Timeout: "1s"},
			Cutover:   config.Cutover{Strategy: "recreate"},
		}},
	}
	var out bytes.Buffer
	if err := runRollback(context.Background(), f, cfg, &out, "v1", 0); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"), "TAG=v1") {
		t.Fatalf("rollback did not reach RollbackTo for v1:\n%s", strings.Join(f.Commands, "\n"))
	}
}
