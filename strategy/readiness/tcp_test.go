package readiness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestTCPProbeSucceeds(t *testing.T) {
	p, err := newTCP(config.Readiness{Type: "tcp", Port: 8010, Timeout: "5s"})
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake() // unstubbed = success
	if err := p.Probe(context.Background(), f, "10.0.0.5"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"), "/dev/tcp/10.0.0.5/8010") {
		t.Fatalf("expected a /dev/tcp check, got %v", f.Commands)
	}
}

func TestTCPProbeTimesOut(t *testing.T) {
	p, err := newTCP(config.Readiness{Type: "tcp", Port: 8010, Timeout: "10ms"})
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake()
	f.Stub("/dev/tcp/localhost/8010", "", errors.New("refused"))
	if err := p.Probe(context.Background(), f, "localhost"); err == nil {
		t.Fatal("want timeout error")
	}
}
