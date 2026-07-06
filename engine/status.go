package engine

import (
	"context"
	"fmt"
	"sort"
)

type ServiceStatus struct {
	Name       string `json:"name"`
	RunningTag string `json:"running_tag"`
	Up         bool   `json:"up"`
}

type StatusReport struct {
	CurrentTag  string          `json:"current_tag"`
	PreviousTag string          `json:"previous_tag"`
	LastFailure string          `json:"last_failure,omitempty"`
	Services    []ServiceStatus `json:"services"`
}

// Status reports the deployed tag pair derived from deploy history plus the
// live running image tag per service. It is read-only.
func (e *Engine) Status(ctx context.Context) (StatusReport, error) {
	h, err := loadHistory(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return StatusReport{}, err
	}
	rep := StatusReport{LastFailure: lastFailure(h), PreviousTag: previousTag(h)}
	if cur, ok := currentRecord(h); ok {
		rep.CurrentTag = cur.Tag
	}
	names := make([]string, 0, len(e.Cfg.Services))
	for name := range e.Cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ss := ServiceStatus{Name: name}
		cid, img, err := e.runningImage(ctx, name)
		if err != nil {
			return StatusReport{}, fmt.Errorf("status %s: %w", name, err)
		}
		if cid != "" {
			ss.Up = true
			ss.RunningTag = img
		}
		rep.Services = append(rep.Services, ss)
	}
	return rep, nil
}
