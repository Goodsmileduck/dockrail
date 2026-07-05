package connection

import (
	"context"

	"github.com/goodsmileduck/dockrail/config"
)

type Connection interface {
	Run(ctx context.Context, cmd string) (string, error)
}

func New(t config.Target) Connection {
	if t.Host == "" {
		return NewLocal()
	}
	return NewSSH(t.Host, t.Port)
}
