package engine

import (
	"context"
	"fmt"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type check struct {
	name string
	cmd  string
}

func Preflight(ctx context.Context, conn connection.Connection, cfg *config.Config) []error {
	checks := []check{
		{"docker", "docker version --format ok"},
		{"compose plugin", "docker compose version"},
		{"compose file", fmt.Sprintf("test -f %s", cfg.Compose)},
	}
	for _, s := range cfg.Services {
		if s.Placement.Type == "gpu" {
			checks = append(checks, check{"gpu driver", "nvidia-smi -L"})
			break
		}
	}
	var errs []error
	for _, c := range checks {
		if _, err := conn.Run(ctx, c.cmd); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
		}
	}
	return errs
}
