package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func newLogsCmd() *cobra.Command {
	var tail int
	var follow bool
	c := &cobra.Command{
		Use:   "logs <service>",
		Short: "show logs for a service on the target host",
		Long: "Show logs for a service on the target host.\n\n" +
			"Note: --follow (-f) is accepted but streams only what the transport\n" +
			"returns before the command exits; true tailing is not yet supported.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, conn, err := loadConn(cmd)
			if err != nil {
				return err
			}
			service := args[0]
			if _, ok := cfg.Services[service]; !ok {
				return fmt.Errorf("unknown service %q", service)
			}
			return runLogs(cmd.Context(), conn, cfg, service, tail, follow, cmd.OutOrStdout())
		},
	}
	c.Flags().IntVar(&tail, "tail", 100, "number of trailing log lines to show")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "follow log output")
	return c
}

func runLogs(ctx context.Context, conn connection.Connection, cfg *config.Config, service string, tail int, follow bool, out io.Writer) error {
	logsCmd := fmt.Sprintf("docker compose -f %s logs --tail %d", cfg.Compose, tail)
	if follow {
		logsCmd += " -f"
	}
	logsCmd += " " + service
	res, err := conn.Run(ctx, logsCmd)
	if err != nil {
		return err
	}
	fmt.Fprint(out, res)
	return nil
}
