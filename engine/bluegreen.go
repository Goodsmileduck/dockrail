package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/strategy/placement"
	"github.com/goodsmileduck/dockrail/strategy/readiness"
)

func otherColor(c string) string {
	if c == "blue" {
		return "green"
	}
	return "blue"
}

// activeColor returns the currently-running color for a service, or "" if
// neither blue nor green is up (first deploy).
func (e *Engine) activeColor(ctx context.Context, service string) (string, error) {
	for _, c := range []string{"blue", "green"} {
		out, err := e.Conn.Run(ctx, fmt.Sprintf("docker compose -f %s ps -q %s-%s",
			e.Cfg.Compose, service, c))
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(out) != "" {
			return c, nil
		}
	}
	return "", nil
}

func (e *Engine) composeCmd(prefix, tag, gpu, action, svcColor string) string {
	env := fmt.Sprintf("%sTAG=%s ", prefix, tag)
	if gpu != "" {
		env += fmt.Sprintf("DOCKRAIL_GPU=%s ", gpu)
	}
	return fmt.Sprintf("%sdocker compose -f %s %s %s", env, e.Cfg.Compose, action, svcColor)
}

// proxyCutover performs a blue-green cutover for one service. With a free GPU
// slot it is zero-gap (green up alongside blue, flip, stop blue); otherwise it
// follows on_no_free_gpu (fail = abort; stop-old-first = stop blue, start
// green, flip, with auto-rollback to blue on readiness failure).
func (e *Engine) proxyCutover(ctx context.Context, name string, svc config.Service, tag string, prefix string) error {
	if !safeTag.MatchString(tag) {
		return fmt.Errorf("unsafe image tag %q", tag)
	}
	prober, err := readiness.New(svc.Readiness, svc.Model)
	if err != nil {
		return err
	}
	active, err := e.activeColor(ctx, name)
	if err != nil {
		return err
	}
	target := "blue"
	if active != "" {
		target = otherColor(active)
	} else {
		active = otherColor(target)
	}
	blueSvc := name + "-" + active
	greenSvc := name + "-" + target

	// GPU capacity decision (only for gpu placement).
	gpu := ""
	sequenced := false
	if svc.Placement.Type == "gpu" {
		placer, err := placement.New(svc.Placement)
		if err != nil {
			return err
		}
		idx, perr := placer.Pick(ctx, e.Conn)
		switch {
		case perr == nil:
			gpu = idx // free slot -> zero-gap
		case errors.Is(perr, placement.ErrNoFreeGPU):
			if svc.Placement.OnNoFreeGPU == "fail" {
				return fmt.Errorf("%s: no free GPU and on_no_free_gpu=fail", name)
			}
			sequenced = true // stop-old-first
		default:
			return perr
		}
	}

	e.logf("step pull: %s tag %s (%s)", name, tag, target)
	if _, err := e.Conn.Run(ctx, e.composeCmd(prefix, tag, gpu, "pull", greenSvc)); err != nil {
		return fmt.Errorf("pull green: %w", err)
	}

	if sequenced {
		// Free VRAM first: stop blue, then bring up green.
		e.logf("step stop-old-first: freeing VRAM by stopping %s", blueSvc)
		if _, err := e.Conn.Run(ctx, e.composeCmd(prefix, tag, "", "stop", blueSvc)); err != nil {
			return fmt.Errorf("stop blue: %w", err)
		}
		if _, err := e.Conn.Run(ctx, e.composeCmd(prefix, tag, gpu, "up -d --no-deps", greenSvc)); err != nil {
			return fmt.Errorf("start green: %w", err)
		}
		if err := prober.Probe(ctx, e.Conn); err != nil {
			// Auto-rollback: green never became ready and blue is down.
			e.logf("green failed readiness; auto-rolling back to %s", blueSvc)
			_, _ = e.Conn.Run(ctx, e.composeCmd(prefix, tag, "", "up -d --no-deps", blueSvc))
			return fmt.Errorf("green readiness failed, rolled back to blue: %w", err)
		}
		return flipUpstream(ctx, e.Conn, e.Cfg.Project, svc.Cutover.Proxy, name, target, svc.Readiness.Port)
	}

	// Zero-gap: green up alongside blue, gate, flip, then stop blue.
	e.logf("step blue-green: starting %s alongside %s", greenSvc, blueSvc)
	if _, err := e.Conn.Run(ctx, e.composeCmd(prefix, tag, gpu, "up -d --no-deps", greenSvc)); err != nil {
		return fmt.Errorf("start green: %w", err)
	}
	if err := prober.Probe(ctx, e.Conn); err != nil {
		// Green failed but blue still serves; tear down green, leave blue.
		_, _ = e.Conn.Run(ctx, e.composeCmd(prefix, tag, "", "stop", greenSvc))
		return fmt.Errorf("green readiness failed (blue still serving): %w", err)
	}
	if err := flipUpstream(ctx, e.Conn, e.Cfg.Project, svc.Cutover.Proxy, name, target, svc.Readiness.Port); err != nil {
		return err
	}
	e.logf("flip complete; stopping old %s", blueSvc)
	if _, err := e.Conn.Run(ctx, e.composeCmd(prefix, tag, "", "stop", blueSvc)); err != nil {
		return fmt.Errorf("stop blue after flip: %w", err)
	}
	return nil
}
