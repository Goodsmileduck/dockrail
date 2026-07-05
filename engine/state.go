package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goodsmileduck/dockrail/connection"
)

type State struct {
	PreviousTag string `json:"previous_tag"`
	CurrentTag  string `json:"current_tag"`
	LastFailure string `json:"last_failure,omitempty"`
}

func statePath(project string) string {
	return fmt.Sprintf("$HOME/.dockrail/%s/state.json", project)
}

func loadState(ctx context.Context, conn connection.Connection, project string) (State, error) {
	out, err := conn.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null || true", statePath(project)))
	var s State
	if err != nil {
		return s, err
	}
	if strings.TrimSpace(out) == "" {
		return s, nil // first deploy
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return s, fmt.Errorf("corrupt state file: %w", err)
	}
	return s, nil
}

func saveState(ctx context.Context, conn connection.Connection, project string, s State) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	dir := fmt.Sprintf("$HOME/.dockrail/%s", project)
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s <<'DDEOF'\n%s\nDDEOF", dir, statePath(project), raw)
	_, err = conn.Run(ctx, cmd)
	return err
}

func acquireLock(ctx context.Context, conn connection.Connection, project string) (func(), error) {
	lockDir := fmt.Sprintf("$HOME/.dockrail/%s/lock", project)
	mk := fmt.Sprintf("mkdir -p $HOME/.dockrail/%s && mkdir %s", project, lockDir)
	if _, err := conn.Run(ctx, mk); err != nil {
		return nil, fmt.Errorf("another deploy appears to be running (lock %s held): %w", lockDir, err)
	}
	release := func() {
		_, _ = conn.Run(context.Background(), fmt.Sprintf("rmdir %s", lockDir))
	}
	return release, nil
}
