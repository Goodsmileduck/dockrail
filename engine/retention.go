package engine

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// captureLogs saves a tail of the slot container's docker logs to the host
// state dir before compose recreates it (recreation deletes the old
// container's logs). Best-effort: failures are logged, never fatal.
// svcName is the logical service; composeName the compose service whose
// container is about to be replaced (e.g. "api" or "api-blue").
func (e *Engine) captureLogs(ctx context.Context, svcName, composeName string) {
	cid, err := e.Conn.Run(ctx, fmt.Sprintf("docker compose -f %s ps -q %s", e.Cfg.Compose, composeName))
	if err != nil || strings.TrimSpace(cid) == "" {
		return
	}
	cid = strings.TrimSpace(cid)
	img, err := e.Conn.Run(ctx, fmt.Sprintf("docker inspect --format '{{.Config.Image}}' %s", cid))
	if err != nil {
		return
	}
	tag := "unknown"
	if i := strings.LastIndex(strings.TrimSpace(img), ":"); i >= 0 {
		tag = strings.TrimSpace(img)[i+1:]
	}
	if !safeTag.MatchString(tag) {
		tag = "unknown"
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	dir := fmt.Sprintf("$HOME/.dockrail/%s/logs", e.Cfg.Project)
	dst := fmt.Sprintf("%s/%s-%s-%s.log", dir, svcName, tag, ts)
	if _, err := e.Conn.Run(ctx, fmt.Sprintf(
		"mkdir -p %s && docker logs --tail 1000 %s > %s 2>&1 || true", dir, cid, dst)); err != nil {
		e.logf("warn: log capture for %s failed: %v", svcName, err)
	}
}

// prune removes images and saved log files whose tags fall outside the last
// retain_containers distinct successfully-deployed tags. Failed tags are
// never victims (forensics exemption); only unrelated-to-window successful
// tags are removed, and only for images compose itself maps to this project.
// Best-effort throughout.
func (e *Engine) prune(ctx context.Context, h []Record) {
	keep := map[string]bool{}
	for _, t := range retainedTags(h, retainWindow(e.Cfg)) {
		keep[t] = true
	}
	seen := map[string]bool{}
	for _, r := range h {
		if !r.success() || keep[r.Tag] || seen[r.Tag] || !safeTag.MatchString(r.Tag) {
			continue
		}
		seen[r.Tag] = true
		imgs, err := e.Conn.Run(ctx, fmt.Sprintf("TAG=%s docker compose -f %s config --images", r.Tag, e.Cfg.Compose))
		if err != nil {
			e.logf("warn: prune: resolve images for %s: %v", r.Tag, err)
			continue
		}
		for _, img := range strings.Fields(imgs) {
			// config --images echoes the interpolated tag back; only remove
			// refs that actually end in the victim tag.
			if !strings.HasSuffix(img, ":"+r.Tag) {
				continue
			}
			if _, err := e.Conn.Run(ctx, fmt.Sprintf("docker image rm %s || true", img)); err != nil {
				e.logf("warn: prune image %s: %v", img, err)
			}
		}
		if _, err := e.Conn.Run(ctx, fmt.Sprintf(
			"rm -f $HOME/.dockrail/%s/logs/*-%s-*.log", e.Cfg.Project, r.Tag)); err != nil {
			e.logf("warn: prune logs for %s: %v", r.Tag, err)
		}
	}
}
