package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type Drift struct {
	Service, Image, Local, Remote, Note string
}

// ImageDrift compares, per service, the digest of the image on the target
// host against what the tag currently resolves to in the registry. Both
// lookups run on the target (agentless; the host's docker creds apply).
// Advisory only — dockrail never redeploys on drift (D10).
func ImageDrift(ctx context.Context, conn connection.Connection, cfg *config.Config) []Drift {
	var out []Drift
	for _, name := range slices.Sorted(maps.Keys(cfg.Services)) {
		img := cfg.Services[name].ImageTag
		if strings.Contains(img, "@sha256:") || !safeTag.MatchString(img) {
			continue // pinned = immutable; unsafe = already rejected by config validation
		}
		local, err := conn.Run(ctx, fmt.Sprintf("docker image inspect --format '{{join .RepoDigests \"\\n\"}}' %s", img))
		if err != nil {
			out = append(out, Drift{Service: name, Image: img, Note: "image not present on host"})
			continue
		}
		remote, err := conn.Run(ctx, fmt.Sprintf("docker buildx imagetools inspect --format '{{.Manifest.Digest}}' %s", img))
		if err != nil {
			out = append(out, Drift{Service: name, Image: img, Local: digestOf(local), Note: fmt.Sprintf("remote lookup failed: %v", err)})
			continue
		}
		remoteDigest := strings.TrimSpace(remote)
		if matchesDigest(local, remoteDigest) {
			continue
		}
		out = append(out, Drift{Service: name, Image: img, Local: digestOf(local), Remote: remoteDigest, Note: "drift"})
	}
	return out
}

// digestOf returns the first digest part of a RepoDigests listing.
func digestOf(repoDigests string) string {
	for _, line := range strings.Split(strings.TrimSpace(repoDigests), "\n") {
		if _, d, ok := strings.Cut(line, "@"); ok {
			return d
		}
	}
	return ""
}

// matchesDigest reports whether any local RepoDigest line ends in the remote
// digest (local lines are "name@sha256:...").
func matchesDigest(repoDigests, remote string) bool {
	if remote == "" {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(repoDigests), "\n") {
		if _, d, ok := strings.Cut(line, "@"); ok && d == remote {
			return true
		}
	}
	return false
}
