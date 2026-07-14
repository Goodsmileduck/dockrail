package fleet

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Project   string             `yaml:"project"`
	Compose   string             `yaml:"compose"`
	Registry  Registry           `yaml:"registry"`
	Scheduler Scheduler          `yaml:"scheduler"`
	Hosts     map[string]Host    `yaml:"hosts"`
	Backends  map[string]Backend `yaml:"backends"`
	Services  map[string]Service `yaml:"services"`
}

type Registry struct {
	Server string `yaml:"server"`
}

type Scheduler struct {
	Policy string `yaml:"policy"` // spread|binpack|first-fit; "" = default (spread)
}

type Host struct {
	SSH  string `yaml:"ssh"`  // "user@host"; empty = local exec
	Port int    `yaml:"port"` // 0 = 22
	GPUs []int  `yaml:"gpus"`
}

type Backend struct {
	Service   string    `yaml:"service"`
	ImageTag  string    `yaml:"image_tag"`
	Model     string    `yaml:"model"`
	Replicas  int       `yaml:"replicas"`
	Placement Placement `yaml:"placement"`
	Readiness Readiness `yaml:"readiness"`
}

type Placement struct {
	VRAMMin string   `yaml:"vram_min"`
	GPU     GPUSpec  `yaml:"gpu"`
	Pool    []string `yaml:"pool"`   // host names the scheduler may use for auto
	Policy  string   `yaml:"policy"` // per-backend override of scheduler.policy
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
	Service   string    `yaml:"service"`
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
	// Replicas defaults to 1 when unset (0), applied once here before validation
	// so validate() stays side-effect-free (mirrors config.RetainContainers).
	for name, b := range cfg.Backends {
		if b.Replicas == 0 {
			if n := len(b.Placement.GPU.Pins); n > 0 {
				b.Replicas = n // pins define the replica count
			} else {
				b.Replicas = 1
			}
			cfg.Backends[name] = b
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Packing-policy names, shared by config validation and the scheduler so the
// closed set lives in one place. PolicySpread is the default when unset.
const (
	PolicySpread   = "spread"
	PolicyBinpack  = "binpack"
	PolicyFirstFit = "first-fit"
)

// validPolicy allows the empty string (resolves to the default at schedule
// time) and the three known packing policies.
func validPolicy(p string) bool {
	switch p {
	case "", PolicySpread, PolicyBinpack, PolicyFirstFit:
		return true
	}
	return false
}

func (c *Config) validate() error {
	if c.Project == "" {
		return fmt.Errorf("project is required")
	}
	if !nameRe.MatchString(c.Project) {
		return fmt.Errorf("project %q must match %s", c.Project, nameRe.String())
	}
	if len(c.Hosts) == 0 {
		return fmt.Errorf("at least one entry under hosts is required")
	}
	if len(c.Backends) > 0 || len(c.Services) > 0 {
		if c.Compose == "" {
			return fmt.Errorf("compose is required when backends or services are declared")
		}
	}
	if !validPolicy(c.Scheduler.Policy) {
		return fmt.Errorf("scheduler.policy must be spread|binpack|first-fit, got %q", c.Scheduler.Policy)
	}
	// gpuIndex[host] is the set of declared GPU indices, used to validate pins.
	gpuIndex := make(map[string]map[int]bool, len(c.Hosts))
	for name, h := range c.Hosts {
		if err := validName("hosts", name); err != nil {
			return err
		}
		if h.SSH == "" {
			return fmt.Errorf("hosts.%s: ssh is required", name)
		}
		set := make(map[int]bool, len(h.GPUs))
		for _, g := range h.GPUs {
			set[g] = true
		}
		gpuIndex[name] = set
	}
	for name, b := range c.Backends {
		if err := validName("backends", name); err != nil {
			return err
		}
		if b.Service == "" {
			return fmt.Errorf("backends.%s: service is required", name)
		}
		if b.ImageTag == "" {
			return fmt.Errorf("backends.%s: image_tag is required", name)
		}
		if b.Replicas < 1 {
			return fmt.Errorf("backends.%s: replicas must be >= 1", name)
		}
		if !validPolicy(b.Placement.Policy) {
			return fmt.Errorf("backends.%s: placement.policy must be spread|binpack|first-fit, got %q", name, b.Placement.Policy)
		}
		if !b.Placement.GPU.Auto && len(b.Placement.GPU.Pins) == 0 {
			return fmt.Errorf("backends.%s: placement.gpu must be \"auto\" or a list of pins", name)
		}
		if pins := b.Placement.GPU.Pins; len(pins) > 0 {
			if b.Replicas != len(pins) {
				return fmt.Errorf("backends.%s: replicas (%d) must equal the number of gpu pins (%d)", name, b.Replicas, len(pins))
			}
			seen := make(map[string]bool, len(pins))
			for _, pin := range pins {
				if seen[pin] {
					return fmt.Errorf("backends.%s: duplicate gpu pin %q", name, pin)
				}
				seen[pin] = true
			}
		}
		p := b.Placement
		if p.GPU.Auto {
			if len(p.Pool) == 0 {
				return fmt.Errorf("backends.%s: placement.pool is required when gpu: auto", name)
			}
			for _, host := range p.Pool {
				if _, ok := c.Hosts[host]; !ok {
					return fmt.Errorf("backends.%s: placement.pool references unknown host %q", name, host)
				}
			}
		}
		for _, pin := range p.GPU.Pins {
			host, idx, err := ParsePin(pin)
			if err != nil {
				return fmt.Errorf("backends.%s: %w", name, err)
			}
			if _, ok := gpuIndex[host]; !ok {
				return fmt.Errorf("backends.%s: gpu pin %q references unknown host", name, pin)
			}
			if !gpuIndex[host][idx] {
				return fmt.Errorf("backends.%s: gpu pin %q: host %q has no gpu %d", name, pin, host, idx)
			}
		}
	}
	for name, s := range c.Services {
		if err := validName("services", name); err != nil {
			return err
		}
		if s.Service == "" {
			return fmt.Errorf("services.%s: service is required", name)
		}
		if s.Host == "" {
			return fmt.Errorf("services.%s: host is required", name)
		}
		if _, ok := c.Hosts[s.Host]; !ok {
			return fmt.Errorf("services.%s: host %q is not a declared host", name, s.Host)
		}
		for _, u := range s.Uses {
			if _, ok := c.Backends[u.Backend]; !ok {
				return fmt.Errorf("services.%s: uses references unknown backend %q", name, u.Backend)
			}
			switch u.Wiring.Strategy {
			case "nginx-upstream":
			case "env-list":
				if u.Wiring.Var == "" {
					return fmt.Errorf("services.%s: env-list wiring requires var", name)
				}
			default:
				return fmt.Errorf("services.%s: wiring.strategy must be nginx-upstream|env-list, got %q", name, u.Wiring.Strategy)
			}
		}
	}
	return nil
}

// validName rejects a map-entry name (host/backend/service) that is not
// shell-safe; kind is the block name used in the error (e.g. "hosts").
func validName(kind, name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%s.%s: name must match %s", kind, name, nameRe.String())
	}
	return nil
}

// ParsePin splits a "host:index" GPU pin.
func ParsePin(pin string) (host string, idx int, err error) {
	i := strings.LastIndex(pin, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("gpu pin %q must be host:index", pin)
	}
	host = pin[:i]
	idx, err = strconv.Atoi(pin[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("gpu pin %q: bad index: %w", pin, err)
	}
	return host, idx, nil
}
