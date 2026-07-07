package cmd

import (
	"context"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/engine"
)

func newRollbackCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "rollback [TAG]",
		Short: "restore the previous image tag, or an explicit retained TAG",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			lockWait, _ := cmd.Flags().GetDuration("lock-wait")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			conn := connection.New(cfg.Target)
			tag := ""
			if len(args) == 1 {
				tag = args[0]
			}
			return runRollback(cmd.Context(), conn, cfg, cmd.OutOrStdout(), tag, lockWait)
		},
	}
	c.Flags().Duration("lock-wait", 0, "wait up to this long for the deploy lock (e.g. 5m); 0 fails immediately")
	return c
}

func runRollback(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer, tag string, lockWait time.Duration) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out, LockWait: lockWait}
	if tag != "" {
		return e.RollbackTo(ctx, tag)
	}
	return e.Rollback(ctx)
}
