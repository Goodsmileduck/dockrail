package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
project: demo
compose: docker-compose.yml
target: { host: deploy@example.com, port: 32 }
services:
  web:
    image_tag: "abc123"
    readiness: { type: http, path: /health, port: 8010, timeout: 90s }
    cutover:   { strategy: recreate }
`

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(write(t, validYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Project != "demo" || cfg.Services["web"].Readiness.Port != 8010 {
		t.Errorf("bad parse: %+v", cfg)
	}
}

func TestLoadInvalid(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"missing project", strings.Replace(validYAML, "project: demo", "", 1), "project"},
		{"missing compose", strings.Replace(validYAML, "compose: docker-compose.yml", "", 1), "compose"},
		{"no services", "project: p\ncompose: c.yml\n", "services"},
		{"bad cutover", strings.Replace(validYAML, "strategy: recreate", "strategy: blue-green", 1), "cutover.strategy"},
		{"bad readiness", strings.Replace(validYAML, "type: http", "type: magic", 1), "readiness.type"},
		{"unsafe project", strings.Replace(validYAML, "project: demo", "project: \"my app; rm -rf\"", 1), "project"},
		{"unsafe service name", strings.Replace(validYAML, "  web:", "  \"web; rm -rf\":", 1), "name must match"},
		{"unsafe proxy", strings.Replace(validYAML, "strategy: recreate", "strategy: proxy, proxy: \"nginx; sh\"", 1), "cutover.proxy"},
		{"bad timeout", strings.Replace(validYAML, "timeout: 90s", "timeout: soon", 1), "timeout"},
		{"gpu needs pool", validYAML + "    placement: { type: gpu, vram_min: 20GiB }\n", "pool"},
		{"bad on_no_free_gpu", validYAML + "    placement: { type: gpu, pool: [0], vram_min: 20GiB, on_no_free_gpu: retry }\n", "on_no_free_gpu"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, c.yaml))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}
