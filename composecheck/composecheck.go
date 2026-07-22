// Package composecheck validates the local compose file against deploy.yml
// references using compose-go — parse + interpolate only, no Docker daemon,
// so it works anywhere dockrail runs (laptop, CI). The target host's copy of
// the compose file is still checked separately by engine.Preflight.
package composecheck

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/goodsmileduck/dockrail/config"
)

// Load parses and interpolates the compose file at path. TAG and DOCKRAIL_GPU
// get placeholder values because dockrail injects them at deploy time; other
// ${VAR} references resolve from the invoking environment, same as compose
// itself would.
func Load(ctx context.Context, path, project string) (*types.Project, error) {
	opts, err := cli.NewProjectOptions(
		[]string{path},
		cli.WithName(project),
		cli.WithOsEnv,
		cli.WithEnv([]string{"TAG=dockrail-validate", "DOCKRAIL_GPU=0"}),
		cli.WithInterpolation(true),
		cli.WithNormalization(true),
	)
	if err != nil {
		return nil, err
	}
	return cli.ProjectFromOptions(ctx, opts)
}

// Validate checks that every service deploy.yml references exists in the
// compose file: <name> for recreate cutover, <name>-blue and <name>-green for
// proxy cutover. A parse failure is returned as the single error.
func Validate(ctx context.Context, cfg *config.Config) []error {
	project, err := Load(ctx, cfg.Compose, cfg.Project)
	if err != nil {
		return []error{fmt.Errorf("compose %s: %w", cfg.Compose, err)}
	}
	present := map[string]bool{}
	for name := range project.Services {
		present[name] = true
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(cfg.Services)) {
		svc := cfg.Services[name]
		want := []string{name}
		if svc.Cutover.Strategy == "proxy" {
			want = []string{name + "-blue", name + "-green"}
		}
		for _, w := range want {
			if !present[w] {
				errs = append(errs, fmt.Errorf("services.%s: compose file %s has no service %q", name, cfg.Compose, w))
			}
		}
	}
	return errs
}
