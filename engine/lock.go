package engine

import (
	"context"
	"fmt"

	"github.com/goodsmileduck/dockrail/connection"
)

func acquireLock(ctx context.Context, conn connection.Connection, project string) (func(), error) {
	lockDir := projectDir(project) + "/lock"
	mk := fmt.Sprintf("mkdir -p %s && mkdir %s", projectDir(project), lockDir)
	if _, err := conn.Run(ctx, mk); err != nil {
		return nil, fmt.Errorf("another deploy appears to be running (lock %s held): %w", lockDir, err)
	}
	release := func() {
		_, _ = conn.Run(context.Background(), fmt.Sprintf("rmdir %s", lockDir))
	}
	return release, nil
}
