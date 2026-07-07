package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func lockTestCfg() *config.Config {
	return &config.Config{Project: "demo", Compose: "docker-compose.yml"}
}

func TestRunLockStatusFree(t *testing.T) {
	var buf bytes.Buffer
	held, err := runLockStatus(context.Background(), connection.NewFake(), lockTestCfg(), &buf)
	if err != nil || held {
		t.Fatalf("want free, got held=%v err=%v", held, err)
	}
	if !strings.Contains(buf.String(), "free") {
		t.Errorf("output %q", buf.String())
	}
}

func TestRunLockStatusHeld(t *testing.T) {
	f := connection.NewFake()
	f.Stub("if test -d $HOME/.dockrail/demo/lock", "held", nil)
	f.Stub("cat $HOME/.dockrail/demo/lock/info.json",
		`{"acquired_at":"2026-07-07T10:00:00Z","tag":"v41","by":"ci@runner"}`, nil)
	var buf bytes.Buffer
	held, err := runLockStatus(context.Background(), f, lockTestCfg(), &buf)
	if err != nil || !held {
		t.Fatalf("want held, got held=%v err=%v", held, err)
	}
	if !strings.Contains(buf.String(), "held by ci@runner") {
		t.Errorf("output %q", buf.String())
	}
}

func TestRunLockAcquireAndRelease(t *testing.T) {
	var buf bytes.Buffer
	f := connection.NewFake()
	if err := runLockAcquire(context.Background(), f, lockTestCfg(), &buf); err != nil {
		t.Fatal(err)
	}
	// release on a "held" host
	f2 := connection.NewFake()
	f2.Stub("if test -d $HOME/.dockrail/demo/lock", "held", nil)
	if err := runLockRelease(context.Background(), f2, lockTestCfg(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "released") {
		t.Errorf("output %q", buf.String())
	}
}

func TestRunLockReleaseWhenFree(t *testing.T) {
	var buf bytes.Buffer
	if err := runLockRelease(context.Background(), connection.NewFake(), lockTestCfg(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "lock is not held") {
		t.Errorf("output %q", buf.String())
	}
}
