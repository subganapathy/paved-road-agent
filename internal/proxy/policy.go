package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Policy is what every call passes through before it reaches a backend.
// It is deliberately small: the sandbox's NetworkPolicy keeps everyone
// else out, so this is about bounding what a legitimate but possibly
// subverted agent can do.
type Policy struct {
	limits Limits
	mu     sync.Mutex
	calls  map[string]int // per session
}

func newPolicy(l Limits) *Policy {
	return &Policy{limits: l, calls: map[string]int{}}
}

// admit counts the call against the session's budget.
func (p *Policy) admit(session string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if session == "" {
		return fmt.Errorf("missing X-Session-ID")
	}
	p.calls[session]++
	if p.calls[session] > p.limits.CallsPerSessionMax {
		return fmt.Errorf("session %s exceeded %d connector calls", session, p.limits.CallsPerSessionMax)
	}
	return nil
}

// forget releases a finished session's counter.
func (p *Policy) forget(session string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.calls, session)
}

// rangeOK bounds a time range.
func (p *Policy) rangeOK(start, end time.Time) error {
	if end.Before(start) {
		return fmt.Errorf("end before start")
	}
	if end.Sub(start) > p.limits.QueryRangeMax {
		return fmt.Errorf("range %s exceeds the limit of %s", end.Sub(start), p.limits.QueryRangeMax)
	}
	return nil
}

// clip bounds a result body, marking truncation so the model knows.
func (p *Policy) clip(b []byte) ([]byte, bool) {
	if len(b) <= p.limits.ResultBytesMax {
		return b, false
	}
	return b[:p.limits.ResultBytesMax], true
}

// Audit writes one line per call. Everything a later question about
// "what did the agent read" needs, nothing a credential could leak
// through: no bodies, no tokens.
type Audit struct {
	log *slog.Logger
}

func newAudit(l *slog.Logger) *Audit {
	if l == nil {
		l = slog.Default()
	}
	return &Audit{log: l.With("component", "proxy")}
}

func (a *Audit) call(session, slot, op, summary string, status int, bytes int, took time.Duration, err error) {
	attrs := []any{"session", session, "slot", slot, "op", op, "args", summary, "status", status, "bytes", bytes, "ms", took.Milliseconds()}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
	}
	a.log.Info("connector", attrs...)
}

// summarize renders request arguments for the audit line, bounded.
func summarize(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 200 {
		return s[:197] + "..."
	}
	return s
}

// ---- small HTTP helpers --------------------------------------------------

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, apiError{Error: err.Error()})
}

func decode(r *http.Request, into any) error {
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil
	}
	return json.Unmarshal(b, into)
}

func jsonEncoder(w io.Writer) *json.Encoder { return json.NewEncoder(w) }
