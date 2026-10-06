package session

import (
	"testing"

	"github.com/subganapathy/paved-road-agent/internal/findings"
)

func TestEscalate(t *testing.T) {
	info := func(mod func(r *findings.Report)) *findings.Report {
		r := &findings.Report{Verdict: findings.Info, Instantiation: findings.Instantiation{Proportionality: "reduced"}}
		if mod != nil {
			mod(r)
		}
		return r
	}
	cases := []struct {
		name string
		r    *findings.Report
		want bool
	}{
		{"nil report", nil, true},
		{"info, reduced", info(nil), false},
		{"warning", info(func(r *findings.Report) { r.Verdict = findings.Warning }), true},
		{"blocking", info(func(r *findings.Report) { r.Verdict = findings.Blocking }), true},
		{"question", info(func(r *findings.Report) { r.Questions = []findings.Question{{ID: "q1"}} }), true},
		{"full proof", info(func(r *findings.Report) { r.Instantiation.Proportionality = "full" }), true},
	}
	for _, c := range cases {
		if got, _ := Escalate(c.r); got != c.want {
			t.Errorf("%s: escalate = %v, want %v", c.name, got, c.want)
		}
	}
}
