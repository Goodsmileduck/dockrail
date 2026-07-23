package readiness

import (
	"context"
	"fmt"
	"time"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type HTTP struct {
	Path       string
	Port       int
	Timeout    time.Duration
	retryEvery time.Duration
}

func newHTTP(r config.Readiness) (*HTTP, error) {
	timeout := 60 * time.Second
	if r.Timeout != "" {
		var err error
		if timeout, err = time.ParseDuration(r.Timeout); err != nil {
			return nil, err
		}
	}
	return &HTTP{Path: r.Path, Port: r.Port, Timeout: timeout, retryEvery: 2 * time.Second}, nil
}

func (h *HTTP) Probe(ctx context.Context, conn connection.Connection, host string) error {
	cmd := fmt.Sprintf("curl -fsS -m 5 http://%s:%d%s >/dev/null", host, h.Port, h.Path)
	deadline := time.Now().Add(h.Timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, lastErr = conn.Run(ctx, cmd); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.retryEvery):
		}
	}
	return fmt.Errorf("readiness probe failed after %s: %w", h.Timeout, lastErr)
}
