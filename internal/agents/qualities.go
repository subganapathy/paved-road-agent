package agents

import (
	_ "embed"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// qualitiesYAML is the program: the properties a good change shows, each
// closing one way a change causes an incident, instantiated per change by
// the lead. It is embedded so the binary carries exactly the reviewed
// file, and tests check it names no product.
//
//go:embed qualities.yaml
var qualitiesYAML []byte

// Program is the parsed qualities file.
type Program struct {
	Version   int       `yaml:"version"`
	Preamble  string    `yaml:"preamble"`
	Qualities []Quality `yaml:"qualities"`
}

// Quality is one property of a good change.
type Quality struct {
	ID              string            `yaml:"id"`
	Name            string            `yaml:"name"`
	Prevents        string            `yaml:"prevents"` // the incident it closes the path to
	Definition      string            `yaml:"definition"`
	Instantiate     string            `yaml:"instantiate"`
	Evidence        map[string]string `yaml:"evidence"` // class → where it lives
	Severity        string            `yaml:"severity"`
	Proportionality string            `yaml:"proportionality"`
}

// QualityIDs is the order the report lists them.
var QualityIDs = []string{"correct", "reversible", "within-budget", "stable-under-failure", "progressively-delivered", "available", "secure", "observable", "proven"}

// LoadProgram parses the embedded file and checks its shape.
func LoadProgram() (*Program, error) {
	var p Program
	if err := yaml.Unmarshal(qualitiesYAML, &p); err != nil {
		return nil, fmt.Errorf("qualities.yaml: %w", err)
	}
	if strings.TrimSpace(p.Preamble) == "" {
		return nil, fmt.Errorf("qualities.yaml: empty preamble")
	}
	if len(p.Qualities) != len(QualityIDs) {
		return nil, fmt.Errorf("qualities.yaml: %d qualities, want %d", len(p.Qualities), len(QualityIDs))
	}
	for i, q := range p.Qualities {
		if q.ID != QualityIDs[i] {
			return nil, fmt.Errorf("qualities.yaml: quality %d is %q, want %q", i, q.ID, QualityIDs[i])
		}
		for field, v := range map[string]string{"prevents": q.Prevents, "definition": q.Definition, "instantiate": q.Instantiate, "severity": q.Severity, "proportionality": q.Proportionality} {
			if strings.TrimSpace(v) == "" {
				return nil, fmt.Errorf("qualities.yaml: %s has empty %s", q.ID, field)
			}
		}
		if len(q.Evidence) == 0 {
			return nil, fmt.Errorf("qualities.yaml: %s has no evidence classes", q.ID)
		}
	}
	return &p, nil
}

// Render turns the program into the lead's system prompt text.
func (p *Program) Render() string {
	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(p.Preamble))
	sb.WriteString("\n\n# The properties of a good change\n")
	for _, q := range p.Qualities {
		fmt.Fprintf(&sb, "\n## %s (%s)\n\n", q.Name, q.ID)
		fmt.Fprintf(&sb, "The incident it prevents: %s\n\n", strings.TrimSpace(q.Prevents))
		fmt.Fprintf(&sb, "Definition: %s\n\n", strings.TrimSpace(q.Definition))
		fmt.Fprintf(&sb, "How to instantiate it for a change:\n%s\n", strings.TrimSpace(q.Instantiate))
		sb.WriteString("\nWhere the evidence lives:\n")
		for _, class := range []string{"code", "intent", "actual", "behaviour"} {
			if v, ok := q.Evidence[class]; ok {
				fmt.Fprintf(&sb, "- %s: %s\n", class, strings.TrimSpace(v))
			}
		}
		fmt.Fprintf(&sb, "\nSeverity:\n%s\n", strings.TrimSpace(q.Severity))
		fmt.Fprintf(&sb, "\nProportionality: %s\n", strings.TrimSpace(q.Proportionality))
	}
	return sb.String()
}

// ProductNames is the denylist: words that must never appear in the
// program or in any rendered prompt, because the model is expected to
// discover products, not be told about them. Kubernetes is the one
// assumption and is allowed.
var ProductNames = []string{
	"istio", "linkerd", "envoy", "consul", "spire", "app mesh",
	"argo", "flux", "flagger", "spinnaker", "helm", "kustomize",
	"calico", "cilium", "kindnet", "weave",
	"gatekeeper", "kyverno", "opa",
	"keda", "karpenter",
	"prometheus", "thanos", "mimir", "victoria", "datadog", "grafana", "loki", "kube-state-metrics",
	"pagerduty", "opsgenie",
	"terraform", "pulumi", "crossplane",
	"github", "gitlab",
	"aws", "gcp", "azure", "gke", "eks", "aks",
}

// FindProductNames reports which denylisted names appear in text, as
// whole words, case-insensitively.
func FindProductNames(text string) []string {
	lower := strings.ToLower(text)
	var hits []string
	for _, name := range ProductNames {
		idx := 0
		for {
			i := strings.Index(lower[idx:], name)
			if i < 0 {
				break
			}
			start, end := idx+i, idx+i+len(name)
			before := start == 0 || !isWord(lower[start-1])
			after := end == len(lower) || !isWord(lower[end])
			if before && after {
				hits = append(hits, name)
				break
			}
			idx = end
		}
	}
	return hits
}

func isWord(b byte) bool {
	return b == '-' || b == '_' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}
