// Package plan diffs desired fleet config against observed reality (matched by
// dockrail container labels) and emits a phased, ordered action list. Pure —
// no I/O; execution is the apply engine.
package plan

import (
	"fmt"
	"sort"
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
type Plan struct{ Phases []Phase }

// obsReplica is an observed managed backend replica.
type obsReplica struct {
	host string
	gpu  int
	tag  string
}

func tagOf(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return ""
}

// Compute diffs cfg against observed and returns the phased plan. This task
// handles backend replicas only; services + rewire are added in Task 4.
func Compute(cfg *fleet.Config, observed observe.FleetState) (Plan, error) {
	// Index observed managed backend replicas: obs[backend][replica].
	obs := map[string]map[int]obsReplica{}
	for _, h := range observed.Hosts {
		for _, c := range h.Containers {
			if c.Labels[observe.LabelManaged] != "true" {
				continue
			}
			b := c.Labels[observe.LabelBackend]
			if b == "" {
				continue // service containers handled in Task 4
			}
			r, err := strconv.Atoi(c.Labels[observe.LabelReplica])
			if err != nil {
				continue
			}
			g, _ := strconv.Atoi(c.Labels[observe.LabelGPU])
			if obs[b] == nil {
				obs[b] = map[int]obsReplica{}
			}
			obs[b][r] = obsReplica{host: h.Name, gpu: g, tag: tagOf(c.Image)}
		}
	}

	backends := sortedKeys(cfg.Backends)

	// Classify present replicas: satisfied/update keep their GPU (kept); their
	// tag/update actions collected. Missing replicas placed via PlanDelta.
	kept := schedule.Placements{}
	var converge, drain []Action
	for _, name := range backends {
		b := cfg.Backends[name]
		if !b.Placement.GPU.Auto && len(b.Placement.GPU.Pins) == 0 {
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
			}
		}
	}

	// Place missing replicas on free capacity, kept reserved.
	placements, err := schedule.PlanDelta(cfg, observed, kept)
	if err != nil {
		return Plan{}, err
	}
	for _, name := range backends {
		b := cfg.Backends[name]
		if !b.Placement.GPU.Auto && len(b.Placement.GPU.Pins) == 0 {
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
		}
	}

	// Extra: observed replicas beyond desired count, or backend not desired.
	for _, name := range sortedKeys(obs) {
		desired := 0
		if b, ok := cfg.Backends[name]; ok {
			desired = b.Replicas
		}
		for _, r := range sortedInts(obs[name]) {
			if r >= desired {
				o := obs[name][r]
				drain = append(drain, Action{Kind: RemoveReplica, Backend: name, Replica: r, Host: o.host, GPU: o.gpu})
			}
		}
	}

	// Services: index observed service containers by dockrail.service -> tag.
	obsSvc := map[string]string{}
	for _, h := range observed.Hosts {
		for _, c := range h.Containers {
			if c.Labels[observe.LabelManaged] != "true" {
				continue
			}
			if s := c.Labels[observe.LabelService]; s != "" {
				obsSvc[s] = tagOf(c.Image)
			}
		}
	}
	var rewire []Action
	for _, name := range sortedServiceKeys(cfg.Services) {
		s := cfg.Services[name]
		if cur, ok := obsSvc[name]; !ok {
			converge = append(converge, Action{Kind: DeployService, Service: name, Host: s.Host, Tag: s.ImageTag})
		} else if cur != s.ImageTag {
			converge = append(converge, Action{Kind: UpdateService, Service: name, Host: s.Host, Tag: s.ImageTag, OldTag: cur})
		}
		for _, u := range s.Uses {
			rewire = append(rewire, Action{Kind: Rewire, Service: name, Backend: u.Backend,
				Endpoints: endpointsOf(placements[u.Backend])})
		}
	}

	return assemble(converge, rewire, drain), nil
}

// assemble builds the three-phase plan, dropping empty phases' actions but
// keeping phase structure. rewire is filled in Task 4.
func assemble(converge, rewire, drain []Action) Plan {
	return Plan{Phases: []Phase{
		{Name: "converge", Actions: converge},
		{Name: "rewire", Actions: rewire},
		{Name: "drain", Actions: drain},
	}}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedInts(m map[int]obsReplica) []int {
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	return ks
}

func sortedServiceKeys(m map[string]fleet.Service) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// endpointsOf returns the host of each placed replica (port derivation deferred).
func endpointsOf(as []schedule.Assignment) []string {
	eps := make([]string, 0, len(as))
	for _, a := range as {
		eps = append(eps, a.Host)
	}
	return eps
}
