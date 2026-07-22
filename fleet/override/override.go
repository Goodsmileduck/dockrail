// Package override renders the per-replica / per-service compose overrides
// and the config hash stamped into them. It is shared by apply (which writes
// the override and runs compose) and plan (which computes the same desired
// hash to diff against the observed dockrail.config-hash label).
package override

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/goodsmileduck/dockrail/fleet/observe"
)

// Hash returns "sha256:<hex>" over parts joined with an unprintable
// separator. Order-sensitive by design: the tuple is (image, base, template,
// identity..., placement...), and any reordering is a different config.
func Hash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ReplicaHash is the single source of the replica hash tuple; Replica stamps
// it and the Planner diffs it — both MUST call this, never inline Hash.
func ReplicaHash(base, template, backend string, replica, gpu int, tag string) string {
	return Hash(tag, base, template, backend, strconv.Itoa(replica), strconv.Itoa(gpu))
}

// ServiceHash is the single source of the service hash tuple; Service stamps
// it and the Planner diffs it — both MUST call this, never inline Hash.
func ServiceHash(base, template, service, tag string) string {
	return Hash(tag, base, template, service)
}

// Replica returns a compose override defining the replica as its own
// service <backend>-<replica> that extends the template service, pinned to a
// GPU and stamped with the dockrail identity labels the Observer reads. It is
// a distinct service (not a container_name on the shared template) because
// docker compose operates on services. The config-hash label is ReplicaHash
// of the tuple that shapes this override, so plan can diff desired vs
// observed.
func Replica(base, template, backend string, replica, gpu int, tag string) (body, hash string) {
	name := fmt.Sprintf("%s-%d", backend, replica)
	hash = ReplicaHash(base, template, backend, replica, gpu, tag)
	body = fmt.Sprintf(`services:
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
      %s: "%s"
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
		observe.LabelConfigHash, hash,
		gpu)
	return body, hash
}

// Service returns an override for a routed service: its own service
// extending the template, stamped with the dockrail.service label and the
// config-hash label (ServiceHash) the Planner diffs against.
func Service(base, template, service, tag string) (body, hash string) {
	hash = ServiceHash(base, template, service, tag)
	body = fmt.Sprintf(`services:
  %s:
    extends:
      file: %s
      service: %s
    container_name: %s
    labels:
      %s: "true"
      %s: %s
      %s: "%s"
`, service, base, template, service,
		observe.LabelManaged, observe.LabelService, service,
		observe.LabelConfigHash, hash)
	return body, hash
}
