package placement

import (
	"fmt"
	"strconv"
	"strings"
)

// parseMiB converts a VRAM size string to integer mebibytes. Accepts GiB/Gi,
// MiB/Mi, and a bare number (treated as MiB, matching nvidia-smi's unit).
func parseMiB(s string) (int, error) {
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
