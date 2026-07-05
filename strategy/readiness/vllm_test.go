package readiness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func TestVLLMProbeChecksHealthAndModel(t *testing.T) {
	p, err := newVLLM(config.Readiness{Type: "vllm", Port: 8000, Timeout: "5s"}, "Qwen2.5-VL")
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake()
	f.Stub("/health", "", nil)                                  // server up
	f.Stub("/v1/models", `{"data":[{"id":"Qwen2.5-VL"}]}`, nil) // model served
	if err := p.Probe(context.Background(), f); err != nil {
		t.Fatalf("probe: %v", err)
	}
	all := strings.Join(f.Commands, "\n")
	if !strings.Contains(all, ":8000/health") || !strings.Contains(all, ":8000/v1/models") {
		t.Fatalf("expected health + models checks, got:\n%s", all)
	}
}

func TestVLLMProbeFailsWhenModelAbsent(t *testing.T) {
	p, err := newVLLM(config.Readiness{Type: "vllm", Port: 8000, Timeout: "20ms"}, "Qwen2.5-VL")
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake()
	f.Stub("/health", "", nil)
	f.Stub("/v1/models", `{"data":[{"id":"other-model"}]}`, nil) // wrong model
	if err := p.Probe(context.Background(), f); err == nil {
		t.Fatal("want failure when configured model is not served")
	}
}

func TestVLLMProbeHealthOnlyWhenNoModel(t *testing.T) {
	p, err := newVLLM(config.Readiness{Type: "vllm", Port: 8000, Timeout: "5s"}, "")
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake()
	f.Stub("/health", "", nil)
	if err := p.Probe(context.Background(), f); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "/v1/models") {
		t.Fatal("must not check /v1/models when no model configured")
	}
	_ = errors.New // keep import
}
