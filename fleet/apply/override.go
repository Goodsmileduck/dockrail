// Package apply executes a Planner plan across the fleet via generated
// per-replica compose overrides, health-gated per the fleet serving invariant.
package apply

import (
	"fmt"

	"github.com/goodsmileduck/dockrail/fleet/observe"
)

// replicaOverride returns a compose override defining the replica as its own
// service <backend>-<replica> that extends the template service, pinned to a
// GPU and stamped with the dockrail identity labels the Observer reads. It is
// a distinct service (not a container_name on the shared template) because
// docker compose operates on services.
func replicaOverride(base, template, backend string, replica, gpu int) string {
	name := fmt.Sprintf("%s-%d", backend, replica)
	return fmt.Sprintf(`services:
  %s:
    extends:
      file: %s
      service: %s
    container_name: %s
    labels:
      %s: "true"
      %s: %s
      %s: "%d"
      %s: "%d"
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              device_ids: ["%d"]
              capabilities: [gpu]
`, name, base, template, name,
		observe.LabelManaged,
		observe.LabelBackend, backend,
		observe.LabelReplica, replica,
		observe.LabelGPU, gpu,
		gpu)
}

// serviceOverride returns an override for a routed service: its own service
// extending the template, stamped with the dockrail.service label.
func serviceOverride(base, template, service string) string {
	return fmt.Sprintf(`services:
  %s:
    extends:
      file: %s
      service: %s
    container_name: %s
    labels:
      %s: "true"
      %s: %s
`, service, base, template, service,
		observe.LabelManaged, observe.LabelService, service)
}
