package observe

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
)

const psQuery = "docker ps --format {{.Names}}\\t{{.Image}}"

var errNilConfig = errors.New("observe: nil config")

type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
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
	conn, err := o.Factory(name, h)
	if err != nil {
		hs.Err = err.Error()
		return hs
	}
	psOut, err := conn.Run(ctx, psQuery)
	if err != nil {
		hs.Err = err.Error()
		return hs
	}
	hs.Containers = parseContainers(psOut)
	if len(h.GPUs) > 0 {
		gpuOut, err := conn.Run(ctx, gpuQuery)
		if err != nil {
			hs.Err = err.Error()
			return hs
		}
		gpus, err := parseGPUs(gpuOut, h.GPUs)
		if err != nil {
			hs.Err = err.Error()
			return hs
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
		name, image, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		res = append(res, Container{Name: strings.TrimSpace(name), Image: strings.TrimSpace(image)})
	}
	return res
}
