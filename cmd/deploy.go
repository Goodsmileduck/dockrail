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

func newDeployCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "deploy",
		Short: "deploy the project to the target host",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			conn := connection.New(cfg.Target)
			return runDeploy(cmd.Context(), conn, cfg, cmd.OutOrStdout(), dryRun)
		},
	}
	c.Flags().Bool("dry-run", false, "print the deploy plan without changing anything")
	return c
}

func runDeploy(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer, dryRun bool) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	if !dryRun {
		return e.Deploy(ctx)
	}
	fmt.Fprintln(out, "dry-run: no changes will be made")
	if errs := engine.Preflight(ctx, conn, cfg); len(errs) > 0 {
		return fmt.Errorf("preflight failed: %v", errs)
	}
	for name, svc := range cfg.Services {
		fmt.Fprintf(out, "plan pull %s tag %s\n", name, svc.ImageTag)
		fmt.Fprintf(out, "plan recreate %s (stop old, up -d --no-deps)\n", name)
		fmt.Fprintf(out, "plan readiness %s :%d%s timeout %s\n",
			svc.Readiness.Type, svc.Readiness.Port, svc.Readiness.Path, svc.Readiness.Timeout)
	}
	return nil
}
