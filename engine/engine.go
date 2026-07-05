package engine

import (
	"context"
	"fmt"
	"io"
	"regexp"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/strategy/readiness"
)

type Engine struct {
	Conn connection.Connection
	Cfg  *config.Config
	Out  io.Writer
}

// safeTag guards image tags before they are interpolated into a host shell
// command. Valid image references only use this character set; anything else
// (e.g. a corrupt or tampered state file feeding Rollback) is rejected.
var safeTag = regexp.MustCompile(`^[A-Za-z0-9_.:/@-]+$`)

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

	// Read the rollback anchor once for the whole operation. The host state
	// file holds a single project-level tag pair, so it must be written
	// exactly once (in finalize) — writing per service would let a later
	// service read and overwrite the anchor an earlier one just recorded.
	e.logf("step record-anchor")
	st, err := loadState(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	anchor := st.CurrentTag

	secrets, err := collectSecrets(e.Cfg.Secrets.FromEnv)
	if err != nil {
		return e.recordFailure(ctx, st, fmt.Sprintf("secrets: %v", err), err)
	}
	prefix, err := writeSecretsFile(ctx, e.Conn, e.Cfg.Project, secrets)
	if err != nil {
		return e.recordFailure(ctx, st, fmt.Sprintf("secrets: %v", err), err)
	}
	if err := registryLogin(ctx, e.Conn, e.Cfg.Registry, e.Out); err != nil {
		return e.recordFailure(ctx, st, fmt.Sprintf("registry login: %v", err), err)
	}

	var deployed string
	for name, svc := range e.Cfg.Services {
		if err := e.recreate(ctx, name, svc, svc.ImageTag, prefix); err != nil {
			return e.recordFailure(ctx, st, fmt.Sprintf("deploy %s tag %s: %v", name, svc.ImageTag, err),
				fmt.Errorf("service %s: %w", name, err))
		}
		deployed = svc.ImageTag
	}
	return e.finalize(ctx, st, anchor, deployed)
}

// Rollback restores the previously running image tag recorded in host state,
// re-running the recreate sequence for every service against that tag. It
// takes the deploy lock for its duration. Because host state records a single
// project-level tag pair, all services are restored to the same previous tag.
func (e *Engine) Rollback(ctx context.Context) error {
	e.logf("step lock")
	release, err := acquireLock(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	defer release()

	st, err := loadState(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	if st.PreviousTag == "" {
		return fmt.Errorf("no previous tag recorded for project %q; nothing to roll back to", e.Cfg.Project)
	}
	target := st.PreviousTag
	anchor := st.CurrentTag
	e.logf("step rollback: restoring tag %s", target)
	secrets, err := collectSecrets(e.Cfg.Secrets.FromEnv)
	if err != nil {
		return e.recordFailure(ctx, st, fmt.Sprintf("secrets: %v", err), err)
	}
	prefix, err := writeSecretsFile(ctx, e.Conn, e.Cfg.Project, secrets)
	if err != nil {
		return e.recordFailure(ctx, st, fmt.Sprintf("secrets: %v", err), err)
	}
	if err := registryLogin(ctx, e.Conn, e.Cfg.Registry, e.Out); err != nil {
		return e.recordFailure(ctx, st, fmt.Sprintf("registry login: %v", err), err)
	}
	for name, svc := range e.Cfg.Services {
		if err := e.recreate(ctx, name, svc, target, prefix); err != nil {
			return e.recordFailure(ctx, st, fmt.Sprintf("rollback %s tag %s: %v", name, target, err),
				fmt.Errorf("service %s: %w", name, err))
		}
	}
	// After a rollback the roles swap: what was current becomes the new
	// previous, and the restored target becomes current.
	return e.finalize(ctx, st, anchor, target)
}

// recreate runs the fixed pull→stop→up→probe sequence for one service at an
// explicit image tag. It performs no host-state I/O; the caller owns the
// anchor read and the single finalize write. Deploy uses the service's
// configured tag; Rollback uses the recorded previous tag.
func (e *Engine) recreate(ctx context.Context, name string, svc config.Service, tag string, prefix string) error {
	if svc.Cutover.Strategy != "recreate" {
		return fmt.Errorf("cutover strategy %q not implemented yet", svc.Cutover.Strategy)
	}
	if !safeTag.MatchString(tag) {
		return fmt.Errorf("unsafe image tag %q", tag)
	}
	prober, err := readiness.New(svc.Readiness)
	if err != nil {
		return err
	}
	compose := fmt.Sprintf("%sTAG=%s docker compose -f %s", prefix, tag, e.Cfg.Compose)

	e.logf("step pull: %s tag %s", name, tag)
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("%s pull %s", compose, name)); err != nil {
		return fmt.Errorf("pull: %w", err)
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
		return err
	}
	e.logf("deployed %s tag %s", name, tag)
	return nil
}

// finalize persists the swapped tag pair once and prunes dangling images. The
// prune is best-effort and never fails the operation.
func (e *Engine) finalize(ctx context.Context, st State, previous, current string) error {
	e.logf("step finalize")
	st.PreviousTag, st.CurrentTag, st.LastFailure = previous, current, ""
	if err := saveState(ctx, e.Conn, e.Cfg.Project, st); err != nil {
		return err
	}
	if _, err := e.Conn.Run(ctx, "docker image prune -f"); err != nil {
		e.logf("warn: prune failed: %v", err)
	}
	return nil
}

// recordFailure persists the failure note to host state (best-effort) and
// returns the caller's error. State is not otherwise mutated, so the rollback
// anchor is preserved for a later retry or rollback.
func (e *Engine) recordFailure(ctx context.Context, st State, note string, retErr error) error {
	st.LastFailure = note
	_ = saveState(ctx, e.Conn, e.Cfg.Project, st)
	return retErr
}
