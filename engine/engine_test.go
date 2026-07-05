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

func engineFixture() (*Engine, *connection.Fake) {
	f := connection.NewFake()
	cfg := &config.Config{
		Project: "demo",
		Compose: "docker-compose.yml",
		Services: map[string]config.Service{
			"web": {
				ImageTag:  "v2",
				Readiness: config.Readiness{Type: "http", Path: "/health", Port: 8010, Timeout: "1s"},
				Cutover:   config.Cutover{Strategy: "recreate"},
			},
		},
	}
	return &Engine{Conn: f, Cfg: cfg, Out: &bytes.Buffer{}}, f
}

func TestDeployHappyPathCommandOrder(t *testing.T) {
	e, f := engineFixture()
	f.Stub("state.json", `{"current_tag":"v1"}`, nil)
	if err := e.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.Commands, "\n")
	// ordered milestones of the recreate sequence:
	milestones := []string{
		"docker version",      // preflight
		"state.json",          // read anchor (once, before any service)
		"pull",                // recreate: pull NEW
		"stop web",            // recreate: stop OLD
		"up -d --no-deps web", // start NEW
		"curl",                // probe
		"image prune",         // finalize
	}
	last := -1
	for _, m := range milestones {
		idx := strings.Index(all, m)
		if idx < 0 {
			t.Fatalf("missing milestone %q in commands:\n%s", m, all)
		}
		if idx < last {
			t.Fatalf("milestone %q out of order in:\n%s", m, all)
		}
		last = idx
	}
	if !strings.Contains(all, `"previous_tag":"v1"`) && !strings.Contains(all, `"previous_tag": "v1"`) {
		t.Errorf("state save must record previous tag v1:\n%s", all)
	}
}

func TestDeployReadinessFailureRecordsAndErrors(t *testing.T) {
	e, f := engineFixture()
	f.Stub("curl", "", errors.New("refused"))
	err := e.Deploy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "readiness") {
		t.Fatalf("want readiness error, got %v", err)
	}
	all := strings.Join(f.Commands, "\n")
	if !strings.Contains(all, "last_failure") {
		t.Error("failure must be persisted to state file")
	}
}

func TestDeployHeldLockFailsFast(t *testing.T) {
	e, f := engineFixture()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	err := e.Deploy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "another deploy") {
		t.Fatalf("want lock error, got %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "pull") {
		t.Error("deploy must not proceed while lock is held")
	}
}

func TestDeployReleasesLock(t *testing.T) {
	e, f := engineFixture()
	if err := e.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"), "rmdir $HOME/.dockrail/demo/lock") {
		t.Error("lock not released after deploy")
	}
}

func TestRollback_NoPreviousTag(t *testing.T) {
	e, f := engineFixture()
	f.Stub("state.json", `{"current_tag":"v2"}`, nil)
	err := e.Rollback(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no previous") {
		t.Fatalf("want 'no previous' error, got %v", err)
	}
	for _, c := range f.Commands {
		if strings.Contains(c, "up -d") || strings.Contains(c, "stop web") {
			t.Fatalf("rollback mutated host despite no previous tag: %q", c)
		}
	}
}

func TestRollback_RestoresPreviousTag(t *testing.T) {
	e, f := engineFixture()
	f.Stub("state.json", `{"previous_tag":"v1","current_tag":"v2"}`, nil)
	if err := e.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	cmds := strings.Join(f.Commands, "\n")
	if !strings.Contains(cmds, "TAG=v1 docker compose -f docker-compose.yml pull web") {
		t.Fatalf("expected pull of previous tag v1, got:\n%s", cmds)
	}
	if !strings.Contains(cmds, "TAG=v1 docker compose -f docker-compose.yml up -d --no-deps web") {
		t.Fatalf("expected up of previous tag v1, got:\n%s", cmds)
	}
	var saved string
	for _, c := range f.Commands {
		if strings.Contains(c, "state.json") && strings.Contains(c, "cat >") {
			saved = c
		}
	}
	if !strings.Contains(saved, `"current_tag":"v1"`) || !strings.Contains(saved, `"previous_tag":"v2"`) {
		t.Fatalf("state not swapped after rollback, got: %q", saved)
	}
}

func TestRollback_MultiServicePreservesAnchor(t *testing.T) {
	f := connection.NewFake()
	f.Stub("state.json", `{"previous_tag":"v1","current_tag":"v2"}`, nil)
	cfg := &config.Config{
		Project: "demo", Compose: "docker-compose.yml",
		Services: map[string]config.Service{
			"web": {ImageTag: "v2",
				Readiness: config.Readiness{Type: "http", Path: "/health", Port: 8010, Timeout: "1s"},
				Cutover:   config.Cutover{Strategy: "recreate"}},
			"worker": {ImageTag: "v2",
				Readiness: config.Readiness{Type: "http", Path: "/health", Port: 8020, Timeout: "1s"},
				Cutover:   config.Cutover{Strategy: "recreate"}},
		},
	}
	e := &Engine{Conn: f, Cfg: cfg, Out: &bytes.Buffer{}}
	if err := e.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// State must be written exactly once, regardless of service count, so the
	// anchor (previous_tag) is not clobbered by a second service's save.
	var saves []string
	for _, c := range f.Commands {
		if strings.Contains(c, "cat >") && strings.Contains(c, "state.json") {
			saves = append(saves, c)
		}
	}
	if len(saves) != 1 {
		t.Fatalf("expected exactly one state save, got %d:\n%v", len(saves), saves)
	}
	if !strings.Contains(saves[0], `"current_tag":"v1"`) || !strings.Contains(saves[0], `"previous_tag":"v2"`) {
		t.Fatalf("anchor not preserved: %q", saves[0])
	}
}

func TestDeployProxyStrategyNotImplemented(t *testing.T) {
	e, _ := engineFixture()
	svc := e.Cfg.Services["web"]
	svc.Cutover.Strategy = "proxy"
	e.Cfg.Services["web"] = svc
	err := e.Deploy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("want not-implemented error for proxy, got %v", err)
	}
}
