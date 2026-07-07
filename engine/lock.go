package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

// lockInfo is the advisory holder metadata written inside the lock dir. The
// mkdir is the lock; this file only improves error messages and lock status.
// A crash between mkdir and the metadata write leaves a held lock without it
// (reported as "no holder metadata") — accepted, the remedy (lock release)
// is the same.
type lockInfo struct {
	AcquiredAt string `json:"acquired_at"`
	Tag        string `json:"tag,omitempty"`
	By         string `json:"by"`
}

func lockDir(project string) string      { return projectDir(project) + "/lock" }
func lockInfoPath(project string) string { return lockDir(project) + "/info.json" }

// lockHolderDesc describes who holds the lock, from the advisory metadata:
// "held by <by> since <acquired_at> (deploying <tag>)". Locks without
// readable metadata (pre-metadata dirs, crash before the write) degrade to
// "no holder metadata".
func lockHolderDesc(ctx context.Context, conn connection.Connection, project string) string {
	out, err := conn.Run(ctx, fmt.Sprintf("cat %s", lockInfoPath(project)))
	if err != nil {
		return "no holder metadata"
	}
	var li lockInfo
	if json.Unmarshal([]byte(out), &li) != nil || li.By == "" {
		return "no holder metadata"
	}
	desc := fmt.Sprintf("held by %s since %s", li.By, li.AcquiredAt)
	if li.Tag != "" {
		desc += fmt.Sprintf(" (deploying %s)", li.Tag)
	}
	return desc
}

// acquireLock takes the per-project deploy lock (atomic mkdir on the target).
// tag is recorded in the advisory metadata; empty when not deploying a tag
// (rollback before history is read, manual lock acquire). The returned func
// releases the lock.
func acquireLock(ctx context.Context, conn connection.Connection, project, tag string) (func(), error) {
	dir := lockDir(project)
	mk := fmt.Sprintf("mkdir -p %s && mkdir %s", projectDir(project), dir)
	if _, err := conn.Run(ctx, mk); err != nil {
		return nil, fmt.Errorf("another deploy appears to be running: lock %s %s: %w",
			dir, lockHolderDesc(ctx, conn, project), err)
	}
	// Advisory metadata, best effort: transported base64-encoded like
	// writeSecretsFile so the value never hits shell quoting.
	li := lockInfo{AcquiredAt: time.Now().UTC().Format(time.RFC3339), Tag: tag, By: performer()}
	b, _ := json.Marshal(li)
	enc := base64.StdEncoding.EncodeToString(b)
	if _, err := conn.Run(ctx, fmt.Sprintf("printf %%s %s | base64 -d > %s", enc, lockInfoPath(project))); err != nil {
		// The mkdir is the lock; a failed metadata write must not fail the deploy.
		_ = err
	}
	release := func() {
		_, _ = conn.Run(context.Background(),
			fmt.Sprintf("rm -f %s && rmdir %s", lockInfoPath(project), lockDir(project)))
	}
	return release, nil
}

// lockTag summarizes the tags Deploy is about to roll out, for the lock
// metadata: the distinct service image tags, sorted, comma-joined (services
// usually share one tag, so this is normally just that tag).
func lockTag(cfg *config.Config) string {
	seen := map[string]bool{}
	var tags []string
	for _, s := range cfg.Services {
		if !seen[s.ImageTag] {
			seen[s.ImageTag] = true
			tags = append(tags, s.ImageTag)
		}
	}
	sort.Strings(tags)
	return strings.Join(tags, ",")
}

// lockPollInterval is how often acquireLockWait retries. Package var so wait
// tests can run in milliseconds.
var lockPollInterval = 5 * time.Second

// acquireLockWait acquires the deploy lock, retrying for up to wait when it
// is held. wait <= 0 fails fast (acquireLock's behavior). The first collision
// prints one waiting line to out; retries are silent. On timeout the last
// holder-aware collision error is returned. Ctx cancellation aborts the wait.
func acquireLockWait(ctx context.Context, conn connection.Connection, project, tag string, wait time.Duration, out io.Writer) (func(), error) {
	release, err := acquireLock(ctx, conn, project, tag)
	if err == nil || wait <= 0 {
		return release, err
	}
	fmt.Fprintf(out, "waiting for deploy lock (%s)\n", lockHolderDesc(ctx, conn, project))
	deadline := time.Now().Add(wait)
	for {
		sleep := lockPollInterval
		if remaining := time.Until(deadline); remaining < sleep {
			sleep = remaining
		}
		if sleep < 0 {
			sleep = 0
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		release, err = acquireLock(ctx, conn, project, tag)
		if err == nil {
			return release, nil
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
	}
}
