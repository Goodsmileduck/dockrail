package engine

import (
	"bytes"
	"context"
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

func TestStatus_ReportsStateAndRunningTag(t *testing.T) {
	e, f := statusFixture()
	f.Stub("state.json", `{"previous_tag":"img:v1","current_tag":"img:v2","last_failure":"boom"}`, nil)
	f.Stub("ps -q web", "container123\n", nil)
	f.Stub("inspect", "img:v2\n", nil)

	rep, err := e.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if rep.CurrentTag != "img:v2" || rep.PreviousTag != "img:v1" || rep.LastFailure != "boom" {
		t.Fatalf("state fields wrong: %+v", rep)
	}
	if len(rep.Services) != 1 || rep.Services[0].Name != "web" ||
		rep.Services[0].RunningTag != "img:v2" || !rep.Services[0].Up {
		t.Fatalf("service status wrong: %+v", rep.Services)
	}
}

func TestStatus_ServiceDown(t *testing.T) {
	e, f := statusFixture()
	f.Stub("state.json", `{"current_tag":"img:v2"}`, nil)
	f.Stub("ps -q web", "\n", nil) // no running container

	rep, err := e.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(rep.Services) != 1 || rep.Services[0].Up || rep.Services[0].RunningTag != "" {
		t.Fatalf("expected web down with empty tag, got: %+v", rep.Services)
	}
}
