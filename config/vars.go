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
	if block := extractVarsBlock(raw); block != nil {
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

// varsBlockRe finds the top-level "vars:" key and everything indented under
// it, stopping at the next top-level key (a line starting at column 0) or
// end of input. Returns nil if there is no top-level vars: key.
var varsBlockRe = regexp.MustCompile(`(?m)^vars:[ \t]*\n((?:[ \t]+.*\n?|\n)*)`)

// extractVarsBlock returns just the "vars:\n<indented lines>" fragment of
// raw as a standalone, independently-parseable YAML document, or nil if raw
// has no top-level vars: key.
func extractVarsBlock(raw []byte) []byte {
	m := varsBlockRe.FindSubmatch(raw)
	if m == nil {
		return nil
	}
	return append([]byte("vars:\n"), m[1]...)
}
