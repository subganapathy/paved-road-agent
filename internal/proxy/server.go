package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Server serves the slots over HTTP. Routes are /v1/<slot>/<op>; every
// request carries X-Session-ID (for limits and audit) and, when the proxy
// has a token configured, a bearer token.
type Server struct {
	cfg     *Config
	token   string
	policy  *Policy
	audit   *Audit
	metrics *metricsSlot
	scm     *scmSlot
	mux     *http.ServeMux
}

// New builds a server from configuration, resolving credentials once.
func New(cfg *Config, log *slog.Logger) (*Server, error) {
	s := &Server{cfg: cfg, policy: newPolicy(cfg.Limits), audit: newAudit(log), mux: http.NewServeMux()}
	tok, err := Credential(cfg.Token)
	if err != nil {
		return nil, err
	}
	s.token = tok
	if cfg.Slots.Metrics != nil {
		if s.metrics, err = newMetrics(cfg.Slots.Metrics); err != nil {
			return nil, err
		}
	}
	if cfg.Slots.SCM != nil {
		if s.scm, err = newSCM(cfg.Slots.SCM); err != nil {
			return nil, err
		}
	}
	s.routes()
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Fleet lets the connectors describe the clusters to the model.
func (s *Server) Fleet() Fleet { return s.cfg.Fleet }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.mux.HandleFunc("GET /v1/fleet", s.guard("fleet", "get", func(ctx context.Context, r *http.Request) (any, int, error) {
		return s.cfg.Fleet, 200, nil
	}))

	// metrics: five reads
	s.mux.HandleFunc("POST /v1/metrics/query", s.guard("metrics", "query", func(ctx context.Context, r *http.Request) (any, int, error) {
		if s.metrics == nil {
			return nil, 501, errors.New("metrics slot is not bound")
		}
		var in queryReq
		if err := decode(r, &in); err != nil {
			return nil, 400, err
		}
		return s.raw(s.metrics.query(ctx, in))
	}))
	s.mux.HandleFunc("POST /v1/metrics/query_range", s.guard("metrics", "query_range", func(ctx context.Context, r *http.Request) (any, int, error) {
		if s.metrics == nil {
			return nil, 501, errors.New("metrics slot is not bound")
		}
		var in queryRangeReq
		if err := decode(r, &in); err != nil {
			return nil, 400, err
		}
		return s.raw(s.metrics.queryRange(ctx, s.policy, in))
	}))
	s.mux.HandleFunc("POST /v1/metrics/series", s.guard("metrics", "series", func(ctx context.Context, r *http.Request) (any, int, error) {
		if s.metrics == nil {
			return nil, 501, errors.New("metrics slot is not bound")
		}
		var in seriesReq
		if err := decode(r, &in); err != nil {
			return nil, 400, err
		}
		return s.raw(s.metrics.series(ctx, s.policy, in))
	}))
	s.mux.HandleFunc("POST /v1/metrics/label_values", s.guard("metrics", "label_values", func(ctx context.Context, r *http.Request) (any, int, error) {
		if s.metrics == nil {
			return nil, 501, errors.New("metrics slot is not bound")
		}
		var in labelValuesReq
		if err := decode(r, &in); err != nil {
			return nil, 400, err
		}
		return s.raw(s.metrics.labelValues(ctx, in))
	}))
	s.mux.HandleFunc("POST /v1/metrics/rules", s.guard("metrics", "rules", func(ctx context.Context, r *http.Request) (any, int, error) {
		if s.metrics == nil {
			return nil, 501, errors.New("metrics slot is not bound")
		}
		return s.raw(s.metrics.rules(ctx))
	}))

	// scm: reads
	s.mux.HandleFunc("POST /v1/scm/search_code", s.guard("scm", "search_code", func(ctx context.Context, r *http.Request) (any, int, error) {
		if s.scm == nil {
			return nil, 501, errors.New("scm slot is not bound")
		}
		var in searchCodeReq
		if err := decode(r, &in); err != nil {
			return nil, 400, err
		}
		return s.scm.searchCode(ctx, in)
	}))
	s.mux.HandleFunc("POST /v1/scm/repos", s.guard("scm", "repos", func(ctx context.Context, r *http.Request) (any, int, error) {
		if s.scm == nil {
			return nil, 501, errors.New("scm slot is not bound")
		}
		return s.scm.repos(ctx)
	}))
	s.mux.HandleFunc("POST /v1/scm/read", s.guard("scm", "read", func(ctx context.Context, r *http.Request) (any, int, error) {
		if s.scm == nil {
			return nil, 501, errors.New("scm slot is not bound")
		}
		var in readReq
		if err := decode(r, &in); err != nil {
			return nil, 400, err
		}
		return s.scm.read(ctx, in)
	}))
	s.mux.HandleFunc("POST /v1/scm/pr", s.guard("scm", "pr", func(ctx context.Context, r *http.Request) (any, int, error) {
		if s.scm == nil {
			return nil, 501, errors.New("scm slot is not bound")
		}
		var in prReq
		if err := decode(r, &in); err != nil {
			return nil, 400, err
		}
		return s.scm.pr(ctx, in)
	}))
	s.mux.HandleFunc("POST /v1/scm/open_prs", s.guard("scm", "open_prs", func(ctx context.Context, r *http.Request) (any, int, error) {
		if s.scm == nil {
			return nil, 501, errors.New("scm slot is not bound")
		}
		var in openPRReq
		if err := decode(r, &in); err != nil {
			return nil, 400, err
		}
		return s.scm.openPRs(ctx, in)
	}))
	// The tarball is streamed, not JSON, so it has its own handler.
	s.mux.HandleFunc("GET /v1/scm/tarball", s.tarballHandler)

	// scm: the three writes, refused until the GitHub surface milestone.
	for _, op := range []string{"comment", "check", "propose"} {
		op := op
		s.mux.HandleFunc("POST /v1/scm/"+op, s.guard("scm", op, func(ctx context.Context, r *http.Request) (any, int, error) {
			return nil, 501, fmt.Errorf("scm.%s is not implemented yet; in this version the CLI prints questions and proposals", op)
		}))
	}
}

type handler func(ctx context.Context, r *http.Request) (any, int, error)

// guard is the one path every call takes: authenticate, admit against
// the session's budget, run, clip, audit.
func (s *Server) guard(slot, op string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		session := r.Header.Get("X-Session-ID")
		if err := s.authenticate(r); err != nil {
			s.audit.call(session, slot, op, "", 401, 0, time.Since(started), err)
			writeErr(w, 401, err)
			return
		}
		if err := s.policy.admit(session); err != nil {
			s.audit.call(session, slot, op, "", 429, 0, time.Since(started), err)
			writeErr(w, 429, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		out, status, err := h(ctx, r)
		if err != nil {
			if status == 0 || status/100 == 2 {
				status = 502
			}
			s.audit.call(session, slot, op, "", status, 0, time.Since(started), err)
			writeErr(w, status, err)
			return
		}
		if status == 0 {
			status = 200
		}
		// Raw backend bodies pass through clipped; structured results are encoded.
		if raw, ok := out.(rawBody); ok {
			b, truncated := s.policy.clip(raw.body)
			w.Header().Set("Content-Type", "application/json")
			if truncated {
				w.Header().Set("X-Truncated", "true")
			}
			w.WriteHeader(status)
			_, _ = w.Write(b)
			s.audit.call(session, slot, op, raw.summary, status, len(b), time.Since(started), nil)
			return
		}
		var buf bytes.Buffer
		writeJSONTo(&buf, out)
		b, truncated := s.policy.clip(buf.Bytes())
		w.Header().Set("Content-Type", "application/json")
		if truncated {
			w.Header().Set("X-Truncated", "true")
		}
		w.WriteHeader(status)
		_, _ = w.Write(b)
		s.audit.call(session, slot, op, summarize(out), status, len(b), time.Since(started), nil)
	}
}

// rawBody carries a backend's own JSON through unchanged.
type rawBody struct {
	body    []byte
	summary string
}

func (s *Server) raw(b []byte, status int, err error) (any, int, error) {
	if err != nil {
		return nil, status, err
	}
	return rawBody{body: b, summary: fmt.Sprintf("%d bytes", len(b))}, status, nil
}

func (s *Server) authenticate(r *http.Request) error {
	if s.token == "" {
		return nil
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
		return errors.New("unauthorized")
	}
	return nil
}

func (s *Server) tarballHandler(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	session := r.Header.Get("X-Session-ID")
	if err := s.authenticate(r); err != nil {
		writeErr(w, 401, err)
		return
	}
	if err := s.policy.admit(session); err != nil {
		writeErr(w, 429, err)
		return
	}
	if s.scm == nil {
		writeErr(w, 501, errors.New("scm slot is not bound"))
		return
	}
	repo, ref := r.URL.Query().Get("repo"), r.URL.Query().Get("ref")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	// Buffer so a failure can still be reported as an error status.
	var buf bytes.Buffer
	status, n, err := s.scm.tarball(ctx, repo, ref, s.cfg.Limits.TarballBytesMax, &buf)
	if err != nil {
		if status == 0 || status/100 == 2 {
			status = 502
		}
		s.audit.call(session, "scm", "tarball", repo+"@"+ref, status, 0, time.Since(started), err)
		writeErr(w, status, err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(200)
	_, _ = w.Write(buf.Bytes())
	s.audit.call(session, "scm", "tarball", repo+"@"+ref, 200, int(n), time.Since(started), nil)
}

// Forget releases a session's counters once the controller is done with it.
func (s *Server) Forget(session string) { s.policy.forget(session) }

func writeJSONTo(buf *bytes.Buffer, v any) {
	enc := jsonEncoder(buf)
	_ = enc.Encode(v)
}
