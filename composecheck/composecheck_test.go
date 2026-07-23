package composecheck

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
)

func writeCompose(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func cfgFor(t *testing.T, compose string, svcs map[string]config.Service) *config.Config {
	t.Helper()
	return &config.Config{Project: "demo", Compose: compose, Services: svcs}
}

func TestValidate_AllServicesPresent(t *testing.T) {
	p := writeCompose(t, `
services:
  web:
    image: ghcr.io/example/app:${TAG:-latest}
`)
	cfg := cfgFor(t, p, map[string]config.Service{
		"web": {ImageTag: "v1", Cutover: config.Cutover{Strategy: "recreate"}},
	})
	if errs := Validate(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidate_MissingService(t *testing.T) {
	p := writeCompose(t, `
services:
  other:
    image: ghcr.io/example/app:v1
`)
	cfg := cfgFor(t, p, map[string]config.Service{
		"web": {ImageTag: "v1", Cutover: config.Cutover{Strategy: "recreate"}},
	})
	errs := Validate(context.Background(), cfg)
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
}

func TestValidate_ProxyNeedsBlueGreen(t *testing.T) {
	p := writeCompose(t, `
services:
  api-blue:
    image: ghcr.io/example/api:v1
`)
	cfg := cfgFor(t, p, map[string]config.Service{
		"api": {ImageTag: "v1", Cutover: config.Cutover{Strategy: "proxy", Proxy: "nginx"}},
	})
	errs := Validate(context.Background(), cfg)
	if len(errs) != 1 { // api-green missing
		t.Fatalf("want 1 error (api-green missing), got %v", errs)
	}
}

func TestValidate_BadComposeYAML(t *testing.T) {
	p := writeCompose(t, "services: [not: a: mapping\n")
	cfg := cfgFor(t, p, map[string]config.Service{
		"web": {ImageTag: "v1"},
	})
	if errs := Validate(context.Background(), cfg); len(errs) == 0 {
		t.Fatal("want parse error, got none")
	}
}
