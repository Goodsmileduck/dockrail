package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/engine"
)

func newLockCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lock",
		Short: "inspect or override the deploy lock on the target host",
	}
	c.AddCommand(newLockStatusCmd(), newLockAcquireCmd(), newLockReleaseCmd())
	return c
}

func newLockStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "show whether the deploy lock is held (exit 0 free, 1 held, 2 error)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Exit 2 for config/connection errors so scripts can tell
			// "held" (1) apart from "could not answer" (2).
			fail := func(err error) {
				cmd.PrintErrln("Error:", err)
				os.Exit(2)
			}
			cfg, conn, err := loadConn(cmd)
			if err != nil {
				fail(err)
			}
			held, err := runLockStatus(cmd.Context(), conn, cfg, cmd.OutOrStdout())
			if err != nil {
				fail(err)
			}
			if held {
				os.Exit(1)
			}
			return nil
		},
	}
}

func newLockAcquireCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "acquire",
		Short: "take the deploy lock manually (freeze deploys)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, conn, err := loadConn(cmd)
			if err != nil {
				return err
			}
			return runLockAcquire(cmd.Context(), conn, cfg, cmd.OutOrStdout())
		},
	}
}

func newLockReleaseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "release",
		Short: "remove the deploy lock unconditionally (human override for stale locks)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, conn, err := loadConn(cmd)
			if err != nil {
				return err
			}
			return runLockRelease(cmd.Context(), conn, cfg, cmd.OutOrStdout())
		},
	}
}

func runLockStatus(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer) (bool, error) {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	held, desc, err := e.LockStatus(ctx)
	if err != nil {
		return false, err
	}
	if held {
		fmt.Fprintln(out, desc)
	} else {
		fmt.Fprintln(out, "free")
	}
	return held, nil
}

func runLockAcquire(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	if err := e.LockAcquire(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "lock acquired; deploys are frozen until 'dockrail lock release'")
	return nil
}

func runLockRelease(ctx context.Context, conn connection.Connection, cfg *config.Config, out io.Writer) error {
	e := &engine.Engine{Conn: conn, Cfg: cfg, Out: out}
	desc, err := e.LockRelease(ctx)
	if err != nil {
		return err
	}
	if desc == "" {
		fmt.Fprintln(out, "lock is not held")
		return nil
	}
	fmt.Fprintf(out, "released (was %s)\n", desc)
	return nil
}
