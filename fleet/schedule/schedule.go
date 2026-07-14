// Package schedule bin-packs auto backend replicas onto concrete host:gpu
// slots and validates pins against observed VRAM. It is a pure function — no
// I/O, no host contact — so plans are deterministic and table-testable.
package schedule

import (
	"fmt"
	"sort"

	"github.com/goodsmileduck/dockrail/fleet"
	"github.com/goodsmileduck/dockrail/fleet/observe"
	"github.com/goodsmileduck/dockrail/vram"
)

func resolvePolicy(cfg *fleet.Config, b fleet.Backend) string {
	if b.Placement.Policy != "" {
		return b.Placement.Policy
	}
	if cfg.Scheduler.Policy != "" {
		return cfg.Scheduler.Policy
	}
	return "spread"
}

type Assignment struct {
	Replica int
	Host    string
	GPU     int
}

type Placements map[string][]Assignment

// ScheduleError reports the first replica that could not be placed.
type ScheduleError struct {
	Backend     string
	Replica     int
	NeededMiB   int
	BestFreeMiB int
}

func (e *ScheduleError) Error() string {
	return fmt.Sprintf("backend %q replica %d: no GPU with enough free VRAM (need %d MiB, best free %d MiB)",
		e.Backend, e.Replica, e.NeededMiB, e.BestFreeMiB)
}

type gpuRef struct {
	host string
	idx  int
}

// Plan assigns every GPU-scheduled replica a concrete host:gpu slot.
func Plan(cfg *fleet.Config, state observe.FleetState) (Placements, error) {
	ledger := map[gpuRef]int{} // available MiB per schedulable GPU
	for _, h := range state.Hosts {
		if h.Err != "" {
			continue
		}
		for _, g := range h.GPUs {
			ledger[gpuRef{h.Name, g.Index}] = g.FreeMiB
		}
	}
	// occupied[ref] = set of backend names already holding a replica on that GPU.
	occupied := map[gpuRef]map[string]bool{}
	place := func(ref gpuRef, backend string, need int) {
		ledger[ref] -= need
		if occupied[ref] == nil {
			occupied[ref] = map[string]bool{}
		}
		occupied[ref][backend] = true
	}

	names := make([]string, 0, len(cfg.Backends))
	for name := range cfg.Backends {
		names = append(names, name)
	}
	sort.Strings(names)

	out := Placements{}
	for _, name := range names {
		b := cfg.Backends[name]
		if !b.Placement.GPU.Auto && len(b.Placement.GPU.Pins) == 0 {
			continue // not GPU-scheduled
		}
		need := 0
		if b.Placement.VRAMMin != "" {
			m, err := vram.ParseMiB(b.Placement.VRAMMin)
			if err != nil {
				return nil, fmt.Errorf("backends.%s: %w", name, err)
			}
			need = int(float64(m)*vram.SafetyFactor + 0.5)
		}

		if len(b.Placement.GPU.Pins) > 0 {
			for i, pin := range b.Placement.GPU.Pins {
				host, idx, err := fleet.ParsePin(pin)
				if err != nil {
					return nil, fmt.Errorf("backends.%s: %w", name, err)
				}
				ref := gpuRef{host, idx}
				avail, ok := ledger[ref]
				if !ok {
					return nil, fmt.Errorf("backends.%s: pin %q targets an unschedulable or unknown gpu", name, pin)
				}
				if avail < need {
					return nil, &ScheduleError{Backend: name, Replica: i, NeededMiB: need, BestFreeMiB: avail}
				}
				place(ref, name, need)
				out[name] = append(out[name], Assignment{Replica: i, Host: host, GPU: idx})
			}
			continue
		}

		// auto: place Replicas replicas by spread (most-free-first).
		pool := map[string]bool{}
		for _, h := range b.Placement.Pool {
			pool[h] = true
		}
		policy := resolvePolicy(cfg, b)
		for r := 0; r < b.Replicas; r++ {
			ref, ok, best := selectGPU(policy, ledger, occupied, name, pool, need)
			if !ok {
				return nil, &ScheduleError{Backend: name, Replica: r, NeededMiB: need, BestFreeMiB: best}
			}
			place(ref, name, need)
			out[name] = append(out[name], Assignment{Replica: r, Host: ref.host, GPU: ref.idx})
		}
	}
	return out, nil
}

// selectGPU picks a candidate GPU in the pool that fits `need` and does not
// already hold a replica of `backend`, per policy. Ties (and first-fit order)
// break by (host, index) for determinism. `best` is the largest pool-GPU free
// seen (for the ScheduleError shortfall) even when nothing fits.
func selectGPU(policy string, ledger map[gpuRef]int, occupied map[gpuRef]map[string]bool, backend string, pool map[string]bool, need int) (chosen gpuRef, ok bool, best int) {
	refs := make([]gpuRef, 0, len(ledger))
	for ref := range ledger {
		if pool[ref.host] {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].host != refs[j].host {
			return refs[i].host < refs[j].host
		}
		return refs[i].idx < refs[j].idx
	})
	bestFree := -1
	for _, ref := range refs {
		avail := ledger[ref]
		if avail > bestFree {
			bestFree = avail
		}
		if occupied[ref][backend] || avail < need {
			continue
		}
		if !ok {
			chosen, ok = ref, true
			continue
		}
		switch policy {
		case "binpack": // least-free that still fits
			if avail < ledger[chosen] {
				chosen = ref
			}
		case "first-fit": // first in (host,index) order — keep the earlier one
			// refs is already sorted; the first match wins, so do nothing.
		default: // spread: most-free-first
			if avail > ledger[chosen] {
				chosen = ref
			}
		}
	}
	if bestFree < 0 {
		bestFree = 0
	}
	return chosen, ok, bestFree
}
