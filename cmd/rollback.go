package cmd

import (
	"context"
	"io"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/engine"
)

func newRollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback",
		Short: "restore the previously deployed image tag",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			conn := connection.New(cfg.Target)
			return runRollback(cmd.Context(), conn, cfg, cmd.OutOrStdout())
		},
	}
}

func runRollback(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	return e.Rollback(ctx)
}
