// Package change is the input to an assessment: what a change touches,
// independent of where it came from (a GitHub pull request, a local diff,
// a Terraform plan).
package change

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Status is what happened to a file.
type Status string

const (
	Added    Status = "added"
	Modified Status = "modified"
	Deleted  Status = "deleted"
	Renamed  Status = "renamed"
)

// File is one changed file with its unified-diff hunks.
type File struct {
	Path   string `json:"path"`
	Status Status `json:"status"`
	// Patch is the unified diff for this file: "+" and "-" lines carry the
	// change, so rules can look at what moved, not only which file.
	Patch string `json:"patch,omitempty"`
	// Before and After are the whole file on each side, when the source
	// can provide them. Small configuration files are classified from
	// these rather than from hunks, whose three lines of context rarely
	// include the section a change sits in.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// Change is a reviewable unit: a set of files, and optionally the Terraform
// plan the change produces, as `terraform show -json` prints it.
type Change struct {
	// Ref names the change for people: "org/repo#42", a commit, a path.
	Ref   string `json:"ref"`
	Files []File `json:"files"`
	// PlanJSON is the Terraform plan in JSON, when the change has one.
	PlanJSON json.RawMessage `json:"plan_json,omitempty"`
}

// Sections attributes each added or removed line to the top-level key the
// hunk is under (for YAML: the two-space-indented key such as "ingress:"),
// using the hunk's context lines. Lines before any key go under "".
func (f File) Sections() map[string][]string {
	out := map[string][]string{}
	current := ""
	for _, l := range splitLines(f.Patch) {
		if len(l) == 0 || strings.HasPrefix(l, "@@") || strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") {
			continue
		}
		marker, body := l[0], l[1:]
		if marker != ' ' && marker != '+' && marker != '-' {
			continue
		}
		if m := sectionRE.FindStringSubmatch(body); m != nil {
			current = m[1]
		}
		if marker == '+' || marker == '-' {
			out[current] = append(out[current], body)
		}
	}
	return out
}

var sectionRE = regexp.MustCompile(`^  ([A-Za-z][A-Za-z0-9_]*):`)

func splitLines(s string) []string { return strings.Split(s, "\n") }

// AddedLines returns the lines a file's patch adds, without the "+".
func (f File) AddedLines() []string { return lines(f.Patch, '+') }

// RemovedLines returns the lines a file's patch removes, without the "-".
func (f File) RemovedLines() []string { return lines(f.Patch, '-') }

func lines(patch string, marker byte) []string {
	var out []string
	start := 0
	for i := 0; i <= len(patch); i++ {
		if i == len(patch) || patch[i] == '\n' {
			l := patch[start:i]
			start = i + 1
			if len(l) == 0 || l[0] != marker || (len(l) >= 3 && (l[:3] == "+++" || l[:3] == "---")) {
				continue
			}
			out = append(out, l[1:])
		}
	}
	return out
}
