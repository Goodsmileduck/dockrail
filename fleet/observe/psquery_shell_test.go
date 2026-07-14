package observe

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// TestPSQuery_TemplateSurvivesShell guards against the --format template's \t
// being stripped by the shell before docker sees it (single-quoting is required).
// It substitutes 'printf %s' for 'docker ps --format' as a transparent stand-in
// that echoes the exact argument the shell hands to docker, and asserts the
// backslash-t reaches it intact (docker's Go template renders \t as a real tab).
func TestPSQuery_TemplateSurvivesShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based shell round-trip not applicable on windows")
	}
	cmd := strings.Replace(psQuery, "docker ps --format", "printf %s", 1)
	out, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		t.Fatalf("sh -c: %v", err)
	}
	if !strings.Contains(string(out), `{{.Names}}\t{{.Image}}`) {
		t.Fatalf("format template mangled by shell: got %q", out)
	}
}
