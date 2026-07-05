package readiness

import (
	"context"
	"fmt"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

type Prober interface {
	Probe(ctx context.Context, conn connection.Connection) error
}

func New(r config.Readiness) (Prober, error) {
	switch r.Type {
	case "http":
		return newHTTP(r)
	case "tcp":
		return newTCP(r)
	default:
		return nil, fmt.Errorf("readiness type %q not implemented yet", r.Type)
	}
}
