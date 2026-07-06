package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// projectRe restricts project names to characters safe for shell paths and
// container/upstream names.
var projectRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type Config struct {
	Project  string             `yaml:"project"`
	Compose  string             `yaml:"compose"`
	Registry Registry           `yaml:"registry"`
	Target   Target             `yaml:"target"`
	Secrets  Secrets            `yaml:"secrets"`
	Services map[string]Service `yaml:"services"`
}
type Registry struct {
	Server string `yaml:"server"`
}
type Target struct {
	Host string `yaml:"host"` // "user@host"; empty = local exec
	Port int    `yaml:"port"` // 0 = 22
}
type Secrets struct {
	FromEnv []string `yaml:"from_env"`
}
type Service struct {
	ImageTag  string    `yaml:"image_tag"`
	Model     string    `yaml:"model"`
	Readiness Readiness `yaml:"readiness"`
	Cutover   Cutover   `yaml:"cutover"`
	Placement Placement `yaml:"placement"`
}
type Readiness struct {
	Type    string `yaml:"type"` // http|tcp|vllm|cmd
	Path    string `yaml:"path"`
	Port    int    `yaml:"port"`
	Timeout string `yaml:"timeout"` // Go duration, e.g. "90s"
}
type Cutover struct {
	Strategy string `yaml:"strategy"` // recreate|proxy
	Proxy    string `yaml:"proxy"`    // e.g. nginx-upstream
	Warmup   bool   `yaml:"warmup"`
}
type Placement struct {
	Type        string `yaml:"type"` // ""|none|gpu
	Pool        []int  `yaml:"pool"`
	VRAMMin     string `yaml:"vram_min"`
	OnNoFreeGPU string `yaml:"on_no_free_gpu"` // ""|fail|stop-old-first
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
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

func (c *Config) validate() error {
	if c.Project == "" {
		return fmt.Errorf("project is required")
	}
	// project is interpolated into host shell paths ($HOME/.dockrail/<project>/…)
	// and container/upstream names; restrict it to a safe charset so it cannot
	// word-split or inject.
	if !projectRe.MatchString(c.Project) {
		return fmt.Errorf("project %q must match %s", c.Project, projectRe.String())
	}
	if c.Compose == "" {
		return fmt.Errorf("compose is required")
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("at least one entry under services is required")
	}
	for name, s := range c.Services {
		// service names become host container/upstream names (<name>-blue) and
		// are interpolated into shell commands, so restrict them like project.
		if !projectRe.MatchString(name) {
			return fmt.Errorf("services.%s: name must match %s", name, projectRe.String())
		}
		if s.ImageTag == "" {
			return fmt.Errorf("services.%s: image_tag is required", name)
		}
		switch s.Readiness.Type {
		case "http", "tcp", "vllm", "cmd":
		default:
			return fmt.Errorf("services.%s: readiness.type must be http|tcp|vllm|cmd, got %q", name, s.Readiness.Type)
		}
		if s.Readiness.Timeout != "" {
			if _, err := time.ParseDuration(s.Readiness.Timeout); err != nil {
				return fmt.Errorf("services.%s: readiness.timeout: %w", name, err)
			}
		}
		switch s.Cutover.Strategy {
		case "recreate", "proxy":
		default:
			return fmt.Errorf("services.%s: cutover.strategy must be recreate|proxy, got %q", name, s.Cutover.Strategy)
		}
		// proxy is the nginx container name passed to `docker exec`; it is
		// interpolated into a shell command, so restrict its charset.
		if s.Cutover.Proxy != "" && !projectRe.MatchString(s.Cutover.Proxy) {
			return fmt.Errorf("services.%s: cutover.proxy must match %s", name, projectRe.String())
		}
		switch s.Placement.Type {
		case "", "none":
		case "gpu":
			if len(s.Placement.Pool) == 0 {
				return fmt.Errorf("services.%s: placement.pool is required for type gpu", name)
			}
			if s.Placement.VRAMMin == "" {
				return fmt.Errorf("services.%s: placement.vram_min is required for type gpu", name)
			}
			switch s.Placement.OnNoFreeGPU {
			case "", "fail", "stop-old-first":
			default:
				return fmt.Errorf("services.%s: placement.on_no_free_gpu must be fail|stop-old-first, got %q", name, s.Placement.OnNoFreeGPU)
			}
		default:
			return fmt.Errorf("services.%s: placement.type must be none|gpu, got %q", name, s.Placement.Type)
		}
	}
	return nil
}
