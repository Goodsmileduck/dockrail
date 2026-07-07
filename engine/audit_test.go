package engine

import (
	"context"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

func TestAuditLimitsAndMarksAnchor(t *testing.T) {
	f := connection.NewFake()
	f.Stub("history.jsonl", `{"ts":"2026-07-06T10:00:00Z","tag":"v1","performer":"ci","outcome":"deployed"}
{"ts":"2026-07-06T11:00:00Z","tag":"v2","performer":"ci","outcome":"failed@readiness"}
{"ts":"2026-07-06T12:00:00Z","tag":"v2","performer":"ci","outcome":"deployed"}
`, nil)
	e := &Engine{Conn: f, Cfg: bgCfg(), Out: discard()}
	recs, anchor, err := e.Audit(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Outcome != "failed@readiness" {
		t.Fatalf("recs = %+v", recs)
	}
	if anchor != 1 {
		t.Fatalf("anchor = %d, want 1 (the trailing deployed record)", anchor)
	}
}
