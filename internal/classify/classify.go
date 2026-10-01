// Package classify decides which aspects of the environment a change can
// touch. It is deterministic on purpose: the aspects decide which
// specialists study the environment, so a wrong classification wastes the
// whole assessment. Rules look at paths and at the lines a change adds or
// removes; a model is consulted only for files no rule understands.
package classify

import (
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/subganapathy/paved-road-agent/internal/change"
)

// Aspect is one dimension of impact, with one specialist per aspect.
type Aspect string

const (
	NewCall     Aspect = "new-call"     // a service starts calling another
	Contract    Aspect = "contract"     // the API contract changes
	Authz       Aspect = "authz"        // who may call which methods
	Network     Aspect = "network"      // who may connect, and to what
	Traffic     Aspect = "traffic"      // routing, retries, outlier detection
	Capacity    Aspect = "capacity"     // replicas, autoscaling, limits
	Rollout     Aspect = "rollout"      // canary steps, analysis, disruption, images
	Terraform   Aspect = "terraform"    // an infrastructure plan
	NewResource Aspect = "new-resource" // a plan creates something
	Deletion    Aspect = "deletion"     // a plan destroys or replaces something
	Image       Aspect = "image"        // how the container is built and runs
	Delivery    Aspect = "delivery"     // CI and promotion
)

// All is the closed set, in the order the report lists them.
var All = []Aspect{NewCall, Contract, Authz, Network, Traffic, Capacity, Rollout, Terraform, NewResource, Deletion, Image, Delivery}

// Reason records why an aspect was selected, so a reviewer can disagree with
// the rule rather than with a verdict.
type Reason struct {
	Aspect Aspect `json:"aspect"`
	File   string `json:"file"`
	Rule   string `json:"rule"`
}

// Result is the classification of one change.
type Result struct {
	Aspects []Aspect `json:"aspects"`
	Reasons []Reason `json:"reasons"`
	// Unclassified lists files no rule understood. They are not ignored:
	// the coordinator asks the model what they touch.
	Unclassified []string `json:"unclassified,omitempty"`
	// NoImpact is true when the rules found nothing that runs changed:
	// no aspect, nothing unclassified, no plan. The one confident "no
	// impact".
	NoImpact bool `json:"no_impact"`
}

var (
	dialRE    = regexp.MustCompile(`\.Dial\(|grpc\.NewClient\(|http\.NewRequest\(`)
	clientMod = regexp.MustCompile(`^\s*(require\s+)?"?github\.com/[^/\s]+/[^/\s]+/client\b`)
	rolloutKV = regexp.MustCompile(`^\s*-?\s*(setWeight|pause|duration|analysis|templateName|steps|strategy|canary|maxSurge|maxUnavailable)\b`)
	capKV     = regexp.MustCompile(`^\s*(replicas|minReplicaCount|maxReplicaCount|threshold|cpu|memory|limits|requests|inflightPerPod|minReplicas|maxReplicas)\b`)
	docExt    = map[string]bool{".md": true, ".txt": true, ".rst": true}
)

// Classify applies the rules to a change.
func Classify(c change.Change) Result {
	var r Result
	add := func(a Aspect, file, rule string) {
		if !slices.Contains(r.Aspects, a) {
			r.Aspects = append(r.Aspects, a)
		}
		r.Reasons = append(r.Reasons, Reason{Aspect: a, File: file, Rule: rule})
	}
	for _, f := range c.Files {
		p := f.Path
		base := path.Base(p)
		isDoc := docExt[path.Ext(p)] || strings.HasPrefix(p, "docs/")
		matched := isDoc
		mark := func(a Aspect, rule string) { add(a, p, rule); matched = true }
		added, removed := code(f.AddedLines()), code(f.RemovedLines())
		changed := append(slices.Clone(added), removed...)

		switch {
		case isDoc:
			// documentation: no aspect
		case len(changed) == 0 && f.Status == change.Modified:
			matched = true // only comments or blank lines moved
		case strings.HasPrefix(p, ".platform/") || p == "deploy/base/kustomization.yaml" || base == "Makefile" || base == ".gitignore" || strings.HasPrefix(base, "buf."):
			matched = true // bookkeeping the platform regenerates; the files it lists are classified on their own
		case strings.HasSuffix(p, ".proto"):
			mark(Contract, "a .proto file is the API contract")
		case strings.HasSuffix(p, ".pb.go") || strings.HasPrefix(p, "client/"):
			mark(Contract, "generated stubs and the canonical client follow the contract")
		case strings.HasPrefix(p, "e2e/"):
			mark(Delivery, "the e2e is how the change is proven")
		case base == "service.yaml":
			for _, a := range serviceYAML(f) {
				mark(a.Aspect, a.Rule)
			}
			matched = true
		case strings.HasPrefix(p, "deploy/base/mesh/authz-") || strings.HasPrefix(p, "deploy/base/mesh/peer-authentication"):
			mark(Authz, "an AuthorizationPolicy or PeerAuthentication")
		case strings.HasPrefix(p, "deploy/base/network/"):
			mark(Network, "a NetworkPolicy")
		case strings.HasPrefix(p, "deploy/base/mesh/sidecar") || strings.HasPrefix(p, "deploy/base/mesh/service-entry"):
			mark(Network, "the Sidecar and ServiceEntries decide egress")
		case strings.HasPrefix(p, "deploy/base/mesh/virtual-service") || strings.HasPrefix(p, "deploy/base/mesh/destination-rules"):
			mark(Traffic, "routing, retries and outlier detection")
		case base == "rollout.yaml":
			if anyMatch(changed, capKV) {
				mark(Capacity, "the Rollout's replicas or resources")
			}
			if anyMatch(changed, rolloutKV) || touches(changed, "terminationGracePeriodSeconds", "topologySpread", "podAntiAffinity", "Probe", "image:") {
				mark(Rollout, "the Rollout's strategy, probes, spread or image")
			}
			if !matched {
				mark(Rollout, "the Rollout changed")
			}
		case base == "autoscaling.yaml":
			mark(Capacity, "the ScaledObject")
		case base == "pdb.yaml":
			mark(Rollout, "the PodDisruptionBudget decides how disruption proceeds")
		case strings.HasPrefix(p, "deploy/envs/") && base == "image.yaml":
			mark(Rollout, "an environment's image pin is a release")
		case strings.HasPrefix(p, "deploy/envs/"):
			if anyMatch(changed, capKV) || touches(changed, "ReplicaCount", "replicas") {
				mark(Capacity, "an environment's scale")
			}
			if anyMatch(changed, rolloutKV) || touches(changed, "steps", "analysis") {
				mark(Rollout, "an environment's rollout pace")
			}
			if !matched {
				mark(Rollout, "an environment overlay changed")
			}
		case base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile."):
			mark(Image, "the Dockerfile decides the base image, the user and the layers")
		case strings.HasPrefix(p, ".github/workflows/"):
			mark(Delivery, "a workflow decides how the change is proven and published")
		case strings.HasSuffix(p, ".tf") || strings.HasSuffix(p, ".tfvars") || strings.HasSuffix(p, ".tf.json"):
			mark(Terraform, "a Terraform change")
		case base == "go.mod" || base == "go.sum":
			if anyMatch(added, clientMod) {
				mark(NewCall, "a new dependency on another service's client module")
			} else {
				matched = true
			}
		case strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go"):
			if anyMatch(added, dialRE) || anyMatch(added, clientMod) {
				mark(NewCall, "code that opens a connection to another service")
			}
		}
		if !matched {
			r.Unclassified = append(r.Unclassified, p)
		}
	}
	if len(c.PlanJSON) > 0 {
		for _, a := range planAspects(c.PlanJSON) {
			add(a.Aspect, a.File, a.Rule)
		}
	}
	r.NoImpact = len(c.Files) > 0 && len(r.Aspects) == 0 && len(r.Unclassified) == 0 && len(c.PlanJSON) == 0
	slices.SortFunc(r.Aspects, func(a, b Aspect) int { return slices.Index(All, a) - slices.Index(All, b) })
	return r
}

// serviceYAML classifies a change to the one file teams write. With both
// sides of the file it compares each spec field; with only hunks it falls
// back to the section a hunk's context shows, then to the line itself.
func serviceYAML(f change.File) []Reason {
	rules := map[string]Reason{
		"authorizedCallers": {Aspect: Authz, Rule: "authorizedCallers decide who may call which methods"},
		"ingress":           {Aspect: Network, Rule: "ingress decides who may connect"},
		"egress":            {Aspect: Network, Rule: "egress decides what the service may connect to"},
		"scaling":           {Aspect: Capacity, Rule: "scaling sets replicas and the autoscaling target"},
		"environments":      {Aspect: Capacity, Rule: "an environment override changes scale"},
		"placement":         {Aspect: Rollout, Rule: "placement changes where replicas may run"},
		"proto":             {Aspect: Contract, Rule: "the contract's location changed"},
	}
	var out []Reason
	seen := map[Aspect]bool{}
	emit := func(key string, grew bool) {
		r, ok := rules[key]
		if !ok {
			r = Reason{Aspect: Contract, Rule: "service.yaml changed outside the known fields: " + key}
		}
		r.File = f.Path
		if !seen[r.Aspect] {
			seen[r.Aspect] = true
			out = append(out, r)
		}
		if key == "egress" && grew && !seen[NewCall] {
			seen[NewCall] = true
			out = append(out, Reason{Aspect: NewCall, File: f.Path, Rule: "a destination added to egress is a new call"})
		}
	}
	if f.Before != "" || f.After != "" {
		before, after := specOf(f.Before), specOf(f.After)
		for key := range union(before, after) {
			b, _ := json.Marshal(before[key])
			a, _ := json.Marshal(after[key])
			if string(a) != string(b) {
				emit(key, count(after[key]) > count(before[key]))
			}
		}
		return out
	}
	// Hunks only: the section when the context shows it, else the line.
	for section, lines := range f.Sections() {
		lines = code(lines)
		if len(lines) == 0 {
			continue
		}
		switch {
		case section != "":
			emit(section, len(code(f.AddedLines())) > len(code(f.RemovedLines())))
		case touches(lines, "methods:", "spiffe:"):
			emit("authorizedCallers", false)
		case touches(lines, "host:"):
			emit("egress", touches(code(f.AddedLines()), "host:"))
		case touches(lines, "minReplicas", "maxReplicas", "inflightPerPod"):
			emit("scaling", false)
		default:
			emit("unknown", false)
		}
	}
	return out
}

func specOf(doc string) map[string]any {
	var d struct {
		Spec map[string]any `yaml:"spec"`
	}
	_ = yaml.Unmarshal([]byte(doc), &d)
	if d.Spec == nil {
		return map[string]any{}
	}
	return d.Spec
}

func union(a, b map[string]any) map[string]bool {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	return keys
}

func count(v any) int {
	if l, ok := v.([]any); ok {
		return len(l)
	}
	return 0
}

// planAspects reads a Terraform plan's resource_changes: creates are new
// resources, deletes and replacements are deletions.
func planAspects(planJSON []byte) []Reason {
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return []Reason{{Aspect: Terraform, File: "plan.json", Rule: "a plan that does not parse is itself a finding"}}
	}
	out := []Reason{{Aspect: Terraform, File: "plan.json", Rule: "a Terraform plan"}}
	for _, rc := range plan.ResourceChanges {
		actions := strings.Join(rc.Change.Actions, ",")
		switch {
		case actions == "delete,create" || actions == "create,delete":
			out = append(out, Reason{Aspect: Deletion, File: rc.Address, Rule: "replaced: destroy then create"})
		case slices.Contains(rc.Change.Actions, "delete"):
			out = append(out, Reason{Aspect: Deletion, File: rc.Address, Rule: "destroyed"})
		case slices.Contains(rc.Change.Actions, "create"):
			out = append(out, Reason{Aspect: NewResource, File: rc.Address, Rule: "created"})
		}
	}
	return out
}

// code drops comment-only and blank lines, so a reworded comment is not a
// change to what runs.
func code(lines []string) []string {
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") {
			continue
		}
		out = append(out, l)
	}
	return out
}

func touches(lines []string, words ...string) bool {
	for _, l := range lines {
		for _, w := range words {
			if strings.Contains(l, w) {
				return true
			}
		}
	}
	return false
}

func anyMatch(lines []string, re *regexp.Regexp) bool {
	for _, l := range lines {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}
