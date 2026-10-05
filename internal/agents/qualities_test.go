package agents

import (
	"strings"
	"testing"
)

func TestProgramLoads(t *testing.T) {
	p, err := LoadProgram()
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 2 {
		t.Errorf("version = %d", p.Version)
	}
	for _, q := range p.Qualities {
		if !strings.Contains(strings.Join(strings.Fields(q.Instantiate), " "), "that is the finding") && q.ID != "progressively-delivered" {
			t.Errorf("%s: instantiation guidance should say what the finding is when the quality is unaffected", q.ID)
		}
	}
}

// The program names no product: the model discovers them.
func TestProgramNamesNoProduct(t *testing.T) {
	p, err := LoadProgram()
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{string(qualitiesYAML), p.Render()} {
		if hits := FindProductNames(text); len(hits) > 0 {
			t.Errorf("product names in the program: %v", hits)
		}
	}
}

func TestFindProductNamesIsWholeWord(t *testing.T) {
	if hits := FindProductNames("the artist drew a flux capacitor"); len(hits) != 1 || hits[0] != "flux" {
		t.Errorf("flux should match as a word: %v", hits)
	}
	if hits := FindProductNames("influx of requests; awsome"); len(hits) != 0 {
		t.Errorf("substrings should not match: %v", hits)
	}
	if hits := FindProductNames("Kubernetes is assumed"); len(hits) != 0 {
		t.Errorf("kubernetes is allowed: %v", hits)
	}
}

func TestRenderIsReadable(t *testing.T) {
	p, err := LoadProgram()
	if err != nil {
		t.Fatal(err)
	}
	out := p.Render()
	for _, id := range QualityIDs {
		if !strings.Contains(out, "("+id+")") {
			t.Errorf("rendered prompt lacks %s", id)
		}
	}
	if n := len(out); n < 8000 || n > 40000 {
		t.Errorf("rendered prompt is %d bytes; expected a few thousand words", n)
	}
}
