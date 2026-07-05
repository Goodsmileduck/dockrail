package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestRunStatusPrintsReport(t *testing.T) {
	f := connection.NewFake()
	f.Stub("state.json", `{"previous_tag":"v1","current_tag":"v2"}`, nil)
	f.Stub("ps -q web", "cid1\n", nil)
	f.Stub("inspect", "v2\n", nil)
	cfg := &config.Config{
		Project: "demo", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {
			ImageTag:  "v2",
			Readiness: config.Readiness{Type: "http"},
			Cutover:   config.Cutover{Strategy: "recreate"},
		}},
	}
	var out bytes.Buffer
	if err := runStatus(context.Background(), f, cfg, &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	text := out.String()
	for _, want := range []string{"current_tag:  v2", "previous_tag: v1", "web: v2 (up)"} {
		if !strings.Contains(text, want) {
			t.Errorf("status output missing %q:\n%s", want, text)
		}
	}
}
