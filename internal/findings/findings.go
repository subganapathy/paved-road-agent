// Package findings is the contract every specialist returns and the
// synthesizer consumes: claims backed by evidence, never opinions.
package findings

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Severity is what a finding means for the change.
type Severity string

const (
	Blocking Severity = "blocking" // the change must not ship as is
	Warning  Severity = "warning"  // ship with eyes open
	Info     Severity = "info"     // including "no impact, and here is why"
)

// Evidence is one fact a finding rests on, with where it came from so a
// reader can check it.
type Evidence struct {
	// Kind is metric, config, diff, plan, log or ledger.
	Kind string `json:"kind"`
	// Source names the system or file: "prometheus", "deploy/base/pdb.yaml".
	Source string `json:"source"`
	// Query is what was asked, when a query produced the value.
	Query string `json:"query,omitempty"`
	// Value is what was observed.
	Value string `json:"value"`
}

// Finding is one assessed impact of a change on one aspect.
type Finding struct {
	Aspect         string     `json:"aspect"`
	Severity       Severity   `json:"severity"`
	Claim          string     `json:"claim"`
	Evidence       []Evidence `json:"evidence"`
	Recommendation string     `json:"recommendation,omitempty"`
	// Confidence is the specialist's own, 0 to 1.
	Confidence float64 `json:"confidence"`
	// Studied lists the entities (services, resources) the specialist read.
	Studied []string `json:"studied,omitempty"`
}

// Report is what the synthesizer posts: every finding, and the verdict the
// most severe one implies.
type Report struct {
	Change   string    `json:"change"` // "owner/repo#123" or a commit
	Aspects  []string  `json:"aspects"`
	Findings []Finding `json:"findings"`
	Verdict  Severity  `json:"verdict"`
	Summary  string    `json:"summary"`
}

var severities = []Severity{Blocking, Warning, Info}

// Validate enforces the contract: every finding names an aspect, a known
// severity, a claim and at least one piece of evidence.
func (f Finding) Validate() error {
	var errs []error
	if f.Aspect == "" {
		errs = append(errs, errors.New("aspect is required"))
	}
	if !slices.Contains(severities, f.Severity) {
		errs = append(errs, fmt.Errorf("severity %q must be one of blocking, warning, info", f.Severity))
	}
	if strings.TrimSpace(f.Claim) == "" {
		errs = append(errs, errors.New("claim is required"))
	}
	if len(f.Evidence) == 0 {
		errs = append(errs, errors.New("a finding without evidence is an opinion: evidence is required"))
	}
	for i, e := range f.Evidence {
		if e.Kind == "" || e.Source == "" || e.Value == "" {
			errs = append(errs, fmt.Errorf("evidence[%d] needs kind, source and value", i))
		}
	}
	if f.Confidence < 0 || f.Confidence > 1 {
		errs = append(errs, fmt.Errorf("confidence %v must be between 0 and 1", f.Confidence))
	}
	return errors.Join(errs...)
}

// Verdict is the most severe finding's severity; info when there are none.
func Verdict(fs []Finding) Severity {
	v := Info
	for _, f := range fs {
		if rank(f.Severity) > rank(v) {
			v = f.Severity
		}
	}
	return v
}

func rank(s Severity) int { return slices.Index(slices.Clone([]Severity{Info, Warning, Blocking}), s) }

// Parse reads findings the agent produced. It accepts either a bare JSON
// array of findings or an object with a "findings" field, and validates
// each one.
func Parse(b []byte) ([]Finding, error) {
	var list []Finding
	if err := json.Unmarshal(b, &list); err != nil {
		var obj struct {
			Findings []Finding `json:"findings"`
		}
		if err2 := json.Unmarshal(b, &obj); err2 != nil {
			return nil, fmt.Errorf("findings are neither an array nor an object with findings: %w", err)
		}
		list = obj.Findings
	}
	var errs []error
	for i, f := range list {
		if err := f.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("finding %d (%s): %w", i, f.Aspect, err))
		}
	}
	return list, errors.Join(errs...)
}
