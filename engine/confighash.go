package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// shouldSkip reports whether Deploy can no-op: the LAST history record
// overall is a success whose recorded config hash matches the desired one.
// The anchor is deliberately not currentRecord (the rollback anchor), which
// skips trailing failures: if a deploy succeeds and a later forced redeploy
// fails, the service may be down while the last *successful* record still
// matches the hash — anchoring on the last record means any trailing
// failure disables the skip, so a plain deploy always recovers.
func (e *Engine) shouldSkip(h []Record, hash string) bool {
	if e.Force {
		return false
	}
	n := len(h)
	return n > 0 && h[n-1].success() && h[n-1].ConfigHash != "" && h[n-1].ConfigHash == hash
}

// desiredHash fingerprints the deploy's inputs: the compose file as it exists
// on the target (dockrail runs compose against that copy, so that is the copy
// that matters) plus every service's deploy.yml stanza (tag, readiness,
// cutover, placement) in sorted name order. Secret VALUES are deliberately
// excluded — they are collected after the skip decision and hashing them
// would record value-equality in history; changing only a secret requires
// deploy --force.
func (e *Engine) desiredHash(ctx context.Context) (string, error) {
	out, err := e.Conn.Run(ctx, fmt.Sprintf("sha256sum -- %s", e.Cfg.Compose))
	if err != nil {
		return "", fmt.Errorf("hash compose file: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("hash compose file: empty sha256sum output")
	}
	parts := []string{fields[0]}
	for _, n := range slices.Sorted(maps.Keys(e.Cfg.Services)) {
		svc := e.Cfg.Services[n]
		b, err := yaml.Marshal(svc)
		if err != nil {
			return "", err
		}
		parts = append(parts, n, string(b))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
