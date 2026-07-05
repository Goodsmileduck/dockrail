package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/strategy/readiness"
)

type Engine struct {
	Conn connection.Connection
	Cfg  *config.Config
	Out  io.Writer
}

func (e *Engine) logf(format string, a ...any) {
	fmt.Fprintf(e.Out, format+"\n", a...)
}

func (e *Engine) Deploy(ctx context.Context) error {
	e.logf("step lock")
	release, err := acquireLock(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	defer release()

	e.logf("step preflight")
	if errs := Preflight(ctx, e.Conn, e.Cfg); len(errs) > 0 {
		return fmt.Errorf("preflight failed: %v", errs)
	}
	for name, svc := range e.Cfg.Services {
		if err := e.deployService(ctx, name, svc); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
	}
	return nil
}

func (e *Engine) deployService(ctx context.Context, name string, svc config.Service) error {
	if svc.Cutover.Strategy != "recreate" {
		return fmt.Errorf("cutover strategy %q not implemented yet", svc.Cutover.Strategy)
	}
	prober, err := readiness.New(svc.Readiness)
	if err != nil {
		return err
	}
	compose := fmt.Sprintf("TAG=%s docker compose -f %s", svc.ImageTag, e.Cfg.Compose)

	e.logf("step pull: %s tag %s", name, svc.ImageTag)
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("%s pull %s", compose, name)); err != nil {
		return fmt.Errorf("pull: %w", err)
	}

	e.logf("step record-anchor")
	st, err := loadState(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}

	e.logf("step recreate: stop old + start new")
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("%s stop %s", compose, name)); err != nil {
		return fmt.Errorf("stop old: %w", err)
	}
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("%s up -d --no-deps %s", compose, name)); err != nil {
		return fmt.Errorf("start new: %w", err)
	}

	e.logf("step readiness")
	if err := prober.Probe(ctx, e.Conn); err != nil {
		st.LastFailure = fmt.Sprintf("deploy %s tag %s: %v", name, svc.ImageTag, err)
		_ = saveState(ctx, e.Conn, e.Cfg.Project, st)
		return err
	}

	e.logf("step finalize")
	st.PreviousTag, st.CurrentTag, st.LastFailure = st.CurrentTag, svc.ImageTag, ""
	if err := saveState(ctx, e.Conn, e.Cfg.Project, st); err != nil {
		return err
	}
	if _, err := e.Conn.Run(ctx, "docker image prune -f"); err != nil {
		e.logf("warn: prune failed: %v", err)
	}
	e.logf("deployed %s tag %s", name, svc.ImageTag)
	return nil
}
