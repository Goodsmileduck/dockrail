package readiness

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

// VLLM waits for a vLLM server to become ready. Unlike a plain HTTP probe it
// confirms the *model* is loaded and served: /health returning 200 only means
// the process is up, while weight loading into VRAM can take minutes and the
// model appears in /v1/models only once it is servable.
type VLLM struct {
	Port       int
	Model      string
	Timeout    time.Duration
	retryEvery time.Duration
}

func newVLLM(r config.Readiness, model string) (*VLLM, error) {
	timeout := 600 * time.Second // weight loading is slow
	if r.Timeout != "" {
		var err error
		if timeout, err = time.ParseDuration(r.Timeout); err != nil {
			return nil, err
		}
	}
	port := r.Port
	if port == 0 {
		port = 8000 // vLLM default
	}
	return &VLLM{Port: port, Model: model, Timeout: timeout, retryEvery: 2 * time.Second}, nil
}

func (p *VLLM) Probe(ctx context.Context, conn connection.Connection) error {
	health := fmt.Sprintf("curl -fsS -m 5 http://localhost:%d/health >/dev/null", p.Port)
	models := fmt.Sprintf("curl -fsS -m 5 http://localhost:%d/v1/models", p.Port)
	deadline := time.Now().Add(p.Timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = p.check(ctx, conn, health, models)
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.retryEvery):
		}
	}
	return fmt.Errorf("vllm readiness on port %d failed after %s: %w", p.Port, p.Timeout, lastErr)
}

func (p *VLLM) check(ctx context.Context, conn connection.Connection, health, models string) error {
	if _, err := conn.Run(ctx, health); err != nil {
		return fmt.Errorf("health: %w", err)
	}
	if p.Model == "" {
		return nil
	}
	out, err := conn.Run(ctx, models)
	if err != nil {
		return fmt.Errorf("models: %w", err)
	}
	// vLLM returns {"data":[{"id":"<model>"}...]}. A substring check on the
	// configured id is sufficient and avoids a JSON dependency in the probe.
	if !strings.Contains(out, fmt.Sprintf(`"id":"%s"`, p.Model)) &&
		!strings.Contains(out, fmt.Sprintf(`"id": "%s"`, p.Model)) {
		return fmt.Errorf("model %q not yet served", p.Model)
	}
	return nil
}
