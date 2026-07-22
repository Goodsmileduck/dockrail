// Package secrets resolves secret values from a configured provider. The
// engine remains responsible for delivery (mode-600 env_file on the target,
// decision D8); providers only fetch values into memory.
package secrets

import (
	"context"
	"fmt"
	"os"
)

// Provider fetches secret values by name from wherever it is backed by.
type Provider interface {
	// Fetch returns a value for every requested name; any missing or empty
	// name is an error (same contract collectSecrets had).
	Fetch(ctx context.Context, names []string) (map[string]string, error)
}

// New constructs the Provider named by provider ("" or "env" → Env{};
// "infisical" → NewInfisical()). Any other value is an error.
func New(provider string) (Provider, error) {
	switch provider {
	case "", "env":
		return Env{}, nil
	case "infisical":
		return NewInfisical()
	default:
		return nil, fmt.Errorf("secrets.provider %q: must be env or infisical", provider)
	}
}

// Env reads each name from dockrail's own environment (invoking shell / CI
// job) — the v1 behavior, now as the default provider. The dogfood project
// keeps these in ~/.bashrc/.env; a non-interactive SSH shell on the target
// would not have them, so dockrail forwards them explicitly. An unset or
// empty required secret is fatal.
type Env struct{}

func (Env) Fetch(_ context.Context, names []string) (map[string]string, error) {
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

// NewInfisical is a temporary stub so New compiles; Task 10 replaces it with
// a real Infisical-backed provider.
func NewInfisical() (Provider, error) {
	return nil, fmt.Errorf("infisical provider: not yet implemented")
}
