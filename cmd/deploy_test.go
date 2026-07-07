package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestDryRunPrintsPlanWithoutMutating(t *testing.T) {
	f := connection.NewFake()
	cfg := &config.Config{
		Project: "demo", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {
			ImageTag:  "v2",
			Readiness: config.Readiness{Type: "http", Path: "/health", Port: 8010},
			Cutover:   config.Cutover{Strategy: "recreate"},
		}},
	}
	var out bytes.Buffer
	if err := runDeploy(context.Background(), f, cfg, &out, true, 0); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"plan pull web tag v2", "plan recreate web", "plan readiness http :8010/health"} {
		if !strings.Contains(text, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, text)
		}
	}
	for _, c := range f.Commands {
		for _, forbidden := range []string{"pull", "up -d", "stop", "curl", "DDEOF"} {
			if strings.Contains(c, forbidden) {
				t.Errorf("dry-run executed mutating command: %q", c)
			}
		}
	}
}
