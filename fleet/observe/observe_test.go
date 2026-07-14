package observe

import (
	"context"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
)

func TestObserve_TwoHosts(t *testing.T) {
	cfg := &fleet.Config{
		Project: "p",
		Hosts: map[string]fleet.Host{
			"gpu-a": {SSH: "u@a", GPUs: []int{0, 1}},
			"gpu-b": {SSH: "u@b"}, // no GPUs → no nvidia-smi call
		},
	}
	fakes := map[string]*connection.Fake{"gpu-a": connection.NewFake(), "gpu-b": connection.NewFake()}
	fakes["gpu-a"].Stub("docker ps", "chat-api\tregistry/chat:v2\nllama-0\tregistry/vllm:v1\n", nil)
	fakes["gpu-a"].Stub("nvidia-smi", "0, 24576, 0, 24576\n1, 24576, 20000, 4576\n", nil)
	fakes["gpu-b"].Stub("docker ps", "worker\tregistry/worker:v3\n", nil)

	o := &Observer{Cfg: cfg, Factory: func(name string, _ fleet.Host) (connection.Connection, error) {
		return fakes[name], nil
	}}
	st, err := o.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(st.Hosts) != 2 || st.Hosts[0].Name != "gpu-a" || st.Hosts[1].Name != "gpu-b" {
		t.Fatalf("hosts/order wrong: %+v", st.Hosts)
	}
	a := st.Hosts[0]
	if len(a.Containers) != 2 || a.Containers[0].Name != "chat-api" || a.Containers[0].Image != "registry/chat:v2" {
		t.Fatalf("gpu-a containers wrong: %+v", a.Containers)
	}
	if len(a.GPUs) != 2 || a.GPUs[1].FreeMiB != 4576 {
		t.Fatalf("gpu-a gpus wrong: %+v", a.GPUs)
	}
	// gpu-b has no declared GPUs: nvidia-smi must NOT have been called.
	for _, c := range fakes["gpu-b"].Commands {
		if c == gpuQuery {
			t.Fatalf("nvidia-smi should not run on gpu-b")
		}
	}
	if len(st.Hosts[1].GPUs) != 0 {
		t.Fatalf("gpu-b should have no gpus: %+v", st.Hosts[1].GPUs)
	}
}

func TestObserve_HostErrorRecorded(t *testing.T) {
	cfg := &fleet.Config{Project: "p", Hosts: map[string]fleet.Host{
		"good": {SSH: "u@g"},
		"bad":  {SSH: "u@b"},
	}}
	good := connection.NewFake()
	good.Stub("docker ps", "svc\timg:v1\n", nil)
	bad := connection.NewFake()
	bad.Stub("docker ps", "", context.DeadlineExceeded)

	o := &Observer{Cfg: cfg, Factory: func(name string, _ fleet.Host) (connection.Connection, error) {
		if name == "good" {
			return good, nil
		}
		return bad, nil
	}}
	st, err := o.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe must not fail wholesale: %v", err)
	}
	byName := map[string]HostState{}
	for _, h := range st.Hosts {
		byName[h.Name] = h
	}
	if byName["bad"].Err == "" {
		t.Fatalf("bad host should record an error")
	}
	if len(byName["good"].Containers) != 1 {
		t.Fatalf("good host should still be observed: %+v", byName["good"])
	}
}
