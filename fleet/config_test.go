package fleet

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fleet.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

const goodFleet = `
project: ml-platform
registry: { server: registry.example.com/acme/ml }
hosts:
  gpu-a:
    ssh: deploy@gpu-a.example.com
    gpus: [0,1,2,3]
  gpu-b:
    ssh: deploy@gpu-b.example.com
    port: 32
    gpus: [0,1]
backends:
  llama-70b:
    image_tag: "vllm-v0.9.2"
    model: /models/llama70b/best
    replicas: 3
    placement:
      vram_min: "20GiB"
      gpu: auto
      pool: [gpu-a, gpu-b]
    readiness: { type: vllm, timeout: 300s }
  embed-small:
    image_tag: "vllm-v0.9.2"
    replicas: 2
    placement:
      vram_min: "6GiB"
      gpu: [gpu-a:2, gpu-a:3]
    readiness: { type: vllm, timeout: 180s }
services:
  chat-api:
    host: gpu-a
    image_tag: "${TAG}"
    uses:
      - backend: llama-70b
        wiring: { strategy: nginx-upstream }
    readiness: { type: http, path: /health, port: 8080 }
    cutover: { strategy: proxy }
`

func TestLoad_GoodFleet(t *testing.T) {
	cfg, err := Load(writeTemp(t, goodFleet))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Project != "ml-platform" {
		t.Fatalf("project = %q", cfg.Project)
	}
	if len(cfg.Hosts) != 2 || cfg.Hosts["gpu-b"].Port != 32 ||
		len(cfg.Hosts["gpu-a"].GPUs) != 4 {
		t.Fatalf("hosts wrong: %+v", cfg.Hosts)
	}
	if cfg.Backends["llama-70b"].Replicas != 3 ||
		!cfg.Backends["llama-70b"].Placement.GPU.Auto {
		t.Fatalf("llama backend wrong: %+v", cfg.Backends["llama-70b"])
	}
	if got := cfg.Backends["embed-small"].Placement.GPU.Pins; len(got) != 2 || got[0] != "gpu-a:2" {
		t.Fatalf("embed pins wrong: %+v", got)
	}
	if cfg.Backends["embed-small"].Placement.GPU.Auto {
		t.Fatalf("embed should not be auto")
	}
	svc := cfg.Services["chat-api"]
	if svc.Host != "gpu-a" || len(svc.Uses) != 1 ||
		svc.Uses[0].Backend != "llama-70b" || svc.Uses[0].Wiring.Strategy != "nginx-upstream" {
		t.Fatalf("service wrong: %+v", svc)
	}
}

func TestLoad_RejectsUnknownKey(t *testing.T) {
	_, err := Load(writeTemp(t, goodFleet+"\nbogus_top: 1\n"))
	if err == nil {
		t.Fatal("expected error on unknown key")
	}
}
