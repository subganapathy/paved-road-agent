// Package findings is the report contract: what the lead returns and the
// controller validates, renders and gates on. Claims carry evidence or
// they are dropped; every property gets a verdict; questions carry the
// file text each answer implies.
package findings

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Severity is what a finding means for the change.
type Severity string

const (
	Blocking Severity = "blocking" // the change must not ship as is
	Warning  Severity = "warning"  // ship with eyes open
	Info     Severity = "info"     // including "cannot open this path, and here is why"
)

var severities = []Severity{Blocking, Warning, Info}

// Properties, in report order. Keys are the JSON keys (underscored).
var Properties = []string{"correct", "reversible", "within_budget", "stable_under_failure", "progressively_delivered", "available", "secure", "observable", "proven"}

// Evidence is one fact a finding rests on, with where it came from so a
// reader can check it.
type Evidence struct {
	Kind   string `json:"kind"`            // metric, config, diff, plan, doc, answer
	Source string `json:"source"`          // the tool or file
	Query  string `json:"query,omitempty"` // the query or path
	Value  string `json:"value"`           // what it returned
}

// Finding is one claim under one property.
type Finding struct {
	Property       string     `json:"property"`
	Severity       Severity   `json:"severity"`
	Claim          string     `json:"claim"`
	Evidence       []Evidence `json:"evidence"`
	Recommendation string     `json:"recommendation,omitempty"`
	Confidence     float64    `json:"confidence"`
	Studied        []string   `json:"studied,omitempty"`
}

// Verdict is a property's result.
type Verdict struct {
	Verdict Severity `json:"verdict"`
	Summary string   `json:"summary"`
}

// Obligation is what one property means for this change.
type Obligation struct {
	Property  string `json:"property"`
	Establish string `json:"establish"`
	Evidence  string `json:"evidence"`
}

// Instantiation is the lead's first move, written down.
type Instantiation struct {
	WorstCase       string       `json:"worst_case"`
	Proportionality string       `json:"proportionality"`
	Obligations     []Obligation `json:"obligations"`
}

// StackLine is one discovered binding line with its evidence.
type StackLine struct {
	Is       string `json:"is"`
	Evidence string `json:"evidence"`
}

// EnvFacts is what discovery measured for one environment.
type EnvFacts struct {
	Clusters  []string `json:"clusters"`
	Instances int      `json:"instances"`
	RPS       float64  `json:"rps"`
	Callers   []string `json:"callers"`
	External  bool     `json:"external"`
}

// Discovery is the run memory, summarized.
type Discovery struct {
	Stack    map[string]StackLine `json:"stack"`
	Verified []string             `json:"verified"`
	Broken   []string             `json:"broken"`
	ByEnv    map[string]EnvFacts  `json:"by_env"`
}

// Unknown is a fact with no source.
type Unknown struct {
	What   string   `json:"what"`
	Tried  []string `json:"tried"`
	Blocks []string `json:"blocks"`
}

// FileChange is the text a question's answer, or a proposal, puts in a file.
type FileChange struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Reason  string `json:"reason,omitempty"`
}

// Question is a yes/no question with the file change each answer implies.
type Question struct {
	ID       string                `json:"id"`
	Text     string                `json:"text"`
	Blocks   []string              `json:"blocks"`
	Answers  map[string]FileChange `json:"answers"`
	WaivedBy string                `json:"waived_by,omitempty"`
}

// Report is the whole thing.
type Report struct {
	Change        string             `json:"change"`
	Instantiation Instantiation      `json:"instantiation"`
	Discovery     Discovery          `json:"discovery"`
	Properties    map[string]Verdict `json:"properties"`
	Findings      []Finding          `json:"findings"`
	Unknowns      []Unknown          `json:"unknowns"`
	Questions     []Question         `json:"questions"`
	Proposals     []FileChange       `json:"proposals"`
	Verdict       Severity           `json:"verdict"`
	Summary       string             `json:"summary"`
}

// Validate enforces the contract on a finding.
func (f Finding) Validate() error {
	var errs []string
	if !slices.Contains(Properties, f.Property) {
		errs = append(errs, fmt.Sprintf("property %q is not one of %s", f.Property, strings.Join(Properties, ", ")))
	}
	if !slices.Contains(severities, f.Severity) {
		errs = append(errs, fmt.Sprintf("severity %q is not blocking, warning or info", f.Severity))
	}
	if strings.TrimSpace(f.Claim) == "" {
		errs = append(errs, "claim is empty")
	}
	if len(f.Evidence) == 0 {
		errs = append(errs, "no evidence")
	}
	for i, e := range f.Evidence {
		if e.Source == "" || e.Value == "" {
			errs = append(errs, fmt.Sprintf("evidence[%d] needs a source and a value", i))
		}
	}
	if f.Confidence < 0 || f.Confidence > 1 {
		errs = append(errs, "confidence must be between 0 and 1")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Normalize drops findings without evidence (returned, so the caller can
// say so), fills missing property verdicts from the findings, and sets
// the overall verdict. The controller calls it before rendering.
func (r *Report) Normalize() (dropped []Finding) {
	kept := r.Findings[:0]
	for _, f := range r.Findings {
		if err := f.Validate(); err != nil {
			dropped = append(dropped, f)
			continue
		}
		kept = append(kept, f)
	}
	r.Findings = kept
	if r.Properties == nil {
		r.Properties = map[string]Verdict{}
	}
	for _, p := range Properties {
		v, ok := r.Properties[p]
		worst := Info
		for _, f := range r.Findings {
			if f.Property == p && rank(f.Severity) < rank(worst) {
				worst = f.Severity
			}
		}
		if !ok || !slices.Contains(severities, v.Verdict) {
			v.Verdict = worst
		}
		if v.Summary == "" {
			v.Summary = "no finding recorded"
		}
		r.Properties[p] = v
	}
	overall := Info
	for _, v := range r.Properties {
		if rank(v.Verdict) < rank(overall) {
			overall = v.Verdict
		}
	}
	r.Verdict = overall
	return dropped
}

func rank(s Severity) int {
	switch s {
	case Blocking:
		return 0
	case Warning:
		return 1
	}
	return 2
}

var fence = regexp.MustCompile("(?s)```json\\s*(.*?)```")

// Parse extracts the report from the lead's final message: the last
// fenced json block, or the whole text if it is JSON.
func Parse(text string) (*Report, error) {
	body := strings.TrimSpace(text)
	if m := fence.FindAllStringSubmatch(text, -1); len(m) > 0 {
		body = strings.TrimSpace(m[len(m)-1][1])
	}
	var r Report
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return nil, fmt.Errorf("report is not valid JSON: %w", err)
	}
	if r.Properties == nil && len(r.Findings) == 0 && len(r.Questions) == 0 {
		return nil, errors.New("report has no properties, findings or questions")
	}
	return &r, nil
}

// Render writes the report for a terminal or a comment.
func (r *Report) Render() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "verdict: %s\n%s\n", strings.ToUpper(string(r.Verdict)), r.Summary)
	if r.Instantiation.WorstCase != "" {
		fmt.Fprintf(&sb, "\nworst case: %s (%s proof)\n", r.Instantiation.WorstCase, r.Instantiation.Proportionality)
	}
	if len(r.Discovery.Stack) > 0 {
		sb.WriteString("\nstack:\n")
		for _, k := range []string{"mesh", "deploy", "admission", "enforcer", "autoscaler", "metrics"} {
			if l, ok := r.Discovery.Stack[k]; ok {
				fmt.Fprintf(&sb, "  %-11s %s\n", k+":", l.Is)
			}
		}
		if len(r.Discovery.Broken) > 0 {
			fmt.Fprintf(&sb, "  broken bindings: %s\n", strings.Join(r.Discovery.Broken, ", "))
		}
	}
	sb.WriteString("\nproperties:\n")
	for _, p := range Properties {
		if v, ok := r.Properties[p]; ok {
			fmt.Fprintf(&sb, "  %-24s %-8s %s\n", p, v.Verdict, v.Summary)
		}
	}
	for _, f := range r.Findings {
		fmt.Fprintf(&sb, "\n[%s] %s (confidence %.2f)\n  %s\n", f.Severity, f.Property, f.Confidence, f.Claim)
		for _, e := range f.Evidence {
			src := e.Source
			if e.Query != "" {
				src += " " + e.Query
			}
			fmt.Fprintf(&sb, "  evidence (%s): %s = %s\n", e.Kind, src, oneLine(e.Value))
		}
		if f.Recommendation != "" {
			fmt.Fprintf(&sb, "  recommend: %s\n", f.Recommendation)
		}
	}
	for _, u := range r.Unknowns {
		fmt.Fprintf(&sb, "\nunknown: %s (blocks %s); tried: %s\n", u.What, strings.Join(u.Blocks, ", "), strings.Join(u.Tried, "; "))
	}
	for _, q := range r.Questions {
		fmt.Fprintf(&sb, "\nQUESTION %s (blocks %s):\n  %s\n", q.ID, strings.Join(q.Blocks, ", "), q.Text)
		for _, a := range []string{"yes", "no"} {
			if fc, ok := q.Answers[a]; ok {
				fmt.Fprintf(&sb, "  if %s → write to %s:\n%s\n", a, fc.Path, indent(fc.Content))
			}
		}
	}
	for _, p := range r.Proposals {
		fmt.Fprintf(&sb, "\nPROPOSAL %s (%s):\n%s\n", p.Path, p.Reason, indent(p.Content))
	}
	return sb.String()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 140 {
		return s[:137] + "..."
	}
	return s
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
