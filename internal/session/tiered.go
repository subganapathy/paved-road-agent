package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/subganapathy/paved-road-agent/internal/findings"
)

// Tiered runs triage on the smaller model first and escalates to the
// deep tier only when triage finds a meaningful worst case, a warning or
// blocking verdict, or a question it could not resolve. Most changes end
// at triage. The deep run receives triage's instantiation and verdicts so
// it starts informed rather than from scratch.
type Tiered struct {
	Triage Options // the dev-tier lead
	Deep   Options // the release-tier lead
}

// Outcome is what a tiered review produced.
type Outcome struct {
	Report    *findings.Report
	Escalated bool
	Sessions  []string // triage, then deep if it ran
	Usage     []string // one line per session
	LastText  string   // the final message when it was not a report
}

// Escalate decides whether triage's report warrants the deep tier.
func Escalate(r *findings.Report) (bool, string) {
	switch {
	case r == nil:
		return true, "triage produced no report"
	case r.Verdict == findings.Blocking:
		return true, "triage found a blocking finding"
	case r.Verdict == findings.Warning:
		return true, "triage found a warning"
	case len(r.Questions) > 0:
		return true, "triage has open questions"
	case strings.EqualFold(r.Instantiation.Proportionality, "full"):
		return true, "triage judged the worst case meaningful"
	}
	return false, "triage found no meaningful worst case and no finding above info"
}

// Run executes the tiers.
func (t Tiered) Run(ctx context.Context, client anthropic.Client, brief string) (*Outcome, error) {
	out := &Outcome{}
	s, err := Create(ctx, client, t.Triage, brief)
	if err != nil {
		return nil, fmt.Errorf("create triage session: %w", err)
	}
	out.Sessions = append(out.Sessions, s.ID)
	rep, final, text, err := CollectReport(ctx, client, s.ID)
	if final != nil {
		out.Usage = append(out.Usage, usageLine("triage", final))
	}
	if err != nil {
		// Triage could not even produce a report: escalate rather than fail.
		out.LastText = text
		rep = nil
	} else {
		rep.Normalize()
	}
	escalate, why := Escalate(rep)
	if !escalate {
		out.Report = rep
		return out, nil
	}
	out.Escalated = true
	deepBrief := brief
	if rep != nil {
		deepBrief += "\n\n# Triage (a smaller model's first pass; verify, do not trust)\n\nEscalated because: " + why + ".\n" + triageSummary(rep)
	} else {
		deepBrief += "\n\n# Triage\n\nA smaller model's first pass ended without a report; escalated because: " + why + "."
	}
	d, err := Create(ctx, client, t.Deep, deepBrief)
	if err != nil {
		return out, fmt.Errorf("create deep session: %w", err)
	}
	out.Sessions = append(out.Sessions, d.ID)
	deep, final, text, err := CollectReport(ctx, client, d.ID)
	if final != nil {
		out.Usage = append(out.Usage, usageLine("deep", final))
	}
	if err != nil {
		out.LastText = text
		if rep != nil {
			out.Report = rep // the triage report is better than nothing
		}
		return out, err
	}
	deep.Normalize()
	out.Report = deep
	return out, nil
}

func triageSummary(r *findings.Report) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Worst case per triage: %s (%s proof)\n", r.Instantiation.WorstCase, r.Instantiation.Proportionality)
	for _, p := range findings.Properties {
		if v, ok := r.Properties[p]; ok {
			fmt.Fprintf(&sb, "- %s: %s — %s\n", p, v.Verdict, v.Summary)
		}
	}
	if len(r.Discovery.Stack) > 0 {
		sb.WriteString("Stack per triage: ")
		for k, l := range r.Discovery.Stack {
			fmt.Fprintf(&sb, "%s=%s; ", k, l.Is)
		}
		sb.WriteString("\n")
	}
	for _, q := range r.Questions {
		fmt.Fprintf(&sb, "- open question: %s\n", q.Text)
	}
	return sb.String()
}

func usageLine(tier string, s *anthropic.BetaManagedAgentsSession) string {
	return fmt.Sprintf("%s %s: %d input (%d cached), %d output tokens, %.0fs active", tier, s.ID, s.Usage.InputTokens, s.Usage.CacheReadInputTokens, s.Usage.OutputTokens, s.Stats.ActiveSeconds)
}
