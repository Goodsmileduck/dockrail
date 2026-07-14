package apply

import (
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/fleet/observe"
)

func TestReplicaOverride(t *testing.T) {
	got := replicaOverride("docker-compose.yml", "vllm", "llama-70b", 2, 1)
	for _, want := range []string{
		"llama-70b-2:",
		"file: docker-compose.yml",
		"service: vllm",
		"container_name: llama-70b-2",
		observe.LabelManaged + `: "true"`,
		observe.LabelBackend + ": llama-70b",
		observe.LabelReplica + `: "2"`,
		observe.LabelGPU + `: "1"`,
		`device_ids: ["1"]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("replica override missing %q:\n%s", want, got)
		}
	}
}

func TestServiceOverride(t *testing.T) {
	got := serviceOverride("docker-compose.yml", "chat-api", "chat-api")
	for _, want := range []string{"chat-api:", "service: chat-api", "container_name: chat-api", observe.LabelService + ": chat-api"} {
		if !strings.Contains(got, want) {
			t.Fatalf("service override missing %q:\n%s", want, got)
		}
	}
}
