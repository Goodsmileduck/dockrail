package config

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// varRefRe matches either the escape sequence $${ or a variable reference
// ${vars.name}. Anything else (including other ${...} forms) is left alone —
// deploy.yml has no env interpolation by design; compose handles its own.
var varRefRe = regexp.MustCompile(`\$\$\{|\$\{vars\.([A-Za-z0-9_-]+)\}`)

// interpolate performs a single substitution pass over the raw deploy.yml
// text: ${vars.name} is replaced with the value from the top-level vars:
// block, $${ yields a literal ${. Values are inserted verbatim — never
// re-scanned — so there is no recursion. Referencing an undefined variable
// is a hard error; declaring an unused one is fine.
func interpolate(raw []byte) ([]byte, error) {
	// Lenient pre-parse: pull out just the vars block as its own YAML
	// fragment, rather than decoding the whole document. Unsubstituted
	// ${vars.name} references elsewhere in the file (e.g. unquoted inside a
	// flow mapping like `port: ${vars.port}`) are not valid YAML on their
	// own, so the full document can't be parsed until after substitution —
	// only the vars: block itself needs to be valid at this stage.
	var head struct {
		Vars map[string]string `yaml:"vars"`
	}
	block, err := extractVarsBlock(raw)
	if err != nil {
		return nil, err
	}
	if block != nil {
		if err := yaml.Unmarshal(block, &head); err != nil {
			return nil, fmt.Errorf("parse vars: %w", err)
		}
	}
	// Values are spliced into YAML source text; a newline could inject
	// sibling keys, so reject it outright.
	for k, v := range head.Vars {
		if strings.Contains(v, "\n") {
			return nil, fmt.Errorf("vars.%s: value must not contain a newline", k)
		}
	}
	var missing []string
	out := varRefRe.ReplaceAllStringFunc(string(raw), func(m string) string {
		if m == "$${" {
			return "${"
		}
		name := varRefRe.FindStringSubmatch(m)[1]
		v, ok := head.Vars[name]
		if !ok {
			missing = append(missing, "vars."+name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("undefined variable(s): %s", strings.Join(missing, ", "))
	}
	return []byte(out), nil
}

// varsBlockRe finds the top-level "vars:" key. Group 1 captures whatever
// follows on the same line (empty or a comment for a block mapping, content
// for a flow-style declaration); group 2 captures the block body — every
// following line that is indented, blank, or a full-line comment (indented or
// at column 0), stopping at the next top-level key or end of input.
var varsBlockRe = regexp.MustCompile(`(?m)^vars:([^\n]*)(?:\n((?:[ \t]+.*\n?|[ \t]*#.*\n?|[ \t]*\n)*))?`)

// extractVarsBlock accepts only a top-level "vars:" key declared as a block
// mapping — "vars:" alone on its line (a trailing same-line comment is fine),
// followed by "name: value" entries indented beneath it. It returns that
// fragment as a standalone, independently-parseable "vars:\n<indented lines>"
// YAML document, or nil if raw has no top-level vars: key. A flow-style
// declaration on the same line (e.g. "vars: { tag: v42 }") is rejected with
// an explicit error rather than silently yielding an empty vars block.
func extractVarsBlock(raw []byte) ([]byte, error) {
	m := varsBlockRe.FindSubmatch(raw)
	if m == nil {
		return nil, nil
	}
	if rest := strings.TrimLeft(string(m[1]), " \t"); rest != "" && !strings.HasPrefix(rest, "#") {
		return nil, fmt.Errorf(`vars: must be a block mapping (one "name: value" per indented line)`)
	}
	return append([]byte("vars:\n"), m[2]...), nil
}
