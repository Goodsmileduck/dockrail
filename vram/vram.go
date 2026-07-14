// Package vram holds VRAM-size parsing and the shared GPU headroom factor,
// used by both the deploy-time placement strategy and the fleet scheduler.
package vram

import (
	"fmt"
	"strconv"
	"strings"
)

// SafetyFactor reserves headroom over a model's stated VRAM need for KV-cache
// growth under load (20%). Multiply a parsed vram_min by this before comparing
// against free VRAM.
const SafetyFactor = 1.2

// ParseMiB converts a VRAM size string to integer mebibytes. Accepts GiB/Gi,
// MiB/Mi, and a bare number (treated as MiB, matching nvidia-smi's unit).
func ParseMiB(s string) (int, error) {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	mult := 1
	num := lower
	switch {
	case strings.HasSuffix(lower, "gib"):
		num, mult = strings.TrimSuffix(lower, "gib"), 1024
	case strings.HasSuffix(lower, "gi"):
		num, mult = strings.TrimSuffix(lower, "gi"), 1024
	case strings.HasSuffix(lower, "mib"):
		num, mult = strings.TrimSuffix(lower, "mib"), 1
	case strings.HasSuffix(lower, "mi"):
		num, mult = strings.TrimSuffix(lower, "mi"), 1
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid vram size %q: %w", s, err)
	}
	return int(v*float64(mult) + 0.5), nil // round, not truncate
}
