package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	f.Stub("history.jsonl", cannedHistory, nil)
	if err := e.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.Commands, "\n")
	// ordered milestones of the recreate sequence:
	milestones := []string{
		"docker version",      // preflight
		"pull",                // recreate: pull NEW
		"stop web",            // recreate: stop OLD
		"up -d --no-deps web", // start NEW
		"curl",                // probe
		"history.jsonl",       // finalize: append record
		"image prune",         // finalize: prune
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
	if !strings.Contains(all, `"outcome":"deployed"`) {
		t.Errorf("history append must record deployed outcome:\n%s", all)
	}
}

func TestDeployWritesSecretsBeforePull(t *testing.T) {
	t.Setenv("APP_API_KEY", "s3cr3t")
	e, f := engineFixture()
	e.Cfg.Secrets.FromEnv = []string{"APP_API_KEY"}
	f.Stub("history.jsonl", cannedHistory, nil)
	if err := e.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.Commands, "\n")
	envIdx := strings.Index(all, ".dockrail/demo/env")
	pullIdx := strings.Index(all, "pull")
	if envIdx < 0 || pullIdx < 0 || envIdx > pullIdx {
		t.Fatalf("env-file must be written before pull:\n%s", all)
	}
	// every compose command must source the env-file
	if !strings.Contains(all, "set -a; . $HOME/.dockrail/demo/env") {
		t.Fatalf("compose commands must source secrets:\n%s", all)
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
	if !strings.Contains(all, "history.jsonl") || !strings.Contains(all, `"outcome":"failed@deploy"`) {
		t.Error("failure must be appended to history")
	}
}

func TestDeployAppendsFailureRecord(t *testing.T) {
	f := connection.NewFake()
	f.Stub("pull", "", fmt.Errorf("boom"))
	e := &Engine{Conn: f, Cfg: testCfg(false), Out: io.Discard}
	if err := e.Deploy(context.Background()); err == nil {
		t.Fatal("want deploy error")
	}
	var last string
	for _, c := range f.Commands {
		if strings.Contains(c, "history.jsonl") {
			last = c
		}
	}
	if last == "" || !strings.Contains(last, `"outcome":"failed@deploy"`) {
		t.Fatalf("no failure record appended: %s", last)
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
	f.Stub("history.jsonl", `{"ts":"2026-07-06T10:00:00Z","tag":"v2","performer":"ci","outcome":"deployed"}`+"\n", nil)
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
	f.Stub("history.jsonl", cannedHistory, nil)
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
		if strings.Contains(c, "history.jsonl") && strings.Contains(c, ">>") {
			saved = c
		}
	}
	if !strings.Contains(saved, `"tag":"v1"`) || !strings.Contains(saved, `"outcome":"rolled-back"`) {
		t.Fatalf("history not appended after rollback, got: %q", saved)
	}
}

func TestRollback_MultiServicePreservesAnchor(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", cannedHistory, nil)
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
	// History must be appended exactly once, regardless of service count, so
	// the anchor (previous successful record) is not clobbered by a second
	// service's write.
	var saves []string
	for _, c := range f.Commands {
		if strings.Contains(c, ">>") && strings.Contains(c, "history.jsonl") {
			saves = append(saves, c)
		}
	}
	if len(saves) != 1 {
		t.Fatalf("expected exactly one history append, got %d:\n%v", len(saves), saves)
	}
	if !strings.Contains(saves[0], `"tag":"v1"`) || !strings.Contains(saves[0], `"outcome":"rolled-back"`) {
		t.Fatalf("anchor not preserved: %q", saves[0])
	}
}

func TestDeployProxyStrategyRoutesCutover(t *testing.T) {
	t.Setenv("APP_API_KEY", "s3cr3t")
	e, f := engineFixture()
	e.Cfg.Secrets.FromEnv = []string{"APP_API_KEY"}
	e.Cfg.Services["web"] = bgService()
	f.Stub("history.jsonl", cannedHistory, nil)
	f.Stub("query-gpu", "0, 2000\n1, 40960\n", nil)
	f.Stub("ps -q web-blue", "cid-blue\n", nil)
	if err := e.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	all := strings.Join(f.Commands, "\n")
	if !strings.Contains(all, "up -d --no-deps web-green") {
		t.Fatalf("proxy strategy must start the inactive color:\n%s", all)
	}
	if !strings.Contains(all, ".dockrail/demo/nginx/web.conf") {
		t.Fatalf("proxy strategy must flip nginx upstream:\n%s", all)
	}
	if !strings.Contains(all, "set -a; . $HOME/.dockrail/demo/env; set +a; TAG=v2") {
		t.Fatalf("proxy compose commands must source secrets prefix:\n%s", all)
	}
}
