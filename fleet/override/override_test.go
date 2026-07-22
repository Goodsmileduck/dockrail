package override

import (
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/fleet/observe"
)

func TestHash_DeterministicAndOrderSensitive(t *testing.T) {
	a := Hash("img:v1", "compose.yml", "vllm", "b1", "0", "1")
	b := Hash("img:v1", "compose.yml", "vllm", "b1", "0", "1")
	c := Hash("img:v1", "compose.yml", "vllm", "b1", "1", "0")
	if a != b {
		t.Fatal("hash not deterministic")
	}
	if a == c {
		t.Fatal("hash ignored argument order")
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("want sha256: prefix, got %s", a)
	}
}

func TestReplica_StampsConfigHashLabel(t *testing.T) {
	body := Replica("compose.yml", "vllm", "b1", 0, 1, "example.com/vllm:v2")
	hash := ReplicaHash("compose.yml", "vllm", "b1", 0, 1, "example.com/vllm:v2")
	if !strings.Contains(body, `dockrail.config-hash: "`+hash+`"`) {
		t.Fatalf("override missing config-hash label:\n%s", body)
	}
	// existing identity labels must survive the move
	for _, want := range []string{"dockrail.managed", "dockrail.backend: b1", `dockrail.replica: "0"`, `dockrail.gpu: "1"`, `device_ids: ["1"]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("override missing %q:\n%s", want, body)
		}
	}
}

func TestService_StampsConfigHashLabel(t *testing.T) {
	body := Service("compose.yml", "api-tpl", "api", "example.com/api:v3")
	hash := ServiceHash("compose.yml", "api-tpl", "api", "example.com/api:v3")
	if !strings.Contains(body, `dockrail.config-hash: "`+hash+`"`) {
		t.Fatalf("override missing config-hash label:\n%s", body)
	}
}

func TestHashes_NormalizeComposePath(t *testing.T) {
	if ReplicaHash("/srv/app/compose.yml", "vllm", "b1", 0, 1, "v1") != ReplicaHash("compose.yml", "vllm", "b1", 0, 1, "v1") {
		t.Fatal("ReplicaHash must normalize the compose path with filepath.Base")
	}
	if ServiceHash("/srv/app/compose.yml", "api-tpl", "api", "v1") != ServiceHash("compose.yml", "api-tpl", "api", "v1") {
		t.Fatal("ServiceHash must normalize the compose path with filepath.Base")
	}
}

func TestReplicaOverride(t *testing.T) {
	got := Replica("docker-compose.yml", "vllm", "llama-70b", 2, 1, "example.com/llama:v1")
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
	got := Service("docker-compose.yml", "chat-api", "chat-api", "example.com/chat-api:v1")
	for _, want := range []string{"chat-api:", "service: chat-api", "container_name: chat-api", observe.LabelService + ": chat-api"} {
		if !strings.Contains(got, want) {
			t.Fatalf("service override missing %q:\n%s", want, got)
		}
	}
}
