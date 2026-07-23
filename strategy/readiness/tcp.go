package readiness

import (
	"context"
	"fmt"
	"time"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type TCP struct {
	Port       int
	Timeout    time.Duration
	retryEvery time.Duration
}

func newTCP(r config.Readiness) (*TCP, error) {
	timeout := 60 * time.Second
	if r.Timeout != "" {
		var err error
		if timeout, err = time.ParseDuration(r.Timeout); err != nil {
			return nil, err
		}
	}
	if r.Port == 0 {
		return nil, fmt.Errorf("tcp readiness requires a port")
	}
	return &TCP{Port: r.Port, Timeout: timeout, retryEvery: 2 * time.Second}, nil
}

func (p *TCP) Probe(ctx context.Context, conn connection.Connection, host string) error {
	// bash's /dev/tcp pseudo-device opens a connection; redirect closes it.
	cmd := fmt.Sprintf("timeout 5 bash -c '</dev/tcp/%s/%d' 2>/dev/null", host, p.Port)
	deadline := time.Now().Add(p.Timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, lastErr = conn.Run(ctx, cmd); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.retryEvery):
		}
	}
	return fmt.Errorf("tcp readiness on port %d failed after %s: %w", p.Port, p.Timeout, lastErr)
}
