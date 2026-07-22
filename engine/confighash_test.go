package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func hashEngine(t *testing.T, tag string) (*Engine, *connection.Fake) {
	t.Helper()
	fake := connection.NewFake()
	fake.Stub("sha256sum", "abc123  docker-compose.yml\n", nil)
	cfg := &config.Config{Project: "demo", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {ImageTag: tag}}}
	return &Engine{Conn: fake, Cfg: cfg, Out: &strings.Builder{}}, fake
}

func TestDesiredHash_StableForSameInputs(t *testing.T) {
	e1, _ := hashEngine(t, "v1")
	e2, _ := hashEngine(t, "v1")
	h1, err := e1.desiredHash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := e2.desiredHash(context.Background())
	if h1 != h2 {
		t.Fatalf("hash unstable: %s vs %s", h1, h2)
	}
}

func TestDesiredHash_ChangesWithTag(t *testing.T) {
	e1, _ := hashEngine(t, "v1")
	e2, _ := hashEngine(t, "v2")
	h1, _ := e1.desiredHash(context.Background())
	h2, _ := e2.desiredHash(context.Background())
	if h1 == h2 {
		t.Fatal("hash must change when image_tag changes")
	}
}

func TestDesiredHash_ChangesWithRemoteCompose(t *testing.T) {
	e1, _ := hashEngine(t, "v1")
	fake2 := connection.NewFake()
	fake2.Stub("sha256sum", "def456  docker-compose.yml\n", nil)
	cfg2 := &config.Config{Project: "demo", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {ImageTag: "v1"}}}
	e2 := &Engine{Conn: fake2, Cfg: cfg2, Out: &strings.Builder{}}
	h1, _ := e1.desiredHash(context.Background())
	h2, _ := e2.desiredHash(context.Background())
	if h1 == h2 {
		t.Fatal("hash must change when remote compose file changes")
	}
}
