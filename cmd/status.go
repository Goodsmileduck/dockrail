package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/engine"
)

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "show deployed and running image tags per service",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, conn, err := loadConn(cmd)
			if err != nil {
				return err
			}
			asJSON, _ := cmd.Flags().GetBool("json")
			return runStatus(cmd.Context(), conn, cfg, cmd.OutOrStdout(), asJSON)
		},
	}
	cmd.Flags().Bool("json", false, "emit machine-readable JSON instead of text")
	return cmd
}

func runStatus(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer, asJSON bool) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	rep, err := e.Status(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(out, rep)
	}
	fmt.Fprintf(out, "current_tag:  %s\n", rep.CurrentTag)
	fmt.Fprintf(out, "previous_tag: %s\n", rep.PreviousTag)
	if rep.LastFailure != "" {
		fmt.Fprintf(out, "last_failure: %s\n", rep.LastFailure)
	}
	for _, s := range rep.Services {
		state := "down"
		if s.Up {
			state = "up"
		}
		fmt.Fprintf(out, "  %s: %s (%s)\n", s.Name, s.RunningTag, state)
	}
	return nil
}
