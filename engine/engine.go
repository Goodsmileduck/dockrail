package engine

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

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
// (e.g. a corrupt or tampered history line feeding Rollback) is rejected.
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

	secrets, err := collectSecrets(e.Cfg.Secrets.FromEnv)
	if err != nil {
		return e.recordFailure(ctx, "", "secrets", err)
	}
	prefix, err := writeSecretsFile(ctx, e.Conn, e.Cfg.Project, secrets)
	if err != nil {
		return e.recordFailure(ctx, "", "secrets", err)
	}
	if err := registryLogin(ctx, e.Conn, e.Cfg.Registry, e.Out); err != nil {
		return e.recordFailure(ctx, "", "registry", err)
	}

	var deployed string
	ids := map[string]string{}
	for name, svc := range e.Cfg.Services {
		if err := e.cutover(ctx, name, svc, svc.ImageTag, prefix); err != nil {
			return e.recordFailure(ctx, svc.ImageTag, "deploy",
				fmt.Errorf("service %s: %w", name, err))
		}
		deployed = svc.ImageTag
		if cid, err := e.containerID(ctx, name); err == nil && cid != "" {
			ids[name] = cid
		}
	}
	return e.finalize(ctx, deployed, "deployed", ids)
}

// Rollback restores the previously running image tag recorded in deploy
// history, re-running the cutover sequence for every service against that
// tag. It takes the deploy lock for its duration. Because history records a
// single project-level tag per attempt, all services are restored to the
// same previous tag.
func (e *Engine) Rollback(ctx context.Context) error {
	e.logf("step lock")
	release, err := acquireLock(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	defer release()

	h, err := loadHistory(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	target := previousTag(h)
	if target == "" {
		return fmt.Errorf("no previous tag in history for project %q; nothing to roll back to", e.Cfg.Project)
	}
	return e.deployTag(ctx, target, "rolled-back")
}

// RollbackTo restores an explicit tag. The tag must appear as a successful
// record within the last retainWindow distinct deployed tags, and its image
// must still be present on the host (retention may have pruned it).
func (e *Engine) RollbackTo(ctx context.Context, tag string) error {
	if !safeTag.MatchString(tag) {
		return fmt.Errorf("unsafe image tag %q", tag)
	}
	e.logf("step lock")
	release, err := acquireLock(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	defer release()

	h, err := loadHistory(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}
	window := retainedTags(h, retainWindow(e.Cfg))
	found := false
	for _, t := range window {
		if t == tag {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("tag %q is not in the retained history window; candidates: %s",
			tag, strings.Join(window, ", "))
	}
	if err := e.imagePresent(ctx, tag); err != nil {
		return fmt.Errorf("tag %q is in history but its image is gone from the host (pruned?): %w", tag, err)
	}
	return e.deployTag(ctx, tag, "rolled-back")
}

// retainedTags returns the last n distinct successfully-deployed tags,
// newest first.
func retainedTags(h []Record, n int) []string {
	var tags []string
	seen := map[string]bool{}
	for i := len(h) - 1; i >= 0 && len(tags) < n; i-- {
		if h[i].success() && !seen[h[i].Tag] {
			seen[h[i].Tag] = true
			tags = append(tags, h[i].Tag)
		}
	}
	return tags
}

// retainWindow reports how many distinct recent tags are eligible for
// explicit rollback and retention.
func retainWindow(cfg *config.Config) int { return cfg.RetainContainers }

// imagePresent checks every service's image at the given tag exists on the
// host, resolving repo names through compose itself.
func (e *Engine) imagePresent(ctx context.Context, tag string) error {
	out, err := e.Conn.Run(ctx, fmt.Sprintf("TAG=%s docker compose -f %s config --images", tag, e.Cfg.Compose))
	if err != nil {
		return err
	}
	for _, img := range strings.Fields(out) {
		if _, err := e.Conn.Run(ctx, fmt.Sprintf("docker image inspect --format '{{.Id}}' %s", img)); err != nil {
			return fmt.Errorf("image %s: %w", img, err)
		}
	}
	return nil
}

// deployTag runs secrets+registry+cutover for every service at an explicit
// tag and appends one history record with the given success outcome. Shared
// by Rollback and (Task 4) RollbackTo.
func (e *Engine) deployTag(ctx context.Context, tag, outcome string) error {
	e.logf("step rollback: restoring tag %s", tag)
	secrets, err := collectSecrets(e.Cfg.Secrets.FromEnv)
	if err != nil {
		return e.recordFailure(ctx, tag, "secrets", err)
	}
	prefix, err := writeSecretsFile(ctx, e.Conn, e.Cfg.Project, secrets)
	if err != nil {
		return e.recordFailure(ctx, tag, "secrets", err)
	}
	if err := registryLogin(ctx, e.Conn, e.Cfg.Registry, e.Out); err != nil {
		return e.recordFailure(ctx, tag, "registry", err)
	}
	ids := map[string]string{}
	for name, svc := range e.Cfg.Services {
		if err := e.cutover(ctx, name, svc, tag, prefix); err != nil {
			return e.recordFailure(ctx, tag, "rollback",
				fmt.Errorf("service %s: %w", name, err))
		}
		if cid, err := e.containerID(ctx, name); err == nil && cid != "" {
			ids[name] = cid
		}
	}
	return e.finalize(ctx, tag, outcome, ids)
}

// recreate runs the fixed pull→stop→up→probe sequence for one service at an
// explicit image tag. It performs no history I/O; the caller owns the single
// finalize/recordFailure write. Deploy uses the service's configured tag;
// Rollback uses the recorded previous tag.
func (e *Engine) recreate(ctx context.Context, name string, svc config.Service, tag string, prefix string) error {
	if !safeTag.MatchString(tag) {
		return fmt.Errorf("unsafe image tag %q", tag)
	}
	prober, err := readiness.New(svc.Readiness, svc.Model)
	if err != nil {
		return err
	}
	compose := fmt.Sprintf("%sTAG=%s docker compose -f %s", prefix, tag, e.Cfg.Compose)

	e.logf("step pull: %s tag %s", name, tag)
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("%s pull %s", compose, name)); err != nil {
		return fmt.Errorf("pull: %w", err)
	}

	e.logf("step recreate: stop old + start new")
	e.captureLogs(ctx, name, name)
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

func (e *Engine) cutover(ctx context.Context, name string, svc config.Service, tag string, prefix string) error {
	switch svc.Cutover.Strategy {
	case "recreate":
		return e.recreate(ctx, name, svc, tag, prefix)
	case "proxy":
		return e.proxyCutover(ctx, name, svc, tag, prefix)
	default:
		return fmt.Errorf("cutover strategy %q not implemented yet", svc.Cutover.Strategy)
	}
}

// finalize appends the success record and prunes dangling images. The prune
// is best-effort and never fails the operation.
func (e *Engine) finalize(ctx context.Context, tag, outcome string, ids map[string]string) error {
	e.logf("step finalize")
	if err := appendRecord(ctx, e.Conn, e.Cfg.Project, Record{Tag: tag, Outcome: outcome, Services: ids}); err != nil {
		return err
	}
	if _, err := e.Conn.Run(ctx, "docker image prune -f"); err != nil {
		e.logf("warn: prune failed: %v", err)
	}
	if h, err := loadHistory(ctx, e.Conn, e.Cfg.Project); err == nil {
		e.prune(ctx, h)
	} else {
		e.logf("warn: prune skipped: %v", err)
	}
	return nil
}

// recordFailure appends a failed@<step> record (best-effort) and returns the
// caller's error. Nothing else is written, so the rollback anchor — the last
// successful record — is untouched.
func (e *Engine) recordFailure(ctx context.Context, tag, step string, retErr error) error {
	_ = appendRecord(ctx, e.Conn, e.Cfg.Project, Record{Tag: tag, Outcome: "failed@" + step})
	return retErr
}

// containerID reports the running container for a service, checking the
// plain compose name first and then the blue/green slot names.
func (e *Engine) containerID(ctx context.Context, name string) (string, error) {
	for _, n := range []string{name, name + "-blue", name + "-green"} {
		out, err := e.Conn.Run(ctx, fmt.Sprintf("docker compose -f %s ps -q %s", e.Cfg.Compose, n))
		if err != nil {
			return "", err
		}
		if s := strings.TrimSpace(out); s != "" {
			return s, nil
		}
	}
	return "", nil
}
