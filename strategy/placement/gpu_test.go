package placement

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

const smiHeader = "nvidia-smi --query-gpu=index,memory.free"

func TestGPUPicksFreeSlot(t *testing.T) {
	p, err := newGPU(config.Placement{Type: "gpu", Pool: []int{0, 1}, VRAMMin: "18GiB"})
	if err != nil {
		t.Fatal(err)
	}
	f := connection.NewFake()
	// GPU0 nearly full, GPU1 has 40GiB free -> pick "1".
	f.Stub(smiHeader, "0, 2000\n1, 40960\n", nil)
	got, err := p.Pick(context.Background(), f)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got != "1" {
		t.Fatalf("want GPU 1, got %q", got)
	}
}

func TestGPUNoFreeSlotSentinel(t *testing.T) {
	p, _ := newGPU(config.Placement{Type: "gpu", Pool: []int{0}, VRAMMin: "18GiB"})
	f := connection.NewFake()
	// 18GiB*1.2 = 22118 MiB required; 20000 free is not enough.
	f.Stub(smiHeader, "0, 20000\n", nil)
	_, err := p.Pick(context.Background(), f)
	if !errors.Is(err, ErrNoFreeGPU) {
		t.Fatalf("want ErrNoFreeGPU, got %v", err)
	}
}

func TestGPUIgnoresOutOfPool(t *testing.T) {
	p, _ := newGPU(config.Placement{Type: "gpu", Pool: []int{0}, VRAMMin: "1GiB"})
	f := connection.NewFake()
	// GPU3 is free but not in pool; GPU0 is full -> no free slot.
	f.Stub(smiHeader, "0, 100\n3, 80000\n", nil)
	_, err := p.Pick(context.Background(), f)
	if !errors.Is(err, ErrNoFreeGPU) {
		t.Fatalf("want ErrNoFreeGPU (pool-restricted), got %v", err)
	}
	_ = strings.TrimSpace
}
