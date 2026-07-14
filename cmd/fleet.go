package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/observe"
)

func newFleetCmd() *cobra.Command {
	fleetCmd := &cobra.Command{
		Use:   "fleet",
		Short: "multi-host fleet operations (v2)",
	}
	fleetCmd.PersistentFlags().StringP("fleet", "f", "fleet.yml", "path to fleet.yml")

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "show running containers and GPU state across all hosts",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("fleet")
			cfg, err := fleet.Load(path)
			if err != nil {
				return err
			}
			asJSON, _ := cmd.Flags().GetBool("json")
			return runFleetStatus(cmd.Context(), cfg, sshFactory, cmd.OutOrStdout(), asJSON)
		},
	}
	statusCmd.Flags().Bool("json", false, "emit machine-readable JSON instead of text")
	fleetCmd.AddCommand(statusCmd)
	return fleetCmd
}

// sshFactory is the production ConnFactory: build a connection from a host's SSH target.
func sshFactory(_ string, h fleet.Host) (connection.Connection, error) {
	return connection.New(config.Target{Host: h.SSH, Port: h.Port}), nil
}

func runFleetStatus(ctx context.Context, cfg *fleet.Config, factory observe.ConnFactory, out io.Writer, asJSON bool) error {
	o := &observe.Observer{Cfg: cfg, Factory: factory}
	st, err := o.Observe(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	for _, h := range st.Hosts {
		fmt.Fprintf(out, "%s:\n", h.Name)
		if h.Err != "" {
			fmt.Fprintf(out, "  ERROR: %s\n", h.Err)
		}
		for _, c := range h.Containers {
			fmt.Fprintf(out, "  container %s (%s)\n", c.Name, c.Image)
		}
		for _, g := range h.GPUs {
			fmt.Fprintf(out, "  gpu%d: %d/%d MiB free\n", g.Index, g.FreeMiB, g.TotalMiB)
		}
	}
	return nil
}
