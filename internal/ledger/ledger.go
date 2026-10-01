// Package ledger is the agent's memory: facts about entities in the
// environment, keyed so a specialist can ask "what do we already know about
// echo's capacity" before studying anything. Facts carry provenance and a
// validity marker, so stale knowledge is visible rather than silently
// reused. Deduplication is a keyed lookup, not a similarity search.
package ledger

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Fact is one thing a specialist learned.
type Fact struct {
	Entity string `json:"entity"` // "echo", "cluster/dev", "aws_db_instance.orders"
	Aspect string `json:"aspect"` // one of the classifier's aspects
	Fact   string `json:"fact"`   // the statement, with its number or value
	Source string `json:"source"` // where it was read: a query, a file, a plan
	// ValidUntil says when the fact should be re-studied rather than reused.
	ValidUntil time.Time `json:"valid_until,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
	// Change is the assessment that recorded it, for provenance.
	Change string `json:"change,omitempty"`
}

// Store is a file-backed ledger: one JSON file, rewritten on every put.
// Enough for one reviewer; a database replaces it without changing callers.
type Store struct {
	path string
	mu   sync.Mutex
}

// Open opens or creates the ledger at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return &Store{path: path}, nil
}

func (s *Store) load() ([]Fact, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var facts []Fact
	return facts, json.Unmarshal(b, &facts)
}

// Put records a fact. A fact with the same entity, aspect and source
// replaces the earlier one.
func (s *Store) Put(f Fact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	facts, err := s.load()
	if err != nil {
		return err
	}
	if f.RecordedAt.IsZero() {
		f.RecordedAt = time.Now().UTC()
	}
	kept := facts[:0]
	for _, x := range facts {
		if !(x.Entity == f.Entity && x.Aspect == f.Aspect && x.Source == f.Source) {
			kept = append(kept, x)
		}
	}
	kept = append(kept, f)
	b, err := json.MarshalIndent(kept, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}

// Get returns the facts about an entity, all aspects when aspect is "",
// newest first, with stale ones marked by their ValidUntil being past.
func (s *Store) Get(entity, aspect string, now time.Time) ([]Fact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	facts, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []Fact
	for _, f := range facts {
		if f.Entity == entity && (aspect == "" || f.Aspect == aspect) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecordedAt.After(out[j].RecordedAt) })
	return out, nil
}

// Stale reports whether a fact should be re-studied.
func (f Fact) Stale(now time.Time) bool { return !f.ValidUntil.IsZero() && now.After(f.ValidUntil) }
