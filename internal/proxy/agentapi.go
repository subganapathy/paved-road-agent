package proxy

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// AgentAPI is the one slot that faces the agent platform rather than the
// fleet. The worker in the sandbox pod needs to claim work, heartbeat
// its lease, and serve a session's event stream; it does not need to
// hold the credential that authorizes any of that. So the sandbox's
// client points at the proxy, and the proxy forwards an allowlisted set
// of paths to the platform, swapping the placeholder credential for the
// environment key. Session calls carry a per-item sessions token the
// platform issued with the work item, which passes through unchanged.
//
// The sandbox thereby holds nothing: no API key, no environment key,
// and one network peer.
type AgentAPI struct {
	// API is the platform base, https://api.anthropic.com by default.
	API string
	// EnvironmentID restricts the work endpoints to one environment.
	EnvironmentID string
	// EnvironmentKey is injected where the worker sent Placeholder.
	EnvironmentKey string
}

// Placeholder is the bearer the sandbox worker sends in place of a key.
const Placeholder = "proxied"

type route struct {
	method string
	path   *regexp.Regexp
}

// routes is everything the worker needs and nothing else: no session
// creation, no agent or environment management, no listing of other
// sessions, no skill downloads, no memory stores. Methods are the SDK's
// (poll is a long-polling GET).
func (a *AgentAPI) routes() []route {
	env := regexp.QuoteMeta(a.EnvironmentID)
	return []route{
		{"GET", regexp.MustCompile(`^/v1/environments/` + env + `/work/poll$`)},
		{"GET", regexp.MustCompile(`^/v1/environments/` + env + `/work$`)},
		{"GET", regexp.MustCompile(`^/v1/environments/` + env + `/work/[A-Za-z0-9_-]+$`)},
		{"POST", regexp.MustCompile(`^/v1/environments/` + env + `/work/[A-Za-z0-9_-]+$`)}, // release / discard
		{"POST", regexp.MustCompile(`^/v1/environments/` + env + `/work/[A-Za-z0-9_-]+/(ack|heartbeat|stop)$`)},
		{"GET", regexp.MustCompile(`^/v1/sessions/[A-Za-z0-9_-]+$`)},
		{"GET", regexp.MustCompile(`^/v1/sessions/[A-Za-z0-9_-]+/events$`)},
		{"POST", regexp.MustCompile(`^/v1/sessions/[A-Za-z0-9_-]+/events$`)},
		{"GET", regexp.MustCompile(`^/v1/sessions/[A-Za-z0-9_-]+/events/stream$`)},
		{"GET", regexp.MustCompile(`^/v1/sessions/[A-Za-z0-9_-]+/threads/[A-Za-z0-9_-]+/(events|stream)$`)},
	}
}

func (a *AgentAPI) allowed(method, path string) bool {
	for _, r := range a.routes() {
		if r.method == method && r.path.MatchString(path) {
			return true
		}
	}
	return false
}

// AgentAPIPrefix is where the slot is mounted on the proxy; the sandbox
// worker's client uses <proxy>/anthropic as its base URL.
const AgentAPIPrefix = "/anthropic"

// MountAgentAPI adds the slot to a server. The worker authenticates to
// the proxy with X-Proxy-Token, because Authorization carries the
// platform credential.
func (s *Server) MountAgentAPI(a AgentAPI) error {
	h, err := a.handler(AgentAPIPrefix, s)
	if err != nil {
		return err
	}
	s.mux.Handle(AgentAPIPrefix+"/", h)
	return nil
}

// handler returns the reverse proxy mounted under prefix.
func (a *AgentAPI) handler(prefix string, s *Server) (http.Handler, error) {
	if a.EnvironmentID == "" || a.EnvironmentKey == "" {
		return nil, fmt.Errorf("agent api: environment id and key are required")
	}
	api := a.API
	if api == "" {
		api = "https://api.anthropic.com"
	}
	target, err := url.Parse(api)
	if err != nil {
		return nil, err
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
			pr.Out.URL.RawPath = ""
			// The credential swap. Anything else the worker sent as its
			// own header passes through.
			if strings.TrimSpace(pr.In.Header.Get("Authorization")) == "Bearer "+Placeholder {
				pr.Out.Header.Set("Authorization", "Bearer "+a.EnvironmentKey)
			}
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Del("X-Proxy-Token")
		},
		FlushInterval: -1, // event streams are SSE
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		path := strings.TrimPrefix(r.URL.Path, prefix)
		summary := r.Method + " " + path
		if s.token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Proxy-Token")), []byte(s.token)) != 1 {
			s.audit.call("", "agent", "forward", summary, 401, 0, time.Since(start), errors.New("unauthorized"))
			http.Error(w, "unauthorized", 401)
			return
		}
		if !a.allowed(r.Method, path) {
			s.audit.call("", "agent", "forward", summary, 403, 0, time.Since(start), errors.New("not an allowed path"))
			http.Error(w, "the sandbox may not call this", 403)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		rp.ServeHTTP(rec, r)
		s.audit.call("", "agent", "forward", summary, rec.status, 0, time.Since(start), nil)
	}), nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush keeps SSE streaming through the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
