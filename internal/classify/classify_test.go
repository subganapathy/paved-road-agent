package classify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/subganapathy/paved-road-agent/internal/change"
)

// evalCase is one synthetic change with a known impact. The same files
// score the live specialists later; here they score the classifier.
type evalCase struct {
	Name   string        `json:"name"`
	Why    string        `json:"why"`
	Change change.Change `json:"change"`
	Expect struct {
		Aspects []Aspect `json:"aspects"`
		Verdict string   `json:"verdict"`
	} `json:"expect"`
}

func TestClassifierAgainstEvalCases(t *testing.T) {
	paths, _ := filepath.Glob("../../evals/cases/*.json")
	if len(paths) == 0 {
		t.Fatal("no eval cases")
	}
	tp, fp, fn := 0, 0, 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var c evalCase
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		got := Classify(c.Change)
		want := c.Expect.Aspects
		if want == nil {
			want = []Aspect{}
		}
		if got.Aspects == nil {
			got.Aspects = []Aspect{}
		}
		for _, a := range got.Aspects {
			if slices.Contains(want, a) {
				tp++
			} else {
				fp++
			}
		}
		for _, a := range want {
			if !slices.Contains(got.Aspects, a) {
				fn++
			}
		}
		if !slices.Equal(got.Aspects, want) {
			t.Errorf("%s: got %v, want %v (reasons %v, unclassified %v)", c.Name, got.Aspects, want, got.Reasons, got.Unclassified)
		}
		if c.Name == "comment-only" && !got.NoImpact {
			t.Errorf("%s: should be no-impact", c.Name)
		}
		if c.Name == "handler-logic-only" && len(got.Unclassified) != 1 {
			t.Errorf("%s: the handler should be unclassified for the model, got %v", c.Name, got.Unclassified)
		}
	}
	t.Logf("aspect precision %.2f, recall %.2f over %d cases", float64(tp)/float64(tp+fp), float64(tp)/float64(tp+fn), len(paths))
}
