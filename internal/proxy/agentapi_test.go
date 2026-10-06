package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The platform fake records what reached it: path and Authorization.
func fakePlatform(t *testing.T) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization")+" apikey="+r.Header.Get("X-Api-Key")+" ptok="+r.Header.Get("X-Proxy-Token"))
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func agentCall(t *testing.T, ts *httptest.Server, method, path, bearer, proxyToken string) int {
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader("{}"))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if proxyToken != "" {
		req.Header.Set("X-Proxy-Token", proxyToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestAgentAPIForwardsOnlyTheWorkersCalls(t *testing.T) {
	srv, ts, _, audit := newTestServer(t, Limits{})
	platform, seen := fakePlatform(t)
	if err := srv.MountAgentAPI(AgentAPI{API: platform.URL, EnvironmentID: "env_1", EnvironmentKey: "env-key-secret"}); err != nil {
		t.Fatal(err)
	}

	// The worker polls with the placeholder; the platform sees the key.
	if got := agentCall(t, ts, "GET", "/anthropic/v1/environments/env_1/work/poll?beta=true", Placeholder, "proxy-secret"); got != 200 {
		t.Fatalf("poll: %d", got)
	}
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "auth=Bearer env-key-secret") || strings.Contains((*seen)[0], "ptok=proxy-secret") || !strings.Contains((*seen)[0], "apikey= ") {
		t.Fatalf("platform saw %v", *seen)
	}
	// Session calls carry the per-item token and pass through unchanged.
	agentCall(t, ts, "GET", "/anthropic/v1/sessions/sesn_1/events/stream?beta=true", "sessions-token-xyz", "proxy-secret")
	if !strings.Contains((*seen)[1], "auth=Bearer sessions-token-xyz") {
		t.Fatalf("session token must pass through: %v", (*seen)[1])
	}
	agentCall(t, ts, "POST", "/anthropic/v1/environments/env_1/work/wrk_1/heartbeat?beta=true", Placeholder, "proxy-secret")
	agentCall(t, ts, "GET", "/anthropic/v1/sessions/sesn_1/threads/sthr_1/events?beta=true", "sessions-token-xyz", "proxy-secret")
	if len(*seen) != 4 {
		t.Fatalf("expected four forwarded calls, platform saw %d", len(*seen))
	}

	// Everything else is refused before it leaves the proxy.
	for _, tc := range []struct{ method, path string }{
		{"POST", "/anthropic/v1/sessions?beta=true"},                       // creating sessions spends money
		{"GET", "/anthropic/v1/sessions?beta=true"},                        // listing other sessions
		{"GET", "/anthropic/v1/environments/env_2/work/poll?beta=true"},    // another environment
		{"POST", "/anthropic/v1/agents?beta=true"},                         // agent management
		{"GET", "/anthropic/v1/skills/sk_1/versions/1/download?beta=true"}, // downloads
		{"POST", "/anthropic/v1/messages"},                                 // the messages API
		{"DELETE", "/anthropic/v1/sessions/sesn_1?beta=true"},
	} {
		if got := agentCall(t, ts, tc.method, tc.path, Placeholder, "proxy-secret"); got != 403 {
			t.Errorf("%s %s: want 403, got %d", tc.method, tc.path, got)
		}
	}
	if len(*seen) != 4 {
		t.Fatalf("refused calls reached the platform: %v", (*seen)[4:])
	}
	// Without the proxy token nothing is forwarded, allowed path or not.
	if got := agentCall(t, ts, "GET", "/anthropic/v1/environments/env_1/work/poll?beta=true", Placeholder, ""); got != 401 {
		t.Errorf("no proxy token: want 401, got %d", got)
	}
	if got := agentCall(t, ts, "GET", "/anthropic/v1/environments/env_1/work/poll?beta=true", Placeholder, "wrong"); got != 401 {
		t.Errorf("wrong proxy token: want 401, got %d", got)
	}
	if strings.Contains(audit.String(), "env-key-secret") || strings.Contains(audit.String(), "sessions-token-xyz") {
		t.Error("the audit log must not carry credentials")
	}
	if !strings.Contains(audit.String(), "slot=agent") {
		t.Error("agent calls are audited")
	}
}
