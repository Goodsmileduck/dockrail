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
		Use:   "rollback [TAG]",
		Short: "restore the previous image tag, or an explicit retained TAG",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			conn := connection.New(cfg.Target)
			tag := ""
			if len(args) == 1 {
				tag = args[0]
			}
			return runRollback(cmd.Context(), conn, cfg, cmd.OutOrStdout(), tag)
		},
	}
}

func runRollback(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer, tag string) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	if tag != "" {
		return e.RollbackTo(ctx, tag)
	}
	return e.Rollback(ctx)
}
