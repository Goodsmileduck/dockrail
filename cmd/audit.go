package cmd

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/engine"
)

func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "print the deploy history recorded on the target host",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			n, _ := cmd.Flags().GetInt("n")
			conn := connection.New(cfg.Target)
			return runAudit(cmd.Context(), conn, cfg, cmd.OutOrStdout(), n)
		},
	}
	cmd.Flags().IntP("n", "n", 20, "number of records to show (0 = all)")
	return cmd
}

func runAudit(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer, n int) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	recs, anchor, err := e.Audit(ctx, n)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		fmt.Fprintln(out, "no deploy history")
		return nil
	}
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "\tTIMESTAMP\tTAG\tPERFORMER\tOUTCOME")
	for i, r := range recs {
		mark := ""
		if i == anchor {
			mark = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", mark, r.TS, r.Tag, r.Performer, r.Outcome)
	}
	return w.Flush()
}
