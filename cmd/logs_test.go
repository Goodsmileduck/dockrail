package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestLogsCommandRequiresService(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"logs"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when no service given")
	}
}

func TestRunLogsEmitsComposeCommand(t *testing.T) {
	f := connection.NewFake()
	f.Stub("logs", "line1\nline2\n", nil)
	cfg := &config.Config{
		Project: "demo", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {ImageTag: "v2",
			Readiness: config.Readiness{Type: "http"},
			Cutover:   config.Cutover{Strategy: "recreate"}}},
	}
	var out bytes.Buffer
	if err := runLogs(context.Background(), f, cfg, "web", 50, false, &out); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"),
		"docker compose -f docker-compose.yml logs --tail 50 web") {
		t.Fatalf("unexpected logs command: %v", f.Commands)
	}
	if out.String() != "line1\nline2\n" {
		t.Fatalf("logs output not forwarded: %q", out.String())
	}
}
