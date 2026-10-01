// Package evalcase reads the synthetic changes under evals/cases. The same
// files score the classifier offline and the specialists live.
package evalcase

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/subganapathy/paved-road-agent/internal/change"
	"github.com/subganapathy/paved-road-agent/internal/findings"
)

// Case is one change with a known impact.
type Case struct {
	Name   string        `json:"name"`
	Why    string        `json:"why"`
	Change change.Change `json:"change"`
	// Repo and SHA, when set, mount a real repository in the session so
	// specialists can read around the diff.
	Repo   string `json:"repo,omitempty"`
	SHA    string `json:"sha,omitempty"`
	Expect struct {
		Aspects []string          `json:"aspects"`
		Verdict findings.Severity `json:"verdict"`
	} `json:"expect"`
}

// Load reads one case file.
func Load(path string) (*Case, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Case
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Change.Ref == "" {
		c.Change.Ref = "case:" + c.Name
	}
	return &c, nil
}
