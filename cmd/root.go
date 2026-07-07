package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/engine"
)

// Version is the build version, stamped via -ldflags at release time.
var Version = "dev"

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "dockrail",
		Short:         "Compose-native deployer with health-gated cutover",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringP("config", "c", "deploy.yml", "path to deploy.yml")

	root.AddCommand(newDeployCmd())
	root.AddCommand(newCheckCmd())
	root.AddCommand(newRollbackCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newLogsCmd())
	root.AddCommand(newAuditCmd())
	root.AddCommand(newLockCmd())
	return root
}

// loadConn is the shared command prologue: load the -c config and open the
// connection its target describes.
func loadConn(cmd *cobra.Command) (*config.Config, connection.Connection, error) {
	path, _ := cmd.Flags().GetString("config")
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	return cfg, connection.New(cfg.Target), nil
}

func newCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "validate config and target host readiness",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, conn, err := loadConn(cmd)
			if err != nil {
				return err
			}
			errs := engine.Preflight(cmd.Context(), conn, cfg)
			for _, e := range errs {
				fmt.Fprintln(cmd.ErrOrStderr(), "FAIL:", e)
			}
			if len(errs) > 0 {
				return fmt.Errorf("%d preflight check(s) failed", len(errs))
			}
			fmt.Fprintln(cmd.OutOrStdout(), "all checks passed")
			return nil
		},
	}
}
