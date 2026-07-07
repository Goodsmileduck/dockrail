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

// varsBlockRe finds the top-level "vars:" key and everything indented under
// it, stopping at the next top-level key (a line starting at column 0 with
// non-blank, non-comment content) or end of input. Blank lines and full-line
// comments (whether indented or at column 0) inside the block are tolerated
// and do not terminate the match early. Returns nil if there is no top-level
// vars: key declared as a block mapping.
var varsBlockRe = regexp.MustCompile(`(?m)^vars:[ \t]*\n((?:[ \t]+.*\n?|[ \t]*#.*\n?|[ \t]*\n)*)`)

// flowStyleVarsRe detects a top-level "vars:" key followed by content on the
// same line (e.g. "vars: { tag: v42 }" or "vars: [1, 2]") rather than a
// newline. A trailing same-line comment (e.g. "vars: # comment") is not
// flow-style — the mapping still follows as indented lines below.
var flowStyleVarsRe = regexp.MustCompile(`(?m)^vars:[ \t]+[^ \t\n#].*$`)

// extractVarsBlock accepts only a top-level "vars:" key declared as a block
// mapping — "vars:" alone on its line, followed by "name: value" entries
// indented beneath it (blank lines and full-line comments, indented or at
// column 0, are allowed within the block). It returns that fragment as a
// standalone, independently-parseable "vars:\n<indented lines>" YAML
// document, or nil if raw has no top-level vars: key. A flow-style
// declaration on the same line (e.g. "vars: { tag: v42 }") is rejected with
// an explicit error rather than silently yielding an empty vars block.
func extractVarsBlock(raw []byte) ([]byte, error) {
	if flowStyleVarsRe.Match(raw) {
		return nil, fmt.Errorf(`vars: must be a block mapping (one "name: value" per indented line)`)
	}
	m := varsBlockRe.FindSubmatch(raw)
	if m == nil {
		return nil, nil
	}
	return append([]byte("vars:\n"), m[1]...), nil
}
