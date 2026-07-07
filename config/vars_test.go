package config

import (
	"strings"
	"testing"
)

const varsYAML = `
project: demo
compose: docker-compose.yml
vars:
  tag: "v42"
  port: "8010"
target: { host: deploy@example.com }
services:
  web:
    image_tag: "${vars.tag}"
    readiness: { type: http, path: /health, port: ${vars.port}, timeout: 90s }
    cutover:   { strategy: recreate }
`

func TestVarsInterpolation(t *testing.T) {
	cfg, err := Load(write(t, varsYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Services["web"].ImageTag; got != "v42" {
		t.Errorf("image_tag = %q, want v42", got)
	}
	if got := cfg.Services["web"].Readiness.Port; got != 8010 {
		t.Errorf("readiness.port = %d, want 8010", got)
	}
}

func TestVarsUndefinedReferenceFails(t *testing.T) {
	yaml := strings.Replace(varsYAML, "${vars.tag}", "${vars.missing}", 1)
	_, err := Load(write(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "vars.missing") {
		t.Fatalf("want undefined-variable error naming vars.missing, got %v", err)
	}
}

func TestVarsEscapeLiteral(t *testing.T) {
	yaml := strings.Replace(varsYAML, `image_tag: "${vars.tag}"`, `image_tag: "$${vars.tag}"`, 1)
	cfg, err := Load(write(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Services["web"].ImageTag; got != "${vars.tag}" {
		t.Errorf("image_tag = %q, want literal ${vars.tag}", got)
	}
}

func TestVarsNoRecursion(t *testing.T) {
	// A var value containing a reference is inserted verbatim, never
	// re-expanded: image_tag must NOT become "8010".
	// (The lenient pre-parse reads tag's raw value "${vars.port}"; the
	// single text pass inserts it into image_tag without re-scanning.)
	yaml := strings.Replace(varsYAML, `tag: "v42"`, `tag: "${vars.port}"`, 1)
	cfg, err := Load(write(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Services["web"].ImageTag; got != "${vars.port}" {
		t.Errorf("image_tag = %q, want un-expanded ${vars.port}", got)
	}
}

func TestVarsValueWithNewlineRejected(t *testing.T) {
	yaml := strings.Replace(varsYAML, `tag: "v42"`, "tag: \"v42\\ninjected: true\"", 1)
	_, err := Load(write(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "newline") {
		t.Fatalf("want newline rejection, got %v", err)
	}
}

func TestVarsStrictSchemaStillApplies(t *testing.T) {
	// A typo'd field is rejected even when the file uses vars.
	yaml := strings.Replace(varsYAML, "image_tag:", "image_tags:", 1)
	_, err := Load(write(t, yaml))
	if err == nil {
		t.Fatal("want unknown-field error, got nil")
	}
}

func TestNoVarsBlockStillWorks(t *testing.T) {
	// Existing configs without vars are untouched (regression guard).
	if _, err := Load(write(t, validYAML)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVarsFlowStyleRejectedWithClearError(t *testing.T) {
	// vars: { tag: v42 } on one line is not a block mapping; the extraction
	// regex requires a newline right after "vars:" so this silently yields
	// an empty vars block today, surfacing as a misleading undefined-var
	// error instead of naming the real problem.
	yaml := strings.Replace(varsYAML, "vars:\n  tag: \"v42\"\n  port: \"8010\"\n", `vars: { tag: v42, port: "8010" }`+"\n", 1)
	_, err := Load(write(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "block mapping") {
		t.Fatalf("want error naming block mapping requirement, got %v", err)
	}
}

func TestVarsBlockToleratesColumnZeroComment(t *testing.T) {
	// A full-line comment at column 0 between indented vars entries must
	// not terminate the block early and silently drop later keys.
	yaml := strings.Replace(varsYAML, "  tag: \"v42\"\n  port: \"8010\"\n", "  tag: \"v42\"\n# a column-0 comment\n  port: \"8010\"\n", 1)
	cfg, err := Load(write(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Services["web"].Readiness.Port; got != 8010 {
		t.Errorf("readiness.port = %d, want 8010 (later key after column-0 comment must still resolve)", got)
	}
}
