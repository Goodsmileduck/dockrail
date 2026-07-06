package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func testConfig() *config.Config {
	return &config.Config{
		Project: "demo", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {
			ImageTag:  "v2",
			Readiness: config.Readiness{Type: "http"},
			Cutover:   config.Cutover{Strategy: "recreate"},
		}},
	}
}

func TestAuditPrintsTableWithAnchorMark(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl",
		`{"ts":"2026-07-06T10:00:00Z","tag":"v1","performer":"ci","outcome":"deployed"}
{"ts":"2026-07-06T12:00:00Z","tag":"v2","performer":"alice","outcome":"deployed"}
`, nil)
	var buf bytes.Buffer
	if err := runAudit(context.Background(), f, testConfig(), &buf, 20); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "v2") || !strings.Contains(out, "alice") {
		t.Fatalf("missing columns:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "v2") && !strings.Contains(line, "*") {
			t.Fatalf("current anchor line not marked:\n%s", out)
		}
	}
}
