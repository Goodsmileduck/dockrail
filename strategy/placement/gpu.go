package placement

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/vram"
)

// ErrNoFreeGPU signals that no GPU in the pool has enough free VRAM for a
// second model copy. The engine interprets it via on_no_free_gpu.
var ErrNoFreeGPU = errors.New("no free GPU with sufficient VRAM")

type GPU struct {
	pool    map[int]bool
	needMiB int
}

func newGPU(p config.Placement) (*GPU, error) {
	need, err := vram.NeededMiB(p.VRAMMin)
	if err != nil {
		return nil, err
	}
	pool := make(map[int]bool, len(p.Pool))
	for _, idx := range p.Pool {
		pool[idx] = true
	}
	return &GPU{pool: pool, needMiB: need}, nil
}

// Pick returns the index of a pool GPU with enough free VRAM, or ErrNoFreeGPU.
func (g *GPU) Pick(ctx context.Context, conn connection.Connection) (string, error) {
	out, err := conn.Run(ctx,
		"nvidia-smi --query-gpu=index,memory.free --format=csv,noheader,nounits")
	if err != nil {
		return "", fmt.Errorf("nvidia-smi: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			return "", fmt.Errorf("unexpected nvidia-smi line %q", line)
		}
		idx, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return "", fmt.Errorf("bad gpu index in %q: %w", line, err)
		}
		freeMiB, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return "", fmt.Errorf("bad free mem in %q: %w", line, err)
		}
		if g.pool[idx] && freeMiB >= g.needMiB {
			return strconv.Itoa(idx), nil
		}
	}
	return "", ErrNoFreeGPU
}
