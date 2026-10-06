// Package proxy is the one component that holds credentials. It exposes
// the connector slots over HTTP to the sandbox, enforces policy on every
// call, writes an audit line per call, and talks to the real backends.
// The model never sees any of this; it sees tool results.
package proxy

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is proxy.yaml: bound once at install time, inside the boundary.
type Config struct {
	Listen string `yaml:"listen"` // default 127.0.0.1:8471 (an explicit address: ":8080" can silently bind only IPv6 when another process holds IPv4)
	Fleet  Fleet  `yaml:"fleet"`
	Slots  Slots  `yaml:"slots"`
	Limits Limits `yaml:"limits"`
	// Token is how the sandbox authenticates to the proxy. The real boundary
	// is the NetworkPolicy; this keeps a stray in-cluster client out.
	// A source, like every credential here: env:NAME or keychain:SERVICE.
	Token string `yaml:"token"`
}

// Fleet names the clusters identifiers may refer to and how metrics label them.
type Fleet struct {
	ClusterLabel string    `yaml:"cluster_label" json:"cluster_label"` // e.g. "cluster"
	Clusters     []Cluster `yaml:"clusters" json:"clusters"`
}

// Cluster is one cluster the fleet knows.
type Cluster struct {
	ID     string            `yaml:"id" json:"id"`
	Env    string            `yaml:"env" json:"env"`
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

// Slots binds each connector to a backend.
type Slots struct {
	Metrics *MetricsSlot `yaml:"metrics,omitempty"`
	SCM     *SCMSlot     `yaml:"scm,omitempty"`
}

// MetricsSlot is a PromQL-speaking backend.
type MetricsSlot struct {
	Kind     string `yaml:"kind"`     // promql
	Endpoint string `yaml:"endpoint"` // base URL of the HTTP API
	Auth     string `yaml:"auth"`     // none | env:NAME | keychain:SERVICE (sent as a bearer token)
}

// SCMSlot is a source-control backend.
type SCMSlot struct {
	Kind            string   `yaml:"kind"` // github
	Org             string   `yaml:"org"`
	Auth            string   `yaml:"auth"`
	AllowBotCommits []string `yaml:"allow_bot_commits,omitempty"`
	// API is the REST base, for tests; default https://api.github.com.
	API string `yaml:"api,omitempty"`
}

// Limits bound every call.
type Limits struct {
	QueryRangeMax      time.Duration `yaml:"query_range_max"`       // default 7d
	ResultBytesMax     int           `yaml:"result_bytes_max"`      // default 256 KiB
	CallsPerSessionMax int           `yaml:"calls_per_session_max"` // default 60: past it, tools answer "unavailable" and the agents must stop exploring
	TarballBytesMax    int64         `yaml:"tarball_bytes_max"`     // default 200 MiB
}

// Load reads proxy.yaml and applies defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.defaults()
	return &c, c.validate()
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8471"
	}
	if c.Fleet.ClusterLabel == "" {
		c.Fleet.ClusterLabel = "cluster"
	}
	if c.Limits.QueryRangeMax == 0 {
		c.Limits.QueryRangeMax = 7 * 24 * time.Hour
	}
	if c.Limits.ResultBytesMax == 0 {
		c.Limits.ResultBytesMax = 256 << 10
	}
	if c.Limits.CallsPerSessionMax == 0 {
		c.Limits.CallsPerSessionMax = 60
	}
	if c.Limits.TarballBytesMax == 0 {
		c.Limits.TarballBytesMax = 200 << 20
	}
	if c.Slots.SCM != nil && c.Slots.SCM.API == "" {
		c.Slots.SCM.API = "https://api.github.com"
	}
}

func (c *Config) validate() error {
	if c.Slots.Metrics != nil {
		if c.Slots.Metrics.Kind != "promql" {
			return fmt.Errorf("slots.metrics.kind %q: only promql is implemented", c.Slots.Metrics.Kind)
		}
		if c.Slots.Metrics.Endpoint == "" {
			return fmt.Errorf("slots.metrics.endpoint is required")
		}
	}
	if c.Slots.SCM != nil {
		if c.Slots.SCM.Kind != "github" {
			return fmt.Errorf("slots.scm.kind %q: only github is implemented", c.Slots.SCM.Kind)
		}
		if c.Slots.SCM.Org == "" {
			return fmt.Errorf("slots.scm.org is required")
		}
	}
	for _, cl := range c.Fleet.Clusters {
		if cl.ID == "" {
			return fmt.Errorf("fleet.clusters: every cluster needs an id")
		}
	}
	return nil
}

// Credential resolves a credential source to its value. Sources, never
// values, appear in configuration: "none", "env:NAME", "keychain:SERVICE"
// (macOS, the operator's own Keychain, for the laptop deployment).
func Credential(source string) (string, error) {
	switch {
	case source == "" || source == "none":
		return "", nil
	case strings.HasPrefix(source, "env:"):
		v := os.Getenv(strings.TrimPrefix(source, "env:"))
		if v == "" {
			return "", fmt.Errorf("credential %s: variable is empty", source)
		}
		return v, nil
	case strings.HasPrefix(source, "keychain:"):
		svc := strings.TrimPrefix(source, "keychain:")
		out, err := exec.Command("security", "find-generic-password", "-a", os.Getenv("USER"), "-s", svc, "-w").Output()
		if err != nil {
			return "", fmt.Errorf("credential %s: %w", source, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	return "", fmt.Errorf("credential source %q: want none, env:NAME or keychain:SERVICE", source)
}
