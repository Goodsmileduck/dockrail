package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/goodsmileduck/dockrail/connection"
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

// writeSecretsFile writes the collected secrets to a mode-600 env-file on the
// target and returns a shell prefix that sources it. Secrets reach the target
// only inside this heredoc write (like state.json), never as command argv on
// later compose invocations. Returns "" and writes nothing for no secrets.
func writeSecretsFile(ctx context.Context, conn connection.Connection, project string, secrets map[string]string) (string, error) {
	if len(secrets) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(secrets))
	for n := range secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		// single-quote the value; escape embedded single quotes for POSIX sh
		v := strings.ReplaceAll(secrets[n], `'`, `'\''`)
		fmt.Fprintf(&b, "export %s='%s'\n", n, v)
	}
	dir := fmt.Sprintf("$HOME/.dockrail/%s", project)
	path := dir + "/env"
	cmd := fmt.Sprintf("mkdir -p %s && umask 177 && cat > %s <<'DDEOF'\n%sDDEOF\nchmod 600 %s",
		dir, path, b.String(), path)
	if _, err := conn.Run(ctx, cmd); err != nil {
		return "", fmt.Errorf("write secrets file: %w", err)
	}
	return fmt.Sprintf("set -a; . %s; set +a; ", path), nil
}
