package fleet

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Project  string             `yaml:"project"`
	Registry Registry           `yaml:"registry"`
	Hosts    map[string]Host    `yaml:"hosts"`
	Backends map[string]Backend `yaml:"backends"`
	Services map[string]Service `yaml:"services"`
}

type Registry struct {
	Server string `yaml:"server"`
}

type Host struct {
	SSH  string `yaml:"ssh"`  // "user@host"; empty = local exec
	Port int    `yaml:"port"` // 0 = 22
	GPUs []int  `yaml:"gpus"`
}

type Backend struct {
	ImageTag  string    `yaml:"image_tag"`
	Model     string    `yaml:"model"`
	Replicas  int       `yaml:"replicas"`
	Placement Placement `yaml:"placement"`
	Readiness Readiness `yaml:"readiness"`
}

type Placement struct {
	VRAMMin string   `yaml:"vram_min"`
	GPU     GPUSpec  `yaml:"gpu"`
	Pool    []string `yaml:"pool"` // host names the scheduler may use for auto
}

// GPUSpec is polymorphic: the scalar `auto` (scheduler picks) or a sequence of
// "host:index" pins.
type GPUSpec struct {
	Auto bool
	Pins []string
}

func (g *GPUSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Value != "auto" {
			return fmt.Errorf("gpu: scalar must be \"auto\", got %q", value.Value)
		}
		g.Auto = true
		return nil
	case yaml.SequenceNode:
		var pins []string
		if err := value.Decode(&pins); err != nil {
			return fmt.Errorf("gpu: %w", err)
		}
		g.Pins = pins
		return nil
	default:
		return fmt.Errorf("gpu: must be \"auto\" or a list of host:index pins")
	}
}

type Use struct {
	Backend string `yaml:"backend"`
	Wiring  Wiring `yaml:"wiring"`
}

type Wiring struct {
	Strategy string `yaml:"strategy"` // nginx-upstream|env-list
	Var      string `yaml:"var"`      // env var name for env-list
}

type Service struct {
	Host      string    `yaml:"host"`
	ImageTag  string    `yaml:"image_tag"`
	Uses      []Use     `yaml:"uses"`
	Readiness Readiness `yaml:"readiness"`
	Cutover   Cutover   `yaml:"cutover"`
}

type Readiness struct {
	Type    string `yaml:"type"`
	Path    string `yaml:"path"`
	Port    int    `yaml:"port"`
	Timeout string `yaml:"timeout"`
}

type Cutover struct {
	Strategy string `yaml:"strategy"` // recreate|proxy
	Warmup   bool   `yaml:"warmup"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fleet config: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error { return nil }
