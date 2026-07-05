package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type ServiceStatus struct {
	Name       string
	RunningTag string
	Up         bool
}

type StatusReport struct {
	CurrentTag  string
	PreviousTag string
	LastFailure string
	Services    []ServiceStatus
}

// Status reports the deployed tag pair from host state plus the live running
// image tag per service. It is read-only.
func (e *Engine) Status(ctx context.Context) (StatusReport, error) {
	st, err := loadState(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return StatusReport{}, err
	}
	rep := StatusReport{
		CurrentTag:  st.CurrentTag,
		PreviousTag: st.PreviousTag,
		LastFailure: st.LastFailure,
	}
	names := make([]string, 0, len(e.Cfg.Services))
	for name := range e.Cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ss := ServiceStatus{Name: name}
		cid, err := e.Conn.Run(ctx, fmt.Sprintf(
			"docker compose -f %s ps -q %s", e.Cfg.Compose, name))
		if err != nil {
			return StatusReport{}, fmt.Errorf("status %s: %w", name, err)
		}
		cid = strings.TrimSpace(cid)
		if cid != "" {
			ss.Up = true
			img, err := e.Conn.Run(ctx, fmt.Sprintf(
				"docker inspect --format '{{.Config.Image}}' %s", cid))
			if err != nil {
				return StatusReport{}, fmt.Errorf("status %s inspect: %w", name, err)
			}
			ss.RunningTag = strings.TrimSpace(img)
		}
		rep.Services = append(rep.Services, ss)
	}
	return rep, nil
}
