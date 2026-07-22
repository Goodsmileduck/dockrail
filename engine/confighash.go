package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

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
	names := make([]string, 0, len(e.Cfg.Services))
	for n := range e.Cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
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
