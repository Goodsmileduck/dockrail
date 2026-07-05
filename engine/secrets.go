package engine

import (
	"fmt"
	"os"
)

// collectSecrets reads each named variable from dockrail's own environment
// (the invoking shell / CI job). the dogfood project keeps these in ~/.bashrc/.env; a
// non-interactive SSH shell on the target would not have them, so dockrail
// forwards them explicitly. An unset or empty required secret is fatal.
func collectSecrets(names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, n := range names {
		v, ok := os.LookupEnv(n)
		if !ok || v == "" {
			return nil, fmt.Errorf("required secret %q is not set in dockrail's environment", n)
		}
		out[n] = v
	}
	return out, nil
}
