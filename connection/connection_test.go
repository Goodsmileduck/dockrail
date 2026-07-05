package connection

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
)

func TestLocalRun(t *testing.T) {
	out, err := NewLocal().Run(context.Background(), "echo hello")
	if err != nil || strings.TrimSpace(out) != "hello" {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestLocalRunFailureIncludesStderr(t *testing.T) {
	_, err := NewLocal().Run(context.Background(), "echo oops >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("want stderr in error, got %v", err)
	}
}

func TestFakeRecordsAndStubs(t *testing.T) {
	f := NewFake()
	f.Stub("docker compose", "ok", nil)
	out, err := f.Run(context.Background(), "docker compose ps")
	if err != nil || out != "ok" {
		t.Fatalf("got %q, %v", out, err)
	}
	if len(f.Commands) != 1 || f.Commands[0] != "docker compose ps" {
		t.Fatalf("commands not recorded: %v", f.Commands)
	}
}

func TestSSHCommandLine(t *testing.T) {
	s := NewSSH("deploy@example.com", 32)
	args := s.sshArgs("docker ps")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p 32", "deploy@example.com", "ControlMaster=auto", "BatchMode=yes", "docker ps"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh args missing %q: %v", want, args)
		}
	}
}

func TestNewPicksImplementation(t *testing.T) {
	if _, ok := New(config.Target{}).(*Local); !ok {
		t.Error("empty host should give Local")
	}
	if _, ok := New(config.Target{Host: "a@b"}).(*SSH); !ok {
		t.Error("host should give SSH")
	}
}
