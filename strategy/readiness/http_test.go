package readiness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestNewHTTP(t *testing.T) {
	p, err := New(config.Readiness{Type: "http", Path: "/health", Port: 8010, Timeout: "90s"}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := p.(*HTTP)
	if h.Path != "/health" || h.Port != 8010 || h.Timeout != 90*time.Second {
		t.Errorf("bad fields: %+v", h)
	}
}

func TestNewUnknownType(t *testing.T) {
	if _, err := New(config.Readiness{Type: "bogus"}, ""); err == nil {
		t.Error("unknown readiness type; want error")
	}
}

func TestHTTPProbeSuccess(t *testing.T) {
	f := connection.NewFake()
	h := &HTTP{Path: "/health", Port: 8010, Timeout: 5 * time.Second}
	if err := h.Probe(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Commands[0], "curl") || !strings.Contains(f.Commands[0], "localhost:8010/health") {
		t.Errorf("unexpected probe command: %v", f.Commands)
	}
}

func TestHTTPProbeTimesOut(t *testing.T) {
	f := connection.NewFake()
	f.Stub("curl", "", errors.New("connection refused"))
	h := &HTTP{Path: "/health", Port: 8010, Timeout: 100 * time.Millisecond, retryEvery: 20 * time.Millisecond}
	err := h.Probe(context.Background(), f)
	if err == nil || !strings.Contains(err.Error(), "readiness") {
		t.Fatalf("want readiness timeout error, got %v", err)
	}
	if len(f.Commands) < 2 {
		t.Errorf("expected retries, got %d attempts", len(f.Commands))
	}
}
