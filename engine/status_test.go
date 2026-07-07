package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func statusFixture() (*Engine, *connection.Fake) {
	f := connection.NewFake()
	cfg := &config.Config{
		Project: "proj", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {
			ImageTag:  "img:v2",
			Readiness: config.Readiness{Type: "http"},
			Cutover:   config.Cutover{Strategy: "recreate"},
		}},
	}
	return &Engine{Conn: f, Cfg: cfg, Out: &bytes.Buffer{}}, f
}

func TestStatus_ReportsHistoryAndRunningTag(t *testing.T) {
	e, f := statusFixture()
	f.Stub("history.jsonl", `{"ts":"2026-07-06T10:00:00Z","tag":"img:v1","performer":"ci","outcome":"deployed"}
{"ts":"2026-07-06T11:00:00Z","tag":"img:v2","performer":"ci","outcome":"deployed"}
`, nil)
	f.Stub("ps -q web", "container123\n", nil)
	f.Stub("inspect", "img:v2\n", nil)

	rep, err := e.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if rep.CurrentTag != "img:v2" || rep.PreviousTag != "img:v1" || rep.LastFailure != "" {
		t.Fatalf("derived fields wrong: %+v", rep)
	}
	if len(rep.Services) != 1 || rep.Services[0].Name != "web" ||
		rep.Services[0].RunningTag != "img:v2" || !rep.Services[0].Up {
		t.Fatalf("service status wrong: %+v", rep.Services)
	}
}

func TestStatus_ReportsLastFailure(t *testing.T) {
	e, f := statusFixture()
	f.Stub("history.jsonl", `{"ts":"2026-07-06T10:00:00Z","tag":"img:v1","performer":"ci","outcome":"deployed"}
{"ts":"2026-07-06T11:00:00Z","tag":"img:v2","performer":"ci","outcome":"failed@readiness"}
`, nil)
	f.Stub("ps -q web", "container123\n", nil)
	f.Stub("inspect", "img:v1\n", nil)

	rep, err := e.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if rep.CurrentTag != "img:v1" {
		t.Fatalf("current tag should remain last success: %+v", rep)
	}
	if !strings.Contains(rep.LastFailure, "img:v2") || !strings.Contains(rep.LastFailure, "failed@readiness") {
		t.Fatalf("last failure not derived: %+v", rep)
	}
}

func TestStatus_ServiceDown(t *testing.T) {
	e, f := statusFixture()
	f.Stub("history.jsonl", `{"ts":"2026-07-06T10:00:00Z","tag":"img:v2","performer":"ci","outcome":"deployed"}
`, nil)
	f.Stub("ps -q web", "\n", nil) // no running container

	rep, err := e.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(rep.Services) != 1 || rep.Services[0].Up || rep.Services[0].RunningTag != "" {
		t.Fatalf("expected web down with empty tag, got: %+v", rep.Services)
	}
}
