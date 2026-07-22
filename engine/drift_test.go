package engine

import (
	"context"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func driftCfg(tag string) *config.Config {
	return &config.Config{Project: "demo", Compose: "docker-compose.yml",
		Services: map[string]config.Service{"web": {ImageTag: tag}}}
}

func TestImageDrift_UpToDate_NoFinding(t *testing.T) {
	fake := connection.NewFake()
	fake.Stub("docker image inspect", "example.com/app@sha256:aaa\n", nil)
	fake.Stub("imagetools inspect", "sha256:aaa\n", nil)
	got := ImageDrift(context.Background(), fake, driftCfg("example.com/app:prod"))
	if len(got) != 0 {
		t.Fatalf("want no findings, got %+v", got)
	}
}

func TestImageDrift_DigestMismatch_ReportsDrift(t *testing.T) {
	fake := connection.NewFake()
	fake.Stub("docker image inspect", "example.com/app@sha256:aaa\n", nil)
	fake.Stub("imagetools inspect", "sha256:bbb\n", nil)
	got := ImageDrift(context.Background(), fake, driftCfg("example.com/app:prod"))
	if len(got) != 1 || got[0].Note != "drift" || got[0].Remote != "sha256:bbb" {
		t.Fatalf("want one drift finding, got %+v", got)
	}
}

func TestImageDrift_PinnedImageSkipped(t *testing.T) {
	fake := connection.NewFake()
	got := ImageDrift(context.Background(), fake, driftCfg("example.com/app@sha256:aaa"))
	if len(got) != 0 || len(fake.Commands) != 0 {
		t.Fatalf("pinned image must be skipped without host commands, got %+v / %v", got, fake.Commands)
	}
}

func TestImageDrift_RemoteLookupFails_ReportsNote(t *testing.T) {
	fake := connection.NewFake()
	fake.Stub("docker image inspect", "example.com/app@sha256:aaa\n", nil)
	fake.Stub("imagetools inspect", "", context.DeadlineExceeded)
	got := ImageDrift(context.Background(), fake, driftCfg("example.com/app:prod"))
	if len(got) != 1 || got[0].Note == "drift" {
		t.Fatalf("want lookup-failure note, got %+v", got)
	}
}
