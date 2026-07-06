package engine

import (
	"context"
	"fmt"

	"github.com/goodsmileduck/dockrail/connection"
)

// flipUpstream points the nginx upstream for a service at the given color's
// container and reloads nginx. The operator's nginx config must `include` the
// $HOME/.dockrail/<project>/nginx/ directory. Reload is issued inside the
// nginx container named by cutover.proxy.
func flipUpstream(ctx context.Context, conn connection.Connection, project, nginxContainer, service, color string, port int) error {
	dir := fmt.Sprintf("$HOME/.dockrail/%s/nginx", project)
	path := fmt.Sprintf("%s/%s.conf", dir, service)
	frag := fmt.Sprintf("upstream %s { server %s-%s:%d; }\n", service, service, color, port)
	write := fmt.Sprintf("mkdir -p %s && cat > %s <<'DDEOF'\n%sDDEOF", dir, path, frag)
	if _, err := conn.Run(ctx, write); err != nil {
		return fmt.Errorf("write upstream fragment: %w", err)
	}
	if _, err := conn.Run(ctx, fmt.Sprintf("docker exec %s nginx -s reload", nginxContainer)); err != nil {
		return fmt.Errorf("nginx reload: %w", err)
	}
	return nil
}
