package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

const cannedHistory = `{"ts":"2026-07-06T10:00:00Z","tag":"v1","performer":"ci","outcome":"deployed"}
{"ts":"2026-07-06T11:00:00Z","tag":"v2","performer":"alice","outcome":"failed@readiness"}
{"ts":"2026-07-06T12:00:00Z","tag":"v2","performer":"alice","outcome":"deployed"}
`

func TestLoadHistoryParsesLines(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", cannedHistory, nil)
	h, err := loadHistory(context.Background(), f, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 || h[1].Outcome != "failed@readiness" {
		t.Fatalf("got %+v", h)
	}
}

func TestLoadHistoryEmptyFileIsFirstDeploy(t *testing.T) {
	f := connection.NewFake()
	h, err := loadHistory(context.Background(), f, "proj")
	if err != nil || len(h) != 0 {
		t.Fatalf("h=%v err=%v", h, err)
	}
}

func TestLoadHistoryCorruptLineFails(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", "{not json}\n", nil)
	if _, err := loadHistory(context.Background(), f, "proj"); err == nil {
		t.Fatal("want error on corrupt history")
	}
}

func TestCurrentAndPreviousDerivation(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", cannedHistory, nil)
	h, _ := loadHistory(context.Background(), f, "proj")
	cur, ok := currentRecord(h)
	if !ok || cur.Tag != "v2" {
		t.Fatalf("current = %+v ok=%v", cur, ok)
	}
	if prev := previousTag(h); prev != "v1" {
		t.Fatalf("previous = %q, want v1", prev)
	}
}

func TestPreviousSkipsSameTagAndFailures(t *testing.T) {
	h := []Record{
		{Tag: "v1", Outcome: "deployed"},
		{Tag: "v2", Outcome: "deployed"},
		{Tag: "v1", Outcome: "rolled-back"},
	}
	// current is the rolled-back v1; previous must be v2, not the older v1.
	if prev := previousTag(h); prev != "v2" {
		t.Fatalf("previous = %q, want v2", prev)
	}
}

func TestLastFailureOnlyAfterAnchor(t *testing.T) {
	h := []Record{
		{Tag: "v1", Outcome: "deployed"},
		{Tag: "v2", Outcome: "failed@readiness"},
	}
	if lf := lastFailure(h); !strings.Contains(lf, "v2") {
		t.Fatalf("lastFailure = %q", lf)
	}
	h = append(h, Record{Tag: "v2", Outcome: "deployed"})
	if lf := lastFailure(h); lf != "" {
		t.Fatalf("lastFailure after success = %q, want empty", lf)
	}
}

func TestAppendRecordIssuesAppendAndFillsFields(t *testing.T) {
	t.Setenv("DOCKRAIL_PERFORMER", "ci-bot")
	f := connection.NewFake()
	err := appendRecord(context.Background(), f, "proj", Record{Tag: "v3", Outcome: "deployed"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := f.Commands[len(f.Commands)-1]
	if !strings.Contains(cmd, ">>") || !strings.Contains(cmd, "history.jsonl") {
		t.Fatalf("not an append: %s", cmd)
	}
	if !strings.Contains(cmd, `"performer":"ci-bot"`) || !strings.Contains(cmd, `"ts":"`) {
		t.Fatalf("fields not filled: %s", cmd)
	}
}
