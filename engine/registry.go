package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

// registryLogin authenticates the target to a private registry before pulls.
// It runs only when a server is configured and both credential env vars are
// present in dockrail's environment; otherwise it is a logged no-op (the host
// may already be authenticated). The password is piped via --password-stdin,
// never placed in argv.
func registryLogin(ctx context.Context, conn connection.Connection, reg config.Registry, out io.Writer) error {
	if reg.Server == "" {
		return nil
	}
	user, uok := os.LookupEnv("DOCKRAIL_REGISTRY_USER")
	pass, pok := os.LookupEnv("DOCKRAIL_REGISTRY_PASSWORD")
	if !uok || !pok || user == "" || pass == "" {
		fmt.Fprintf(out, "registry: no DOCKRAIL_REGISTRY_USER/PASSWORD set — skip login, assuming host is authenticated to %s\n", reg.Server)
		return nil
	}
	// escape single quotes in the password for the POSIX printf
	esc := strings.ReplaceAll(pass, `'`, `'\''`)
	cmd := fmt.Sprintf("printf '%%s' '%s' | docker login %s --username %s --password-stdin",
		esc, reg.Server, user)
	if _, err := conn.Run(ctx, cmd); err != nil {
		return fmt.Errorf("docker login %s: %w", reg.Server, err)
	}
	return nil
}
