package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/apply"
	"github.com/goodsmileduck/dockrail/fleet/observe"
)

func TestRunFleetStatus_Text(t *testing.T) {
	cfg := &fleet.Config{Project: "p", Hosts: map[string]fleet.Host{
		"gpu-a": {SSH: "u@a", GPUs: []int{0}},
	}}
	fake := connection.NewFake()
	fake.Stub("docker ps", "chat\treg/chat:v2\n", nil)
	fake.Stub("nvidia-smi", "0, 24576, 4000, 20576\n", nil)
	factory := func(name string, h fleet.Host) (connection.Connection, error) { return fake, nil }

	var buf bytes.Buffer
	if err := runFleetStatus(context.Background(), cfg, observe.ConnFactory(factory), &buf, false); err != nil {
		t.Fatalf("runFleetStatus: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"gpu-a", "chat", "reg/chat:v2", "gpu0", "20576"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunFleetStatus_JSON(t *testing.T) {
	cfg := &fleet.Config{Project: "p", Hosts: map[string]fleet.Host{"h": {SSH: "u@h"}}}
	fake := connection.NewFake()
	fake.Stub("docker ps", "svc\timg:v1\n", nil)
	factory := func(name string, h fleet.Host) (connection.Connection, error) { return fake, nil }

	var buf bytes.Buffer
	if err := runFleetStatus(context.Background(), cfg, observe.ConnFactory(factory), &buf, true); err != nil {
		t.Fatalf("runFleetStatus: %v", err)
	}
	if !strings.Contains(buf.String(), `"hosts"`) || !strings.Contains(buf.String(), `"img:v1"`) {
		t.Fatalf("json output wrong:\n%s", buf.String())
	}
}

func TestRunFleetPlan_Text(t *testing.T) {
	cfg := &fleet.Config{Project: "p", Hosts: map[string]fleet.Host{"h": {SSH: "u@h", GPUs: []int{0, 1}}},
		Backends: map[string]fleet.Backend{
			"llama": {ImageTag: "v2", Replicas: 1, Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}}},
		}}
	fake := connection.NewFake()
	// no containers running -> plan should place llama/0.
	fake.Stub("docker ps", "", nil)
	fake.Stub("nvidia-smi", "0, 24576, 0, 24576\n1, 24576, 0, 24576\n", nil)
	factory := func(name string, h fleet.Host) (connection.Connection, error) { return fake, nil }

	var buf bytes.Buffer
	if err := runFleetPlan(context.Background(), cfg, observe.ConnFactory(factory), &buf, false); err != nil {
		t.Fatalf("runFleetPlan: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"converge", "place", "llama/0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan output missing %q:\n%s", want, out)
		}
	}
}

func TestRunFleetApply_DryRunReadOnly(t *testing.T) {
	cfg := &fleet.Config{Project: "p", Compose: "docker-compose.yml",
		Hosts: map[string]fleet.Host{"h": {SSH: "u@h", GPUs: []int{0, 1}}},
		Backends: map[string]fleet.Backend{
			"llama": {Service: "vllm", ImageTag: "v2", Replicas: 1,
				Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}},
				Readiness: fleet.Readiness{Type: "tcp", Port: 8000}},
		}}
	fake := connection.NewFake()
	fake.Stub("docker ps", "", nil)
	fake.Stub("nvidia-smi", "0, 24576, 0, 24576\n1, 24576, 0, 24576\n", nil)
	factory := func(name string, h fleet.Host) (connection.Connection, error) { return fake, nil }

	var buf bytes.Buffer
	err := runFleetApply(context.Background(), cfg, observe.ConnFactory(factory), &buf,
		apply.Options{}, 0, true /*dryRun*/, false)
	if err != nil {
		t.Fatalf("runFleetApply dry-run: %v", err)
	}
	if !strings.Contains(buf.String(), "place") || !strings.Contains(buf.String(), "llama/0") {
		t.Fatalf("dry-run should print the plan:\n%s", buf.String())
	}
	for _, c := range fake.Commands {
		if strings.Contains(c, "up -d") || strings.Contains(c, "rm -f") || strings.Contains(c, "lock") {
			t.Fatalf("dry-run must not mutate or lock, saw: %q", c)
		}
	}
}

func TestRunFleetApply_ExecutesUnderLock(t *testing.T) {
	cfg := &fleet.Config{Project: "p", Compose: "docker-compose.yml",
		Hosts: map[string]fleet.Host{"h": {SSH: "u@h", GPUs: []int{0, 1}}},
		Backends: map[string]fleet.Backend{
			"llama": {Service: "vllm", ImageTag: "v2", Replicas: 1,
				Placement: fleet.Placement{VRAMMin: "10GiB", Pool: []string{"h"}, GPU: fleet.GPUSpec{Auto: true}},
				Readiness: fleet.Readiness{Type: "tcp", Port: 8000}},
		}}
	fake := connection.NewFake()
	fake.Stub("docker ps", "", nil)
	fake.Stub("nvidia-smi", "0, 24576, 0, 24576\n1, 24576, 0, 24576\n", nil)
	factory := func(name string, h fleet.Host) (connection.Connection, error) { return fake, nil }

	var buf bytes.Buffer
	err := runFleetApply(context.Background(), cfg, observe.ConnFactory(factory), &buf,
		apply.Options{OnFailure: "hold"}, 0, false, false)
	if err != nil {
		t.Fatalf("runFleetApply: %v", err)
	}
	var sawLock, sawUp bool
	for _, c := range fake.Commands {
		if strings.Contains(c, "lock") {
			sawLock = true
		}
		if strings.Contains(c, "up -d") && strings.Contains(c, "llama-0") {
			sawUp = true
		}
	}
	if !sawLock {
		t.Fatalf("expected a fleet-lock command; commands: %v", fake.Commands)
	}
	if !sawUp {
		t.Fatalf("expected a compose up for llama-0; commands: %v", fake.Commands)
	}
	if !strings.Contains(buf.String(), "applied") {
		t.Fatalf("expected an 'applied' line:\n%s", buf.String())
	}
}

func TestFleetApplyCmd_RegistersFlags(t *testing.T) {
	fleetCmd := newFleetCmd()
	var applyCmd *cobra.Command
	for _, c := range fleetCmd.Commands() {
		if c.Name() == "apply" {
			applyCmd = c
			break
		}
	}
	if applyCmd == nil {
		t.Fatal("fleet apply subcommand not registered")
	}
	for _, f := range []string{"on-failure", "scope", "lock-wait", "dry-run", "json"} {
		if applyCmd.Flags().Lookup(f) == nil {
			t.Fatalf("fleet apply missing --%s flag", f)
		}
	}
}
