package findings

import (
	"strings"
	"testing"
)

func TestContract(t *testing.T) {
	good := Finding{Aspect: "network", Severity: Blocking, Claim: "frontend can no longer connect",
		Evidence: []Evidence{{Kind: "diff", Source: "deploy/base/network/allow-from-frontend.yaml", Value: "deleted"}}, Confidence: 0.9}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := Finding{Aspect: "network", Severity: "severe", Claim: "", Confidence: 2}
	err := bad.Validate()
	for _, want := range []string{"severity", "claim is required", "evidence is required", "confidence"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in %v", want, err)
		}
	}
	if Verdict([]Finding{{Severity: Info}, {Severity: Warning}}) != Warning || Verdict(nil) != Info {
		t.Error("verdict is the most severe finding")
	}
	fs, err := Parse([]byte(`{"findings":[{"aspect":"capacity","severity":"info","claim":"no impact","evidence":[{"kind":"config","source":"deploy/base/autoscaling.yaml","value":"unchanged"}],"confidence":1}]}`))
	if err != nil || len(fs) != 1 {
		t.Fatalf("parse: %v %v", fs, err)
	}
}
