package findings

import (
	"strings"
	"testing"
)

const leadMessage = "Here is my assessment.\n\n```json\n" + `{
  "change": "acme/hello#4@abc",
  "instantiation": {"worst_case": "every request from frontend errors", "proportionality": "full",
    "obligations": [{"property": "correct", "establish": "contract holds", "evidence": "proto, call site"}]},
  "discovery": {"stack": {"mesh": {"is": "sidecar mesh, mTLS strict", "evidence": "istio_requests_total present; istio-proxy in pod template"}},
    "verified": ["mesh"], "broken": [], "by_env": {"prod": {"clusters": ["prod"], "instances": 2, "rps": 4.8, "callers": ["frontend"], "external": false}}},
  "properties": {"correct": {"verdict": "warning", "summary": "contract silent on empty names"},
                 "proven": {"verdict": "info", "summary": "handler test covers the branch"}},
  "findings": [
    {"property": "correct", "severity": "warning", "claim": "frontend forwards empty names unchecked",
     "evidence": [{"kind": "diff", "source": "frontend/internal/web/handler.go", "query": "L42", "value": "name := r.URL.Query().Get(\"name\")"}],
     "recommendation": "validate in frontend", "confidence": 0.8, "studied": ["frontend"]},
    {"property": "within_budget", "severity": "blocking", "claim": "a number I made up", "evidence": [], "confidence": 0.9}
  ],
  "unknowns": [], "questions": [], "proposals": [],
  "verdict": "info", "summary": "..."
}` + "\n```\n"

func TestParseNormalizeRender(t *testing.T) {
	r, err := Parse(leadMessage)
	if err != nil {
		t.Fatal(err)
	}
	dropped := r.Normalize()
	if len(dropped) != 1 || dropped[0].Property != "within_budget" {
		t.Fatalf("the evidence-free finding must be dropped, got %+v", dropped)
	}
	if len(r.Findings) != 1 {
		t.Fatalf("kept %d findings", len(r.Findings))
	}
	if len(r.Properties) != len(Properties) {
		t.Errorf("every property gets a verdict; got %d", len(r.Properties))
	}
	if r.Properties["within_budget"].Verdict != Info {
		t.Errorf("a property whose only finding was dropped is info, got %s", r.Properties["within_budget"].Verdict)
	}
	if r.Verdict != Warning {
		t.Errorf("overall verdict follows the properties: got %s", r.Verdict)
	}
	out := r.Render()
	for _, want := range []string{"verdict: WARNING", "mesh:", "sidecar mesh", "[warning] correct", "evidence (diff)", "recommend: validate"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}

func TestParseRejectsNonReports(t *testing.T) {
	if _, err := Parse("I could not finish."); err == nil {
		t.Error("prose is not a report")
	}
	if _, err := Parse("```json\n{\"change\":\"x\"}\n```"); err == nil {
		t.Error("an empty report is not a report")
	}
}

func TestQuestionsRender(t *testing.T) {
	r := &Report{
		Questions: []Question{{ID: "q1", Text: "Is greeter deployed anywhere?", Blocks: []string{"progressively_delivered", "within_budget"},
			Answers: map[string]FileChange{"no": {Path: ".paved-agent/discover.yaml", Content: "service: greeter\nworkloads: []\nprobe: {container: greeter}\n"}}}},
	}
	r.Normalize()
	out := r.Render()
	if !strings.Contains(out, "QUESTION q1") || !strings.Contains(out, "if no → write to .paved-agent/discover.yaml") {
		t.Errorf("render:\n%s", out)
	}
}

func TestHyphenatedIdsAreAccepted(t *testing.T) {
	in, err := ParseInstantiation("```json\n{\"worst_case\":\"x\",\"proportionality\":\"full\",\"obligations\":[{\"property\":\"progressively-delivered\",\"establish\":\"y\",\"evidence\":\"z\"}]}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if in.Obligations[0].Property != "progressively_delivered" {
		t.Errorf("not canonicalized: %q", in.Obligations[0].Property)
	}
	r := &Report{Findings: []Finding{{Property: "stable-under-failure", Severity: Info, Claim: "c", Evidence: []Evidence{{Source: "s", Value: "v"}}}}, Properties: map[string]Verdict{"within-budget": {Verdict: Info}}}
	if d := r.Normalize(); len(d) != 0 {
		t.Errorf("dropped a valid finding with a hyphenated id: %+v", d)
	}
	if _, ok := r.Properties["within_budget"]; !ok {
		t.Error("property key not canonicalized")
	}
}
