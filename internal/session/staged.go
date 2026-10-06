package session

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/subganapathy/paved-road-agent/internal/findings"
)

// Staged is the review in two sessions: the strongest model does the
// first move only — reads the change and writes the obligations — and a
// smaller model executes them: discovery, grading, the report. The
// judgment is short and expensive; the execution is long and cheap.
type Staged struct {
	Instantiate Options // the instantiator, strongest model
	Execute     Options // the lead on the smaller model
}

// Run executes both stages.
func (s Staged) Run(ctx context.Context, client anthropic.Client, brief string) (*Outcome, error) {
	out := &Outcome{}
	first, err := Create(ctx, client, s.Instantiate, brief)
	if err != nil {
		return nil, fmt.Errorf("create instantiation session: %w", err)
	}
	out.Sessions = append(out.Sessions, first.ID)
	final, err := Wait(ctx, client, first.ID, 5e9)
	if final != nil {
		out.Usage = append(out.Usage, usageLine("instantiate", final))
	}
	if err != nil {
		return out, err
	}
	text, err := LastLeadMessage(ctx, client, first.ID)
	if err != nil {
		return out, err
	}
	in, err := findings.ParseInstantiation(text)
	if err != nil {
		out.LastText = text
		return out, fmt.Errorf("instantiation: %w", err)
	}

	execBrief := brief + "\n\n# Instantiation, written by the senior reviewer — adopt it\n\n" + in.Render() +
		"\nEstablish each obligation with the evidence it names; grade from what you establish; finish with the report JSON."
	second, err := Create(ctx, client, s.Execute, execBrief)
	if err != nil {
		return out, fmt.Errorf("create execution session: %w", err)
	}
	out.Sessions = append(out.Sessions, second.ID)
	rep, final, text, err := Collect(ctx, client, second.ID)
	if final != nil {
		out.Usage = append(out.Usage, usageLine("execute", final))
	}
	if err != nil {
		out.LastText = text
		return out, err
	}
	rep.Normalize()
	// The report carries the instantiation the senior reviewer wrote, so
	// the reader sees the judgment and the evidence side by side.
	if rep.Instantiation.WorstCase == "" {
		rep.Instantiation = *in
	}
	out.Report = rep
	return out, nil
}
