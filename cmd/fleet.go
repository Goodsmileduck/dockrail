package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/engine"
	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/apply"
	"github.com/goodsmileduck/dockrail/fleet/observe"
	"github.com/goodsmileduck/dockrail/fleet/plan"
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
	planCmd := &cobra.Command{
		Use:   "plan",
		Short: "show the phased action plan to converge the fleet to fleet.yml",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("fleet")
			cfg, err := fleet.Load(path)
			if err != nil {
				return err
			}
			asJSON, _ := cmd.Flags().GetBool("json")
			return runFleetPlan(cmd.Context(), cfg, sshFactory, cmd.OutOrStdout(), asJSON)
		},
	}
	planCmd.Flags().Bool("json", false, "emit machine-readable JSON instead of text")
	fleetCmd.AddCommand(planCmd)

	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "converge the fleet to fleet.yml (health-gated, phased)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("fleet")
			cfg, err := fleet.Load(path)
			if err != nil {
				return err
			}
			onFailure, _ := cmd.Flags().GetString("on-failure")
			scope, _ := cmd.Flags().GetString("scope")
			lockWait, _ := cmd.Flags().GetDuration("lock-wait")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			asJSON, _ := cmd.Flags().GetBool("json")
			opts := apply.Options{OnFailure: onFailure, Scope: scope}
			return runFleetApply(cmd.Context(), cfg, sshFactory, cmd.OutOrStdout(), opts, lockWait, dryRun, asJSON)
		},
	}
	applyCmd.Flags().String("on-failure", "hold", "on a failed action: hold (stop, keep converged) | rollback (reverse this run)")
	applyCmd.Flags().String("scope", "", "execute only actions for this backend/service (observe whole fleet)")
	applyCmd.Flags().Duration("lock-wait", 0, "wait up to this long for the fleet lock (e.g. 15m); 0 fails immediately")
	applyCmd.Flags().Bool("dry-run", false, "print the plan without mutating (equivalent to fleet plan)")
	applyCmd.Flags().Bool("json", false, "emit machine-readable JSON instead of text")
	fleetCmd.AddCommand(applyCmd)
	return fleetCmd
}

// firstHost returns the lexicographically-first host name, the deterministic
// holder of the fleet lock so concurrent appliers contend for the same lock.
func firstHost(cfg *fleet.Config) string {
	for _, n := range slices.Sorted(maps.Keys(cfg.Hosts)) {
		return n
	}
	return ""
}

// runFleetApply observes the fleet, then applies the plan under a fleet lock.
// --dry-run delegates to the read-only plan printer (no lock, no mutation).
func runFleetApply(ctx context.Context, cfg *fleet.Config, factory observe.ConnFactory, out io.Writer, opts apply.Options, lockWait time.Duration, dryRun, asJSON bool) error {
	if dryRun {
		return runFleetPlan(ctx, cfg, factory, out, asJSON)
	}
	lockHost := firstHost(cfg)
	if lockHost == "" {
		return fmt.Errorf("fleet apply: no hosts configured")
	}
	lockConn, err := factory(lockHost, cfg.Hosts[lockHost])
	if err != nil {
		return fmt.Errorf("fleet apply: connect lock host %q: %w", lockHost, err)
	}
	release, err := engine.AcquireFleetLock(ctx, lockConn, cfg.Project, lockWait, out)
	if err != nil {
		return err
	}
	defer release()

	st, err := observeFleet(ctx, cfg, factory)
	if err != nil {
		return err
	}
	res, runErr := apply.RunFleet(ctx, cfg, st, factory, out, opts)
	if asJSON {
		if err := writeJSON(out, res); err != nil {
			return err
		}
	} else {
		printApplyResult(out, res)
	}
	return runErr
}

// printApplyResult renders an apply.Result as human-readable text.
func printApplyResult(out io.Writer, res apply.Result) {
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	for _, a := range res.Applied {
		fmt.Fprintf(out, "applied  %s\n", a.String())
	}
	if res.Failed != nil {
		fmt.Fprintf(out, "FAILED   %s\n", res.Failed.String())
	}
	for _, a := range res.Pending {
		fmt.Fprintf(out, "pending  %s\n", a.String())
	}
	if len(res.Applied) == 0 && res.Failed == nil && len(res.Pending) == 0 {
		fmt.Fprintln(out, "already converged; no actions")
	}
}

// sshFactory is the production ConnFactory: build a connection from a host's SSH target.
func sshFactory(_ string, h fleet.Host) (connection.Connection, error) {
	return connection.New(config.Target{Host: h.SSH, Port: h.Port}), nil
}

// observeFleet builds an Observer and reads the current fleet state.
func observeFleet(ctx context.Context, cfg *fleet.Config, factory observe.ConnFactory) (observe.FleetState, error) {
	o := &observe.Observer{Cfg: cfg, Factory: factory}
	return o.Observe(ctx)
}

// writeJSON writes v as indented JSON.
func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func runFleetStatus(ctx context.Context, cfg *fleet.Config, factory observe.ConnFactory, out io.Writer, asJSON bool) error {
	st, err := observeFleet(ctx, cfg, factory)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(out, st)
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

func runFleetPlan(ctx context.Context, cfg *fleet.Config, factory observe.ConnFactory, out io.Writer, asJSON bool) error {
	st, err := observeFleet(ctx, cfg, factory)
	if err != nil {
		return err
	}
	p, err := plan.Compute(cfg, st)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(out, p)
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	empty := true
	for i, ph := range p.Phases {
		if len(ph.Actions) == 0 {
			continue
		}
		empty = false
		fmt.Fprintf(out, "Phase %d — %s\n", i+1, ph.Name)
		for _, a := range ph.Actions {
			fmt.Fprintf(out, "  %s\n", a.String())
		}
	}
	if empty && len(p.Warnings) == 0 {
		fmt.Fprintln(out, "already converged; no actions")
	}
	return nil
}
