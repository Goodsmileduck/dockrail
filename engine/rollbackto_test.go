package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const rollbackHistory = `{"ts":"1","tag":"v1","performer":"ci","outcome":"deployed"}
{"ts":"2","tag":"v2","performer":"ci","outcome":"deployed"}
{"ts":"3","tag":"v3","performer":"ci","outcome":"deployed"}
`

func TestRollbackToAcceptsRetainedTag(t *testing.T) {
	e, f := engineFixture()
	f.Stub("history.jsonl", rollbackHistory, nil)
	f.Stub("config --images", "registry.example.com/api:v1\n", nil)
	f.Stub("docker image inspect", "sha256:abc", nil)
	if err := e.RollbackTo(context.Background(), "v1"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "TAG=v1") || !strings.Contains(joined, `"outcome":"rolled-back"`) {
		t.Fatalf("no cutover at v1 / no rolled-back record:\n%s", joined)
	}
}

func TestRollbackToRejectsUnknownTag(t *testing.T) {
	e, f := engineFixture()
	f.Stub("history.jsonl", rollbackHistory, nil)
	err := e.RollbackTo(context.Background(), "v9")
	if err == nil || !strings.Contains(err.Error(), "v3") {
		t.Fatalf("want error listing candidates, got %v", err)
	}
}

func TestRollbackToRejectsPrunedImage(t *testing.T) {
	e, f := engineFixture()
	f.Stub("history.jsonl", rollbackHistory, nil)
	f.Stub("config --images", "registry.example.com/api:v1\n", nil)
	f.Stub("docker image inspect", "", fmt.Errorf("no such image"))
	if err := e.RollbackTo(context.Background(), "v1"); err == nil {
		t.Fatal("want error when image is gone from host")
	}
}
