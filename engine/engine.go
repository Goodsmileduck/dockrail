package engine

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/secrets"
	"github.com/goodsmileduck/dockrail/strategy/readiness"
)

type Engine struct {
	Conn connection.Connection
	Cfg  *config.Config
	Out  io.Writer
	// LockWait is how long Deploy/Rollback wait for the deploy lock before
	// giving up. Zero = fail fast (the default).
	LockWait time.Duration
	// Force bypasses the no-op skip in Deploy: it redeploys even when the
	// config hash matches the last successful deploy.
	Force bool
	// configHash is the digest computed by Deploy for the record it is about
	// to write. Rollback/RollbackTo never set it, so their records carry no
	// hash and a subsequent Deploy never skips after a rollback.
	configHash string
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
	release, err := acquireLockWait(ctx, e.Conn, e.Cfg.Project, lockTag(e.Cfg), e.LockWait, e.Out)
	if err != nil {
		return err
	}
	defer release()

	e.logf("step preflight")
	if errs := Preflight(ctx, e.Conn, e.Cfg); len(errs) > 0 {
		return fmt.Errorf("preflight failed: %v", errs)
	}

	// Deploy depends on readable history: it is threaded into finalize for
	// retention, so an unreadable/corrupt history fails fast here (as it
	// already does in Rollback/RollbackTo) rather than silently skipping prune.
	h, err := loadHistory(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return err
	}

	hash, err := e.desiredHash(ctx)
	if err != nil {
		return err
	}
	if e.shouldSkip(h, hash) {
		e.logf("no changes since last deploy (config hash match); skipping — use --force to redeploy")
		return nil
	}
	e.configHash = hash

	return e.runServices(ctx, func(svc config.Service) string { return svc.ImageTag }, "", "deploy", "deployed", h)
}

// runServices is the shared deploy/rollback body: prepare secrets and registry
// once, cut over every service to the tag tagFor picks, and finalize a single
// success record. failTag is recorded on a secrets/registry failure (before any
// service tag is known); step labels a cutover failure; outcome is the success
// record's outcome. h is the pre-append history, threaded into finalize so it
// need not re-read the file. The caller holds the deploy lock.
func (e *Engine) runServices(ctx context.Context, tagFor func(config.Service) string, failTag, step, outcome string, h []Record) error {
	prov, err := secrets.New(e.Cfg.Secrets.Provider)
	if err != nil {
		return e.recordFailure(ctx, failTag, "secrets", err)
	}
	vals, err := prov.Fetch(ctx, e.Cfg.Secrets.FromEnv)
	if err != nil {
		return e.recordFailure(ctx, failTag, "secrets", err)
	}
	prefix, err := writeSecretsFile(ctx, e.Conn, e.Cfg.Project, vals)
	if err != nil {
		return e.recordFailure(ctx, failTag, "secrets", err)
	}
	if err := registryLogin(ctx, e.Conn, e.Cfg.Registry, e.Out); err != nil {
		return e.recordFailure(ctx, failTag, "registry", err)
	}

	var deployed string
	ids := map[string]string{}
	for name, svc := range e.Cfg.Services {
		tag := tagFor(svc)
		cid, err := e.cutover(ctx, name, svc, tag, prefix)
		if err != nil {
			return e.recordFailure(ctx, tag, step, fmt.Errorf("service %s: %w", name, err))
		}
		deployed = tag
		if cid != "" {
			ids[name] = cid
		}
	}
	return e.finalize(ctx, deployed, outcome, ids, h)
}

// Rollback restores the previously running image tag recorded in deploy
// history, re-running the cutover sequence for every service against that
// tag. It takes the deploy lock for its duration. Because history records a
// single project-level tag per attempt, all services are restored to the
// same previous tag.
func (e *Engine) Rollback(ctx context.Context) error {
	e.logf("step lock")
	release, err := acquireLockWait(ctx, e.Conn, e.Cfg.Project, "", e.LockWait, e.Out)
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
	return e.deployTag(ctx, target, "rolled-back", h)
}

// RollbackTo restores an explicit tag. The tag must appear as a successful
// record within the last retainWindow distinct deployed tags, and its image
// must still be present on the host (retention may have pruned it).
func (e *Engine) RollbackTo(ctx context.Context, tag string) error {
	if !safeTag.MatchString(tag) {
		return fmt.Errorf("unsafe image tag %q", tag)
	}
	e.logf("step lock")
	release, err := acquireLockWait(ctx, e.Conn, e.Cfg.Project, "", e.LockWait, e.Out)
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
	return e.deployTag(ctx, tag, "rolled-back", h)
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

// resolveImages returns the fully-qualified image references compose maps for
// the project at the given tag (e.g. registry.example.com/api:v1). The bare
// tag alone is not a valid image reference, so both presence-checking and
// retention resolve through compose.
func (e *Engine) resolveImages(ctx context.Context, tag string) ([]string, error) {
	out, err := e.Conn.Run(ctx, fmt.Sprintf("TAG=%s docker compose -f %s config --images", tag, e.Cfg.Compose))
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// imagePresent checks that every image compose maps at the given tag exists on
// the host. docker image inspect takes all refs in one call and exits non-zero
// if any is missing.
func (e *Engine) imagePresent(ctx context.Context, tag string) error {
	imgs, err := e.resolveImages(ctx, tag)
	if err != nil {
		return err
	}
	if len(imgs) == 0 {
		return nil
	}
	refs := strings.Join(imgs, " ")
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("docker image inspect --format '{{.Id}}' %s", refs)); err != nil {
		return fmt.Errorf("image(s) %s: %w", refs, err)
	}
	return nil
}

// deployTag restores every service to an explicit tag and appends one history
// record with the given success outcome. Shared by Rollback and RollbackTo; h
// is the pre-append history for retention.
func (e *Engine) deployTag(ctx context.Context, tag, outcome string, h []Record) error {
	e.logf("step rollback: restoring tag %s", tag)
	return e.runServices(ctx, func(config.Service) string { return tag }, tag, "rollback", outcome, h)
}

// recreate runs the fixed pull→stop→up→probe sequence for one service at an
// explicit image tag. It performs no history I/O; the caller owns the single
// finalize/recordFailure write. Deploy uses the service's configured tag;
// Rollback uses the recorded previous tag.
func (e *Engine) recreate(ctx context.Context, name string, svc config.Service, tag string, prefix string) (string, error) {
	if !safeTag.MatchString(tag) {
		return "", fmt.Errorf("unsafe image tag %q", tag)
	}
	prober, err := readiness.New(svc.Readiness, svc.Model)
	if err != nil {
		return "", err
	}
	compose := fmt.Sprintf("%sTAG=%s docker compose -f %s", prefix, tag, e.Cfg.Compose)

	e.logf("step pull: %s tag %s", name, tag)
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("%s pull %s", compose, name)); err != nil {
		return "", fmt.Errorf("pull: %w", err)
	}

	e.logf("step recreate: stop old + start new")
	e.captureLogs(ctx, name, name)
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("%s stop %s", compose, name)); err != nil {
		return "", fmt.Errorf("stop old: %w", err)
	}
	if _, err := e.Conn.Run(ctx, fmt.Sprintf("%s up -d --no-deps %s", compose, name)); err != nil {
		return "", fmt.Errorf("start new: %w", err)
	}

	e.logf("step readiness")
	if err := prober.Probe(ctx, e.Conn, "localhost"); err != nil {
		return "", err
	}
	e.logf("deployed %s tag %s", name, tag)
	return e.slotID(ctx, name), nil
}

// cutover deploys one service at the given tag via its configured strategy and
// returns the id of the container it made live (best-effort; "" if unresolved).
func (e *Engine) cutover(ctx context.Context, name string, svc config.Service, tag string, prefix string) (string, error) {
	switch svc.Cutover.Strategy {
	case "recreate":
		return e.recreate(ctx, name, svc, tag, prefix)
	case "proxy":
		return e.proxyCutover(ctx, name, svc, tag, prefix)
	default:
		return "", fmt.Errorf("cutover strategy %q not implemented yet", svc.Cutover.Strategy)
	}
}

// finalize appends the success record and prunes dangling images. The prune
// is best-effort and never fails the operation. h is the pre-append history;
// retention runs against it plus the record just written, avoiding a re-read
// of the history file from the host.
func (e *Engine) finalize(ctx context.Context, tag, outcome string, ids map[string]string, h []Record) error {
	e.logf("step finalize")
	// TS/Performer are left blank here and filled by appendRecord on the host
	// copy only; retention (retainedTags/success) reads Tag and Outcome only,
	// so the in-memory rec below is sufficient. Any future time-based retention
	// must not trust TS on this local copy.
	rec := Record{Tag: tag, Outcome: outcome, Services: ids, ConfigHash: e.configHash}
	if err := appendRecord(ctx, e.Conn, e.Cfg.Project, rec); err != nil {
		return err
	}
	if _, err := e.Conn.Run(ctx, "docker image prune -f"); err != nil {
		e.logf("warn: prune failed: %v", err)
	}
	e.prune(ctx, append(h, rec))
	return nil
}

// recordFailure appends a failed@<step> record (best-effort) and returns the
// caller's error. Nothing else is written, so the rollback anchor — the last
// successful record — is untouched.
func (e *Engine) recordFailure(ctx context.Context, tag, step string, retErr error) error {
	_ = appendRecord(ctx, e.Conn, e.Cfg.Project, Record{Tag: tag, Outcome: "failed@" + step})
	return retErr
}

// composePS returns the trimmed container id of a compose service (empty if it
// is not up). The single place the `docker compose ps -q <svc>` invocation is
// built, so slotID, runningImage, and activeColor stay in sync.
func (e *Engine) composePS(ctx context.Context, composeName string) (string, error) {
	out, err := e.Conn.Run(ctx, fmt.Sprintf("docker compose -f %s ps -q %s", e.Cfg.Compose, composeName))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// slotID returns the running container id for a compose service, or "" if it
// is not up or cannot be resolved. Best-effort: used only to record ids in
// history, so errors are swallowed rather than failing the deploy.
func (e *Engine) slotID(ctx context.Context, composeName string) string {
	cid, err := e.composePS(ctx, composeName)
	if err != nil {
		return ""
	}
	return cid
}

// runningImage resolves the running container id and its image reference for a
// compose service. A blank id (no error) means the service is not up. Used by
// status reporting and by log capture before a slot is recreated.
func (e *Engine) runningImage(ctx context.Context, composeName string) (cid, image string, err error) {
	cid, err = e.composePS(ctx, composeName)
	if err != nil {
		return "", "", err
	}
	if cid == "" {
		return "", "", nil
	}
	img, err := e.inspectField(ctx, cid, "{{.Config.Image}}")
	if err != nil {
		return cid, "", err
	}
	return cid, img, nil
}

// inspectField runs `docker inspect --format <format>` on a container id and
// returns the trimmed output. The single place a docker inspect is issued, so
// runningImage and containerIP stay consistent.
func (e *Engine) inspectField(ctx context.Context, cid, format string) (string, error) {
	out, err := e.Conn.Run(ctx, fmt.Sprintf("docker inspect --format '%s' %s", format, cid))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// containerIP returns the first IPv4 across the container's networks. Single-
// network is the norm for proxy-path color services; multi-network resolves to
// the first address (documented limitation).
func (e *Engine) containerIP(ctx context.Context, cid string) (string, error) {
	out, err := e.inspectField(ctx, cid, "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}")
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", cid, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("no IP for container %s", cid)
	}
	return fields[0], nil
}
