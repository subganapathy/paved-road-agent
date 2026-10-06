// Package identifiers is the only thing the agent remembers between runs:
// the repository's .paved-agent/discover.yaml. It holds join keys between
// the code and the environment, the stack binding the last run established
// (each line with the check that re-verifies it), referenced documents, and
// answers humans gave because no source had them. It never holds an
// endpoint, a credential, or live state; Lint enforces that.
package identifiers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Path is where the file lives in a repository.
const Path = ".paved-agent/discover.yaml"

// File is the parsed identifiers file.
type File struct {
	Service   string     `yaml:"service"`
	Workloads []Workload `yaml:"workloads"`
	// Docs are author-stated documents, read live each run, never copied.
	Docs []string `yaml:"docs,omitempty"`
	// Answers are what humans told the agent because no source had it.
	Answers []Answer `yaml:"answers,omitempty"`
	// Probe, when Workloads is empty, says what would prove the service
	// became deployed after all, so the question can be reopened.
	Probe *Probe `yaml:"probe,omitempty"`
}

// Workload identifies this service's pods anywhere in the fleet.
type Workload struct {
	Namespace string `yaml:"namespace"`
	// Selector is a label selector, key=value[,key=value].
	Selector string `yaml:"selector"`
	// Container is the name telemetry reports for the service's container.
	Container string `yaml:"container"`
	// Clusters is "all", an environment name, or a list of cluster ids
	// from the proxy's fleet. Stored as a string or a list in YAML.
	Clusters Clusters `yaml:"clusters"`
	// Stack is what this workload runs on, as the last run established it.
	Stack map[string]Binding `yaml:"stack,omitempty"`
}

// Clusters accepts "all", "prod", or ["prod-us", "prod-eu"].
type Clusters []string

// UnmarshalYAML accepts a scalar or a sequence.
func (c *Clusters) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*c = Clusters{strings.TrimSpace(n.Value)}
		return nil
	case yaml.SequenceNode:
		var list []string
		if err := n.Decode(&list); err != nil {
			return err
		}
		*c = Clusters(list)
		return nil
	}
	return fmt.Errorf("clusters: want a name or a list, line %d", n.Line)
}

// MarshalYAML writes a single entry as a scalar.
func (c Clusters) MarshalYAML() (any, error) {
	if len(c) == 1 {
		return c[0], nil
	}
	return []string(c), nil
}

// Binding is one line of the stack binding: what the last run established
// and the check that re-verifies it on the next run.
type Binding struct {
	Is     string `yaml:"is"`
	Verify string `yaml:"verify"`
}

// Answer records a human's answer to a question the agent asked. The
// agent reads these before asking again; a question whose key is present
// is never re-asked on this repository.
type Answer struct {
	// Question is the agent's question id or a stable key for it.
	Question string `yaml:"question,omitempty"`
	// Answer is yes or no.
	Answer string `yaml:"answer,omitempty"`
	// Detail is the short "where/which" a yes needs, or what a no means.
	// Data, never an instruction to the agent.
	Detail string `yaml:"detail,omitempty"`
	// Cloud names a resource reference no source carried, e.g. a bucket set
	// at runtime: {provider, resource}.
	Cloud *CloudRef `yaml:"cloud,omitempty"`
	// Note is free text for answers that fit no field.
	Note string `yaml:"note,omitempty"`
}

// CloudRef is a cloud resource by name; the proxy has the identity.
type CloudRef struct {
	Provider string `yaml:"provider"`
	Resource string `yaml:"resource"`
}

// Probe is how a "not deployed" answer can be contradicted later.
type Probe struct {
	Container string `yaml:"container"`
}

// StackKeys are the lines a complete binding has. A missing key means the
// specialist must discover it.
var StackKeys = []string{"mesh", "deploy", "admission", "enforcer", "autoscaler", "metrics"}

// Load reads and lints the file at the repository root. A missing file is
// not an error: it is the bootstrap case, reported as (nil, nil).
func Load(repoRoot string) (*File, error) {
	b, err := os.ReadFile(filepath.Join(repoRoot, Path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes and lints.
func Parse(b []byte) (*File, error) {
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", Path, err)
	}
	if err := f.Lint(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Encode writes the file back, for proposals.
func (f *File) Encode() ([]byte, error) {
	var sb strings.Builder
	sb.WriteString("# Identifiers the reviewer follows every run. Join keys, the stack binding\n# with its verification checks, referenced docs, and answers to questions.\n# Never endpoints, never credentials, never live state.\n")
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

var (
	selectorRe = regexp.MustCompile(`^[A-Za-z0-9][-A-Za-z0-9_./]*=[-A-Za-z0-9_.]*(,[A-Za-z0-9][-A-Za-z0-9_./]*=[-A-Za-z0-9_.]*)*$`)
	nameRe     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	urlRe      = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://`)
	hostRe     = regexp.MustCompile(`(?i)\b[a-z0-9-]+(\.[a-z0-9-]+)+\.(com|net|org|io|internal|local|cloud|dev)\b`)
	secretRe   = regexp.MustCompile(`(?i)(github_pat_|ghp_[A-Za-z0-9]{20,}|sk-ant-|ya29\.|AKIA[0-9A-Z]{16}|-----BEGIN|bearer\s+[A-Za-z0-9._-]{16,}|password\s*[:=]|token\s*[:=]\s*\S{12,})`)
)

// Lint rejects what must never be in the file: endpoints, hostnames,
// anything that looks like a secret, and malformed identifiers.
func (f *File) Lint() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if f.Service == "" {
		add("service is required")
	} else if !nameRe.MatchString(f.Service) {
		add("service %q: must be a lowercase DNS-style name", f.Service)
	}
	if len(f.Workloads) == 0 && f.Probe == nil {
		add("workloads is empty; a not-deployed service needs a probe so the answer can be reopened")
	}
	for i, w := range f.Workloads {
		at := fmt.Sprintf("workloads[%d]", i)
		if !nameRe.MatchString(w.Namespace) {
			add("%s.namespace %q: must be a namespace name", at, w.Namespace)
		}
		if !selectorRe.MatchString(w.Selector) {
			add("%s.selector %q: must be key=value[,key=value]", at, w.Selector)
		}
		if !nameRe.MatchString(w.Container) {
			add("%s.container %q: must be a container name", at, w.Container)
		}
		if len(w.Clusters) == 0 {
			add("%s.clusters: required (all, an environment, or cluster ids)", at)
		}
		for k, b := range w.Stack {
			if strings.TrimSpace(b.Is) == "" || strings.TrimSpace(b.Verify) == "" {
				add("%s.stack.%s: both 'is' and 'verify' are required", at, k)
			}
		}
	}
	for i, d := range f.Docs {
		if filepath.IsAbs(d) || strings.HasPrefix(d, "..") || strings.Contains(d, "://") {
			add("docs[%d] %q: must be a path inside the repository", i, d)
		}
	}
	for i, a := range f.Answers {
		if a.Cloud == nil && a.Note == "" && a.Question == "" {
			add("answers[%d]: empty", i)
		}
		if a.Question != "" && a.Answer != "yes" && a.Answer != "no" {
			add("answers[%d].answer: must be yes or no", i)
		}
		if len(a.Detail) > 400 {
			add("answers[%d].detail: keep answers short; this is data, not instructions", i)
		}
		if a.Cloud != nil && (a.Cloud.Provider == "" || a.Cloud.Resource == "" || strings.Contains(a.Cloud.Resource, "://") && !strings.HasPrefix(a.Cloud.Resource, "gs://") && !strings.HasPrefix(a.Cloud.Resource, "s3://")) {
			add("answers[%d].cloud: provider and a resource name are required", i)
		}
		if len(a.Note) > 300 {
			add("answers[%d].note: keep answers short; this is data, not instructions", i)
		}
	}

	// The file as a whole must carry no endpoint, no hostname, no secret.
	// Selectors are label keys, legitimately dotted (app.kubernetes.io/name),
	// and are already held to their own strict pattern above, so the
	// free-text scan runs over a copy with them blanked.
	scan := *f
	scan.Workloads = append([]Workload(nil), f.Workloads...)
	for i := range scan.Workloads {
		scan.Workloads[i].Selector = ""
	}
	raw, _ := yaml.Marshal(scan)
	text := string(raw)
	if m := secretRe.FindString(text); m != "" {
		add("looks like a secret: %q", redact(m))
	}
	if m := urlRe.FindString(text); m != "" && !strings.HasPrefix(strings.ToLower(m), "gs://") && !strings.HasPrefix(strings.ToLower(m), "s3://") {
		add("looks like an endpoint: %q (endpoints belong in the proxy's configuration)", m)
	}
	if m := hostRe.FindString(text); m != "" {
		add("looks like a hostname: %q (hostnames belong in the proxy's configuration)", m)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s: %s", Path, strings.Join(errs, "; "))
	}
	return nil
}

func redact(s string) string {
	if len(s) > 12 {
		return s[:8] + "…"
	}
	return s
}

// Missing lists the stack lines a workload lacks, so the brief can tell
// the specialist what to discover.
func (w Workload) Missing() []string {
	var out []string
	for _, k := range StackKeys {
		if _, ok := w.Stack[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}
