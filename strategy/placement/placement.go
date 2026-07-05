package placement

import (
	"context"
	"fmt"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type Placer interface {
	Pick(ctx context.Context, conn connection.Connection) (string, error)
}

func New(p config.Placement) (Placer, error) {
	switch p.Type {
	case "", "none":
		return None{}, nil
	default:
		return nil, fmt.Errorf("placement type %q not implemented yet", p.Type)
	}
}

type None struct{}

func (None) Pick(context.Context, connection.Connection) (string, error) { return "", nil }
