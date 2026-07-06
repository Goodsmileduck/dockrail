package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

func TestFlipUpstreamWritesAndReloads(t *testing.T) {
	f := connection.NewFake()
	if err := flipUpstream(context.Background(), f, "demo", "nginx", "web", "green", 8000); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.Commands, "\n")
	if !strings.Contains(all, ".dockrail/demo/nginx/web.conf") {
		t.Fatalf("must write per-service upstream fragment:\n%s", all)
	}
	if !strings.Contains(all, "server web-green:8000;") {
		t.Fatalf("upstream must target the active color:\n%s", all)
	}
	if !strings.Contains(all, "docker exec nginx nginx -s reload") {
		t.Fatalf("must reload nginx:\n%s", all)
	}
}
