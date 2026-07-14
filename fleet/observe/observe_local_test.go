package observe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
	"github.com/goodsmileduck/dockrail/fleet"
)

// TestObserve_ThroughRealShell is a level-2 integration test: it runs
// Observer.Observe against connection.Local (a REAL `sh -c`) with stub `docker`
// and `nvidia-smi` executables on PATH. Unlike the fake-connection unit tests
// (which hand back canned tab-separated output), this exercises the actual
// shell → command → parse chain, so it catches shell-escaping regressions in
// psQuery/gpuQuery that the fakes structurally cannot — e.g. the `\t` in the
// `docker ps --format` template being stripped by the shell before docker sees
// it. The stub `docker` renders the received --format template with `printf
// %b`, so if the backslash-escape were lost the tabs would vanish and this test
// would fail.
func TestObserve_ThroughRealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub-binary + sh harness is POSIX-only")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()

	writeStub(t, dir, "docker", `#!/bin/sh
if [ "$1" = "ps" ]; then
  fmt=""
  while [ $# -gt 0 ]; do
    if [ "$1" = "--format" ]; then shift; fmt="$1"; fi
    shift
  done
  row=$(printf '%s' "$fmt" \
    | sed 's#{{\.Names}}#llama-0#' \
    | sed 's#{{\.Image}}#reg/vllm:v2#' \
    | sed 's#{{\.Label "dockrail\.managed"}}#true#' \
    | sed 's#{{\.Label "dockrail\.backend"}}#llama#' \
    | sed 's#{{\.Label "dockrail\.replica"}}#0#' \
    | sed 's#{{\.Label "dockrail\.gpu"}}#1#' \
    | sed 's#{{\.Label "dockrail\.service"}}##')
  printf '%b\n' "$row"
fi
`)
	writeStub(t, dir, "nvidia-smi", `#!/bin/sh
printf '0, 24576, 0, 24576\n1, 24576, 20000, 4576\n'
`)

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := &fleet.Config{Hosts: map[string]fleet.Host{"local": {GPUs: []int{0, 1}}}}
	o := &Observer{Cfg: cfg, Factory: func(name string, h fleet.Host) (connection.Connection, error) {
		return connection.NewLocal(), nil
	}}
	st, err := o.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(st.Hosts) != 1 {
		t.Fatalf("want 1 host, got %+v", st.Hosts)
	}
	h := st.Hosts[0]
	if h.Err != "" {
		t.Fatalf("host errored: %s", h.Err)
	}
	if len(h.Containers) != 1 || h.Containers[0].Name != "llama-0" ||
		h.Containers[0].Image != "reg/vllm:v2" ||
		h.Containers[0].Labels[LabelBackend] != "llama" ||
		h.Containers[0].Labels[LabelReplica] != "0" ||
		h.Containers[0].Labels[LabelGPU] != "1" {
		t.Fatalf("container not parsed through the real shell (tab survival?): %+v", h.Containers)
	}
	if len(h.GPUs) != 2 || h.GPUs[0].FreeMiB != 24576 || h.GPUs[1].FreeMiB != 4576 {
		t.Fatalf("gpus not parsed through the real shell: %+v", h.GPUs)
	}
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
}
