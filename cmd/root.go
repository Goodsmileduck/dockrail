package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/composecheck"
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
	root.AddCommand(newFleetCmd())
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
	cmd := &cobra.Command{
		Use:   "check",
		Short: "validate config and target host readiness",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, conn, err := loadConn(cmd)
			if err != nil {
				return err
			}
			errs := engine.Preflight(cmd.Context(), conn, cfg)

			// Local compose validation: only when the file exists here; the
			// target host's copy is preflight's job.
			if _, statErr := os.Stat(cfg.Compose); statErr == nil {
				errs = append(errs, composecheck.Validate(cmd.Context(), cfg)...)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "note: compose file not present locally; skipping local compose validation")
			}

			for _, e := range errs {
				fmt.Fprintln(cmd.ErrOrStderr(), "FAIL:", e)
			}
			if len(errs) > 0 {
				return fmt.Errorf("%d check(s) failed", len(errs))
			}
			fmt.Fprintln(cmd.OutOrStdout(), "all checks passed")

			images, _ := cmd.Flags().GetBool("images")
			if images {
				drifts := engine.ImageDrift(cmd.Context(), conn, cfg)
				if len(drifts) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "images: all digests match")
				} else {
					for _, d := range drifts {
						fmt.Fprintln(cmd.OutOrStdout(), formatDrift(d))
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().Bool("images", false, "also compare host image digests against the registry (advisory)")
	return cmd
}

// formatDrift renders a single ImageDrift finding as one advisory line.
func formatDrift(d engine.Drift) string {
	if d.Note != "drift" {
		return fmt.Sprintf("WARN %s %s: %s", d.Service, d.Image, d.Note)
	}
	return fmt.Sprintf("DRIFT %s %s host=%s registry=%s", d.Service, d.Image, d.Local, d.Remote)
}
