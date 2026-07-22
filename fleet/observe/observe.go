package observe

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
)

// psQuery MUST stay a single backtick raw string so the literal \t escapes and
// the embedded double quotes reach docker intact (the format is single-quoted
// so the shell does not touch it — same shell-safety guarantee as sub-spec 1).
// Do NOT rebuild this with "\t" concatenation: that inserts real tab bytes and
// breaks the TestPSQuery_TemplateSurvivesShell guard (which checks for a literal
// backslash-t) as well as docker template consistency.
const psQuery = `docker ps --format '{{.Names}}\t{{.Image}}\t{{.Label "dockrail.managed"}}\t{{.Label "dockrail.backend"}}\t{{.Label "dockrail.replica"}}\t{{.Label "dockrail.gpu"}}\t{{.Label "dockrail.service"}}'`

// dockrail container labels: self-describing identity the Planner diffs on.
const (
	LabelManaged = "dockrail.managed"
	LabelBackend = "dockrail.backend"
	LabelReplica = "dockrail.replica"
	LabelGPU     = "dockrail.gpu"
	LabelService = "dockrail.service"

	// LabelConfigHash is stamped by fleet/override onto generated overrides;
	// the Planner diffs it against the desired hash (Task 4 wires the
	// psQuery column that surfaces it from `docker ps`).
	LabelConfigHash = "dockrail.config-hash"
)

// labelCols maps the trailing psQuery columns (after name, image) to label keys.
var labelCols = []string{LabelManaged, LabelBackend, LabelReplica, LabelGPU, LabelService}

var errNilConfig = errors.New("observe: nil config")

type Container struct {
	Name   string            `json:"name"`
	Image  string            `json:"image"`
	Labels map[string]string `json:"labels,omitempty"`
}

type HostState struct {
	Name       string      `json:"name"`
	Containers []Container `json:"containers"`
	GPUs       []GPUState  `json:"gpus"`
	Err        string      `json:"err,omitempty"`
}

type FleetState struct {
	Hosts []HostState `json:"hosts"`
}

// ConnFactory builds a connection for a host. Injected so tests pass fakes.
type ConnFactory func(name string, h fleet.Host) (connection.Connection, error)

type Observer struct {
	Cfg     *fleet.Config
	Factory ConnFactory
}

func (o *Observer) Observe(ctx context.Context) (FleetState, error) {
	if o.Cfg == nil {
		return FleetState{}, errNilConfig
	}
	names := make([]string, 0, len(o.Cfg.Hosts))
	for n := range o.Cfg.Hosts {
		names = append(names, n)
	}
	sort.Strings(names)

	var fs FleetState
	for _, name := range names {
		h := o.Cfg.Hosts[name]
		fs.Hosts = append(fs.Hosts, o.observeHost(ctx, name, h))
	}
	return fs, nil
}

func (o *Observer) observeHost(ctx context.Context, name string, h fleet.Host) HostState {
	hs := HostState{Name: name}
	fail := func(err error) HostState {
		hs.Err = err.Error()
		return hs
	}
	conn, err := o.Factory(name, h)
	if err != nil {
		return fail(err)
	}
	psOut, err := conn.Run(ctx, psQuery)
	if err != nil {
		return fail(err)
	}
	hs.Containers = parseContainers(psOut)
	if len(h.GPUs) > 0 {
		gpuOut, err := conn.Run(ctx, gpuQuery)
		if err != nil {
			return fail(err)
		}
		gpus, err := parseGPUs(gpuOut, h.GPUs)
		if err != nil {
			return fail(err)
		}
		hs.GPUs = gpus
	}
	return hs
}

func parseContainers(out string) []Container {
	var res []Container
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		c := Container{Name: strings.TrimSpace(parts[0]), Image: strings.TrimSpace(parts[1])}
		for i, key := range labelCols {
			col := i + 2
			if col >= len(parts) {
				break
			}
			if v := strings.TrimSpace(parts[col]); v != "" {
				if c.Labels == nil {
					c.Labels = map[string]string{}
				}
				c.Labels[key] = v
			}
		}
		res = append(res, c)
	}
	return res
}
