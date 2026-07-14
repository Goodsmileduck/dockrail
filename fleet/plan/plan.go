// Package plan diffs desired fleet config against observed reality (matched by
// dockrail container labels) and emits a phased, ordered action list. Pure —
// no I/O; execution is the apply engine.
package plan

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/observe"
	"github.com/goodsmileduck/dockrail/fleet/schedule"
)

type ActionKind string

const (
	PlaceReplica  ActionKind = "place-replica"
	UpdateReplica ActionKind = "update-replica"
	RemoveReplica ActionKind = "remove-replica"
	DeployService ActionKind = "deploy-service"
	UpdateService ActionKind = "update-service"
	Rewire        ActionKind = "rewire"
)

type Action struct {
	Kind      ActionKind
	Backend   string
	Replica   int
	Service   string
	Host      string
	GPU       int
	Tag       string
	OldTag    string
	Endpoints []string
}

func (a Action) String() string {
	switch a.Kind {
	case PlaceReplica:
		return fmt.Sprintf("place  %s/%d  %s:%d  %s", a.Backend, a.Replica, a.Host, a.GPU, a.Tag)
	case UpdateReplica:
		return fmt.Sprintf("update %s/%d  %s:%d  %s (was %s)", a.Backend, a.Replica, a.Host, a.GPU, a.Tag, a.OldTag)
	case RemoveReplica:
		return fmt.Sprintf("remove %s/%d  %s:%d", a.Backend, a.Replica, a.Host, a.GPU)
	case DeployService:
		return fmt.Sprintf("deploy %s  %s  %s", a.Service, a.Host, a.Tag)
	case UpdateService:
		return fmt.Sprintf("update %s  %s  %s (was %s)", a.Service, a.Host, a.Tag, a.OldTag)
	case Rewire:
		return fmt.Sprintf("rewire %s → %s  %v", a.Service, a.Backend, a.Endpoints)
	}
	return string(a.Kind)
}

type Phase struct {
	Name    string
	Actions []Action
}
type Plan struct {
	Phases   []Phase
	Warnings []string
}

// obsReplica is an observed managed backend replica.
type obsReplica struct {
	host string
	gpu  int
	tag  string
}

func tagOf(image string) string {
	if slash := strings.LastIndex(image, "/"); slash >= 0 {
		image = image[slash+1:]
	}
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return ""
}

// Compute diffs cfg against observed reality (matched by dockrail labels) and
// returns the phased plan plus any warnings. Backends/services on an
// unreachable (Err) host are left unreconciled with a warning rather than
// blindly re-placed elsewhere.
func Compute(cfg *fleet.Config, observed observe.FleetState) (Plan, error) {
	errHosts := map[string]bool{}
	for _, h := range observed.Hosts {
		if h.Err != "" {
			errHosts[h.Name] = true
		}
	}

	// Index observed managed backend replicas and service tags.
	obs := map[string]map[int]obsReplica{}
	obsSvc := map[string]string{}
	for _, h := range observed.Hosts {
		for _, c := range h.Containers {
			if c.Labels[observe.LabelManaged] != "true" {
				continue
			}
			if b := c.Labels[observe.LabelBackend]; b != "" {
				r, err := strconv.Atoi(c.Labels[observe.LabelReplica])
				if err != nil {
					continue
				}
				g, _ := strconv.Atoi(c.Labels[observe.LabelGPU])
				if obs[b] == nil {
					obs[b] = map[int]obsReplica{}
				}
				obs[b][r] = obsReplica{host: h.Name, gpu: g, tag: tagOf(c.Image)}
			} else if s := c.Labels[observe.LabelService]; s != "" {
				obsSvc[s] = tagOf(c.Image)
			}
		}
	}

	backends := slices.Sorted(maps.Keys(cfg.Backends))

	// A backend whose pool/pin hosts include an unreachable host cannot be
	// safely reconciled (its replicas may be hidden there): warn and skip it.
	blocked := map[string]bool{}
	var warnings []string
	for _, name := range backends {
		b := cfg.Backends[name]
		if !gpuScheduled(b) {
			continue
		}
		for _, h := range backendHosts(b) {
			if errHosts[h] {
				blocked[name] = true
				break
			}
		}
		if blocked[name] {
			warnings = append(warnings, fmt.Sprintf("backend %q: a placement host is unreachable — leaving its replicas unplanned", name))
		}
	}

	// changed[backend] = a place/update/remove action touched it this cycle;
	// used to gate rewire so a steady-state fleet emits an empty plan.
	changed := map[string]bool{}
	kept := schedule.Placements{}
	var converge, drain []Action
	for _, name := range backends {
		b := cfg.Backends[name]
		if blocked[name] || !gpuScheduled(b) {
			continue
		}
		for r := 0; r < b.Replicas; r++ {
			o, ok := obs[name][r]
			if !ok {
				continue // missing — placed below
			}
			kept[name] = append(kept[name], schedule.Assignment{Replica: r, Host: o.host, GPU: o.gpu})
			if o.tag != b.ImageTag {
				converge = append(converge, Action{Kind: UpdateReplica, Backend: name, Replica: r,
					Host: o.host, GPU: o.gpu, Tag: b.ImageTag, OldTag: o.tag})
				changed[name] = true
			}
		}
	}

	// Place missing replicas on free capacity, kept reserved. Blocked backends
	// are excluded from the scheduling problem entirely.
	schedCfg := *cfg
	schedCfg.Backends = map[string]fleet.Backend{}
	for _, n := range backends {
		if !blocked[n] {
			schedCfg.Backends[n] = cfg.Backends[n]
		}
	}
	placements, err := schedule.PlanDelta(&schedCfg, observed, kept)
	if err != nil {
		return Plan{}, err
	}
	for _, name := range backends {
		b := cfg.Backends[name]
		if blocked[name] || !gpuScheduled(b) {
			continue
		}
		keptR := map[int]bool{}
		for _, a := range kept[name] {
			keptR[a.Replica] = true
		}
		for _, a := range placements[name] {
			if keptR[a.Replica] {
				continue
			}
			converge = append(converge, Action{Kind: PlaceReplica, Backend: name, Replica: a.Replica,
				Host: a.Host, GPU: a.GPU, Tag: b.ImageTag})
			changed[name] = true
		}
	}

	// Extra: observed replicas beyond desired count, or backend not desired.
	for _, name := range slices.Sorted(maps.Keys(obs)) {
		if blocked[name] {
			continue
		}
		desired := 0
		if b, ok := cfg.Backends[name]; ok {
			desired = b.Replicas
		}
		for _, r := range slices.Sorted(maps.Keys(obs[name])) {
			if r >= desired {
				o := obs[name][r]
				drain = append(drain, Action{Kind: RemoveReplica, Backend: name, Replica: r, Host: o.host, GPU: o.gpu})
				changed[name] = true
			}
		}
	}

	// Services: deploy/update, and rewire only when the service or its backend
	// changed this cycle (a steady-state binding needs no rewire).
	var rewire []Action
	for _, name := range slices.Sorted(maps.Keys(cfg.Services)) {
		s := cfg.Services[name]
		if errHosts[s.Host] {
			warnings = append(warnings, fmt.Sprintf("service %q: host %q is unreachable — leaving it unplanned", name, s.Host))
			continue
		}
		svcChanged := false
		if cur, ok := obsSvc[name]; !ok {
			converge = append(converge, Action{Kind: DeployService, Service: name, Host: s.Host, Tag: s.ImageTag})
			svcChanged = true
		} else if cur != s.ImageTag {
			converge = append(converge, Action{Kind: UpdateService, Service: name, Host: s.Host, Tag: s.ImageTag, OldTag: cur})
			svcChanged = true
		}
		for _, u := range s.Uses {
			if blocked[u.Backend] {
				continue
			}
			if svcChanged || changed[u.Backend] {
				rewire = append(rewire, Action{Kind: Rewire, Service: name, Backend: u.Backend,
					Endpoints: endpointsOf(placements[u.Backend])})
			}
		}
	}

	p := assemble(converge, rewire, drain)
	p.Warnings = warnings
	return p, nil
}

// assemble builds the three-phase plan, dropping empty phases' actions but
// keeping phase structure.
func assemble(converge, rewire, drain []Action) Plan {
	return Plan{Phases: []Phase{
		{Name: "converge", Actions: converge},
		{Name: "rewire", Actions: rewire},
		{Name: "drain", Actions: drain},
	}}
}

// gpuScheduled reports whether a backend places replicas on GPUs (auto or pins).
func gpuScheduled(b fleet.Backend) bool {
	return b.Placement.GPU.Auto || len(b.Placement.GPU.Pins) > 0
}

// endpointsOf returns the host of each placed replica (port derivation deferred).
func endpointsOf(as []schedule.Assignment) []string {
	eps := make([]string, 0, len(as))
	for _, a := range as {
		eps = append(eps, a.Host)
	}
	return eps
}

// backendHosts returns the hosts a backend may occupy (pool for auto, pin hosts
// for pinned) — used to detect when an unreachable host makes it unreconcilable.
func backendHosts(b fleet.Backend) []string {
	if b.Placement.GPU.Auto {
		return b.Placement.Pool
	}
	var hs []string
	for _, pin := range b.Placement.GPU.Pins {
		if h, _, err := fleet.ParsePin(pin); err == nil {
			hs = append(hs, h)
		}
	}
	return hs
}
