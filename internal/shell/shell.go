// Package shell separates the model's shell from the worker's credentials.
//
// In the sandbox pod the worker runs in one container and the shell in
// another. The shell container has no environment variables worth
// having, no service-account token, no process view of the worker, and a
// network that reaches exactly one address — the proxy — which refuses
// it for lack of a token. The worker forwards each bash call to the
// sidecar over the pod's loopback; the two share only the working
// directory the repositories are unpacked into.
//
// So the agent keeps a real, persistent shell, and what the shell can
// reach is decided by the pod, not by anything the model could read.
package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/tools/agenttoolset"
)

// ExecRequest is one bash call, in the built-in tool's own terms.
type ExecRequest struct {
	Session   string `json:"session"`
	Command   string `json:"command"`
	Restart   bool   `json:"restart"`
	TimeoutMs int64  `json:"timeout_ms"`
}

// ExecResponse is the result as the built-in tool would report it.
type ExecResponse struct {
	Output  string `json:"output"`
	IsError bool   `json:"is_error"`
}

// ---- sidecar ---------------------------------------------------------------

// Server is the sidecar: one persistent shell per session, each in its
// own directory under Workdir. Sessions are closed explicitly by the
// worker or when idle for MaxIdle.
type Server struct {
	Workdir string
	MaxIdle time.Duration
	Logger  *slog.Logger

	mu     sync.Mutex
	shells map[string]*held
}

type held struct {
	sess *agenttoolset.BashSession
	used time.Time
}

var sessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// Handler serves /v1/exec, /v1/close and /healthz.
func (s *Server) Handler() http.Handler {
	if s.MaxIdle == 0 {
		s.MaxIdle = 15 * time.Minute
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	s.shells = map[string]*held{}
	go s.reap()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) })
	mux.HandleFunc("POST /v1/exec", s.exec)
	mux.HandleFunc("POST /v1/close", s.close)
	return mux
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	var in ExecRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || !sessionID.MatchString(in.Session) {
		http.Error(w, "exec: a session id and a command are required", 400)
		return
	}
	out, isErr := s.run(r.Context(), in)
	json.NewEncoder(w).Encode(ExecResponse{Output: out, IsError: isErr})
}

func (s *Server) close(w http.ResponseWriter, r *http.Request) {
	var in struct{ Session string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil || !sessionID.MatchString(in.Session) {
		http.Error(w, "close: a session id is required", 400)
		return
	}
	s.drop(in.Session)
	w.WriteHeader(204)
}

// run mirrors the built-in bash tool: restart, require a command, run
// it with the timeout, restart the shell after any failure that leaves
// it in an unknown state.
func (s *Server) run(ctx context.Context, in ExecRequest) (string, bool) {
	if in.Restart {
		s.drop(in.Session)
		if in.Command == "" {
			return "bash session restarted", false
		}
	}
	if in.Command == "" {
		return "bash: command is required", true
	}
	sess, err := s.session(in.Session)
	if err != nil {
		return "bash: " + err.Error(), true
	}
	out, code, err := sess.Exec(ctx, in.Command, time.Duration(in.TimeoutMs)*time.Millisecond)
	if err != nil {
		s.drop(in.Session)
		if errors.Is(err, agenttoolset.ErrTimedOut) {
			return out + "\nsession restarted after timeout", true
		}
		return "bash: " + err.Error(), true
	}
	if code != 0 {
		return fmt.Sprintf("%s\nexit code: %d", out, code), true
	}
	return out, false
}

func (s *Server) session(id string) (*agenttoolset.BashSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.shells[id]; ok {
		h.used = time.Now()
		return h.sess, nil
	}
	dir := filepath.Join(s.Workdir, id)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	// The shell's whole environment. Nothing here is a secret, and
	// nothing from the sidecar's own environment leaks in.
	sess, err := agenttoolset.NewBashSession(dir, map[string]string{
		"PATH": "/usr/local/bin:/usr/bin:/bin",
		"HOME": dir,
		"TERM": "dumb",
		"LANG": "C.UTF-8",
	})
	if err != nil {
		return nil, err
	}
	s.shells[id] = &held{sess: sess, used: time.Now()}
	s.Logger.Info("shell opened", "session", id)
	return sess, nil
}

func (s *Server) drop(id string) {
	s.mu.Lock()
	h, ok := s.shells[id]
	delete(s.shells, id)
	s.mu.Unlock()
	if ok {
		_ = h.sess.Close()
		s.Logger.Info("shell closed", "session", id)
	}
}

func (s *Server) reap() {
	for range time.Tick(time.Minute) {
		s.mu.Lock()
		var stale []string
		for id, h := range s.shells {
			if time.Since(h.used) > s.MaxIdle {
				stale = append(stale, id)
			}
		}
		s.mu.Unlock()
		for _, id := range stale {
			s.drop(id)
		}
	}
}

// ---- worker side -----------------------------------------------------------

// Tool is the worker's "bash": the built-in tool's name and schema, with
// execution forwarded to the sidecar. It implements anthropic.BetaTool.
type Tool struct {
	Base    string // the sidecar, e.g. http://127.0.0.1:9471
	Session string
	HTTP    *http.Client
}

func (t *Tool) Name() string { return "bash" }

func (t *Tool) Description() string {
	return "Run a bash command in a persistent shell. State (cwd, env vars) persists across calls."
}

func (t *Tool) InputSchema() anthropic.BetaToolInputSchemaParam {
	return anthropic.BetaToolInputSchemaParam{
		Type: "object",
		Properties: map[string]any{
			"command":    map[string]any{"type": "string", "description": "The command to run"},
			"restart":    map[string]any{"type": "boolean", "description": "Restart the persistent shell before running"},
			"timeout_ms": map[string]any{"type": "integer", "description": "Per-call timeout in milliseconds"},
		},
	}
}

// Execute forwards the call. A non-zero exit or a sidecar failure is a
// tool error, as with the built-in.
func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in anthropic.BetaManagedAgentsAgentToolset20260401BashInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, &agenttoolset.ToolError{Content: fmt.Sprintf("invalid bash input: %v", err)}
	}
	req := ExecRequest{Session: t.Session, Command: in.Command, Restart: in.Restart, TimeoutMs: in.TimeoutMs}
	var resp ExecResponse
	if err := t.post(ctx, "/v1/exec", req, &resp); err != nil {
		return nil, &agenttoolset.ToolError{Content: "bash: the shell is unavailable: " + err.Error()}
	}
	if resp.IsError {
		return nil, &agenttoolset.ToolError{Content: resp.Output}
	}
	return []anthropic.BetaToolResultBlockParamContentUnion{{OfText: &anthropic.BetaTextBlockParam{Text: resp.Output}}}, nil
}

// Close ends the session's shell in the sidecar.
func (t *Tool) Close() error {
	return t.post(context.Background(), "/v1/close", struct{ Session string }{t.Session}, nil)
}

func (t *Tool) post(ctx context.Context, path string, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(t.Base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := t.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("sidecar: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Unavailable is the "bash" a worker outside the sandbox serves: the
// same tool, refusing every call with a reason the model can act on
// (use read, grep and glob; the shell exists only in the sandbox pod).
type Unavailable struct{}

func (Unavailable) Name() string { return "bash" }
func (Unavailable) Description() string {
	return "Run a bash command in a persistent shell. State (cwd, env vars) persists across calls."
}
func (Unavailable) InputSchema() anthropic.BetaToolInputSchemaParam {
	return (&Tool{}).InputSchema()
}
func (Unavailable) Execute(context.Context, json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	return nil, &agenttoolset.ToolError{Content: "bash is not available in this environment: this worker runs outside the sandbox pod. Use read, grep and glob on the mounted repository instead, and do not retry bash."}
}
