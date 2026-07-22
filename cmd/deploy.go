package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

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
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			lockWait, _ := cmd.Flags().GetDuration("lock-wait")
			force, _ := cmd.Flags().GetBool("force")
			cfg, conn, err := loadConn(cmd)
			if err != nil {
				return err
			}
			return runDeploy(cmd.Context(), conn, cfg, cmd.OutOrStdout(), dryRun, lockWait, force)
		},
	}
	c.Flags().Bool("dry-run", false, "print the deploy plan without changing anything")
	c.Flags().Duration("lock-wait", 0, "wait up to this long for the deploy lock (e.g. 5m); 0 fails immediately")
	c.Flags().Bool("force", false, "deploy even when the config hash matches the last successful deploy")
	return c
}

func runDeploy(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer, dryRun bool, lockWait time.Duration, force bool) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out, LockWait: lockWait, Force: force}
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
