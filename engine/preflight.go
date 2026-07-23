package engine

import (
	"context"
	"encoding/json"
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
	errs = append(errs, proxyPortCollision(ctx, conn, cfg)...)
	return errs
}

// proxyPortCollision fails fast when a proxy-cutover service's two color
// containers publish the same host port — a guaranteed start-time bind
// collision (issue #13). Best-effort: if compose config cannot be rendered or
// parsed, it stays silent rather than blocking the deploy.
func proxyPortCollision(ctx context.Context, conn connection.Connection, cfg *config.Config) []error {
	anyProxy := false
	for _, s := range cfg.Services {
		if s.Cutover.Strategy == "proxy" {
			anyProxy = true
			break
		}
	}
	if !anyProxy {
		return nil
	}
	out, err := conn.Run(ctx, fmt.Sprintf("docker compose -f %s config --format json", cfg.Compose))
	if err != nil {
		return nil
	}
	var parsed struct {
		Services map[string]struct {
			Ports []struct {
				Published string `json:"published"`
			} `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil
	}
	published := func(svc string) map[string]bool {
		set := map[string]bool{}
		for _, p := range parsed.Services[svc].Ports {
			if p.Published != "" {
				set[p.Published] = true
			}
		}
		return set
	}
	var errs []error
	for name, s := range cfg.Services {
		if s.Cutover.Strategy != "proxy" {
			continue
		}
		green := published(name + "-green")
		for port := range published(name + "-blue") {
			if green[port] {
				errs = append(errs, fmt.Errorf(
					"%s-blue and %s-green both publish host port %s; proxy cutover reaches colors by container name, not a shared host port — remove the `ports:` mapping for host port %s from the color services",
					name, name, port, port))
			}
		}
	}
	return errs
}
