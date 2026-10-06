package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakePrometheus answers the five read endpoints and records what it saw.
func fakePrometheus(t *testing.T) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("Authorization") != "Bearer prom-secret" {
			http.Error(w, "no token", 401)
			return
		}
		switch r.URL.Path {
		case "/api/v1/label/__name__/values":
			w.Write([]byte(`{"status":"success","data":["kube_pod_info","istio_requests_total","up"]}`))
		case "/api/v1/label/long/values":
			var names []string
			for i := 0; i < 400; i++ {
				names = append(names, fmt.Sprintf("%s_metric_%03d", []string{"kube", "istio", "envoy", "go"}[i%4], i))
			}
			b, _ := json.Marshal(map[string]any{"status": "success", "data": names})
			w.Write(b)
		case "/api/v1/query":
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"cluster":"prod"},"value":[1,"2"]}]}}`))
		case "/api/v1/query_range", "/api/v1/series", "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{}}`))
		default:
			http.Error(w, "not a read endpoint", 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// fakeGitHub serves one org with one repo and one file.
func fakeGitHub(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-secret" {
			http.Error(w, "no token", 401)
			return
		}
		switch {
		case r.URL.Path == "/search/code":
			q := r.URL.Query().Get("q")
			if !strings.Contains(q, "org:acme") && !strings.Contains(q, "repo:acme/") {
				http.Error(w, "search not scoped to the org: "+q, 400)
				return
			}
			w.Write([]byte(`{"total_count":1,"items":[{"path":"charts/mesh/values.yaml","repository":{"full_name":"acme/platform"}}]}`))
		case r.URL.Path == "/repos/acme/hello/contents/.paved-agent/discover.yaml":
			content := base64.StdEncoding.EncodeToString([]byte("service: hello\n"))
			fmt.Fprintf(w, `{"content":%q,"encoding":"base64","size":15,"sha":"abc"}`, content)
		case r.URL.Path == "/repos/acme/hello/tarball/main":
			w.Header().Set("Content-Type", "application/gzip")
			w.Write(bytes.Repeat([]byte{0x1f, 0x8b, 0, 0}, 64))
		case r.URL.Path == "/repos/acme/hello/pulls":
			w.Write([]byte(`[{"number":7,"title":"x","user":{"login":"a"},"head":{"sha":"deadbeef"},"updated_at":"2026-10-05T00:00:00Z"}]`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestServer(t *testing.T, limits Limits) (*Server, *httptest.Server, *[]string, *bytes.Buffer) {
	prom, seen := fakePrometheus(t)
	gh := fakeGitHub(t)
	t.Setenv("PROM_TOKEN", "prom-secret")
	t.Setenv("GH_TOKEN_TEST", "gh-secret")
	t.Setenv("PROXY_TOKEN", "proxy-secret")
	cfg := &Config{
		Token:  "env:PROXY_TOKEN",
		Fleet:  Fleet{Clusters: []Cluster{{ID: "prod", Env: "prod"}}},
		Slots:  Slots{Metrics: &MetricsSlot{Kind: "promql", Endpoint: prom.URL, Auth: "env:PROM_TOKEN"}, SCM: &SCMSlot{Kind: "github", Org: "acme", Auth: "env:GH_TOKEN_TEST", API: gh.URL}},
		Limits: limits,
	}
	cfg.defaults()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	var auditBuf bytes.Buffer
	srv, err := New(cfg, slog.New(slog.NewTextHandler(&auditBuf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts, seen, &auditBuf
}

func call(t *testing.T, ts *httptest.Server, method, path string, body any, session string) (int, http.Header, []byte) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rdr)
	req.Header.Set("Authorization", "Bearer proxy-secret")
	if session != "" {
		req.Header.Set("X-Session-ID", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func TestMetricsReadsPassThroughWithAuth(t *testing.T) {
	_, ts, seen, _ := newTestServer(t, Limits{})
	status, _, body := call(t, ts, "POST", "/v1/metrics/label_values", map[string]any{"label": "__name__"}, "s1")
	if status != 200 || !strings.Contains(string(body), "istio_requests_total") {
		t.Fatalf("label_values: %d %s", status, body)
	}
	status, _, body = call(t, ts, "POST", "/v1/metrics/query", map[string]any{"query": "up"}, "s1")
	if status != 200 || !strings.Contains(string(body), `"cluster":"prod"`) {
		t.Fatalf("query: %d %s", status, body)
	}
	if len(*seen) != 2 || !strings.HasPrefix((*seen)[0], "/api/v1/label/__name__/values") {
		t.Errorf("backend saw %v", *seen)
	}
}

func TestLongLabelListingsAreGrouped(t *testing.T) {
	_, ts, _, _ := newTestServer(t, Limits{})
	status, _, body := call(t, ts, "POST", "/v1/metrics/label_values", map[string]any{"label": "long"}, "s1")
	if status != 200 || !strings.Contains(string(body), `"families"`) || strings.Contains(string(body), "kube_metric_396") {
		t.Fatalf("a long listing is grouped by prefix, not listed: %d %.300s", status, body)
	}
	if !strings.Contains(string(body), `"count":400`) || !strings.Contains(string(body), "istio (100)") {
		t.Errorf("groups carry counts: %.400s", body)
	}
	status, _, body = call(t, ts, "POST", "/v1/metrics/label_values", map[string]any{"label": "long", "contains": "envoy_"}, "s1")
	if status != 200 || !strings.Contains(string(body), `"count":100`) || !strings.Contains(string(body), "envoy_metric_398") || strings.Contains(string(body), "kube_") {
		t.Fatalf("contains narrows to one family, listed in full: %d %.300s", status, body)
	}
}

func TestRangeLimitIsEnforced(t *testing.T) {
	_, ts, seen, _ := newTestServer(t, Limits{QueryRangeMax: time.Hour})
	start, end := time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)
	status, _, body := call(t, ts, "POST", "/v1/metrics/query_range", map[string]any{"query": "up", "start": start, "end": end}, "s1")
	if status != 502 && status != 400 {
		t.Fatalf("expected a refusal, got %d %s", status, body)
	}
	if len(*seen) != 0 {
		t.Errorf("the backend must not be reached when policy refuses: %v", *seen)
	}
}

func TestResultsAreClipped(t *testing.T) {
	_, ts, _, _ := newTestServer(t, Limits{ResultBytesMax: 20})
	status, hdr, body := call(t, ts, "POST", "/v1/metrics/query", map[string]any{"query": "up"}, "s1")
	if status != 200 || len(body) != 20 || hdr.Get("X-Truncated") != "true" {
		t.Fatalf("clip: %d %d %q", status, len(body), hdr.Get("X-Truncated"))
	}
}

func TestSessionBudget(t *testing.T) {
	srv, ts, _, _ := newTestServer(t, Limits{CallsPerSessionMax: 2})
	for i := 0; i < 2; i++ {
		if status, _, _ := call(t, ts, "GET", "/v1/fleet", nil, "s1"); status != 200 {
			t.Fatalf("call %d: %d", i, status)
		}
	}
	if status, _, _ := call(t, ts, "GET", "/v1/fleet", nil, "s1"); status != 429 {
		t.Fatalf("third call should be refused, got %d", status)
	}
	if status, _, _ := call(t, ts, "GET", "/v1/fleet", nil, "s2"); status != 200 {
		t.Fatalf("another session is unaffected")
	}
	srv.Forget("s1")
	if status, _, _ := call(t, ts, "GET", "/v1/fleet", nil, "s1"); status != 200 {
		t.Fatalf("forgotten session starts fresh")
	}
	if status, _, _ := call(t, ts, "GET", "/v1/fleet", nil, ""); status != 429 {
		t.Fatalf("missing session id must be refused")
	}
}

func TestSCMIsScopedToTheOrg(t *testing.T) {
	_, ts, _, _ := newTestServer(t, Limits{})
	status, _, body := call(t, ts, "POST", "/v1/scm/read", map[string]any{"repo": "evil/hello", "ref": "main", "path": ".paved-agent/discover.yaml"}, "s1")
	if status/100 == 2 {
		t.Fatalf("read outside the org must be refused: %s", body)
	}
	status, _, body = call(t, ts, "POST", "/v1/scm/read", map[string]any{"repo": "hello", "ref": "main", "path": ".paved-agent/discover.yaml"}, "s1")
	if status != 200 || !strings.Contains(string(body), "service: hello") {
		t.Fatalf("read: %d %s", status, body)
	}
	status, _, body = call(t, ts, "POST", "/v1/scm/read", map[string]any{"repo": "hello", "ref": "main", "path": "../etc/passwd"}, "s1")
	if status/100 == 2 {
		t.Fatalf("path traversal must be refused: %s", body)
	}
	status, _, body = call(t, ts, "POST", "/v1/scm/search_code", map[string]any{"query": "mesh"}, "s1")
	if status != 200 || !strings.Contains(string(body), "acme/platform") {
		t.Fatalf("search: %d %s", status, body)
	}
	status, _, body = call(t, ts, "GET", "/v1/scm/tarball?repo=hello&ref=main", nil, "s1")
	if status != 200 || len(body) != 256 {
		t.Fatalf("tarball: %d %d", status, len(body))
	}
	status, _, body = call(t, ts, "POST", "/v1/scm/open_prs", map[string]any{"repo": "hello"}, "s1")
	if status != 200 || !strings.Contains(string(body), `"number":7`) {
		t.Fatalf("open_prs: %d %s", status, body)
	}
}

func TestWritesAreRefusedForNow(t *testing.T) {
	_, ts, _, _ := newTestServer(t, Limits{})
	for _, op := range []string{"comment", "check", "propose"} {
		if status, _, _ := call(t, ts, "POST", "/v1/scm/"+op, map[string]any{}, "s1"); status != 501 {
			t.Errorf("%s: want 501, got %d", op, status)
		}
	}
}

func TestProxyTokenAndAuditHygiene(t *testing.T) {
	_, ts, _, audit := newTestServer(t, Limits{})
	req, _ := http.NewRequest("GET", ts.URL+"/v1/fleet", nil)
	req.Header.Set("X-Session-ID", "s1")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("no bearer: want 401, got %d", resp.StatusCode)
	}
	call(t, ts, "POST", "/v1/metrics/query", map[string]any{"query": "up"}, "s1")
	call(t, ts, "POST", "/v1/scm/read", map[string]any{"repo": "hello", "ref": "main", "path": ".paved-agent/discover.yaml"}, "s1")
	a := audit.String()
	for _, secret := range []string{"prom-secret", "gh-secret", "proxy-secret"} {
		if strings.Contains(a, secret) {
			t.Errorf("audit log leaks %q", secret)
		}
	}
	if !strings.Contains(a, "slot=metrics") || !strings.Contains(a, "op=read") {
		t.Errorf("audit log missing calls:\n%s", a)
	}
}

func TestDNSResolveClassifiesShapes(t *testing.T) {
	_, ts, _, _ := newTestServer(t, Limits{})
	for name, shape := range map[string]string{"hello.hello.svc.cluster.local": "cluster-local", "hello.hello.svc": "cluster-local", "api.ns.svc.clusterset.local": "clusterset", "localhost": "external"} {
		status, _, body := call(t, ts, "POST", "/v1/dns/resolve", map[string]any{"name": name}, "s1")
		if status != 200 || !strings.Contains(string(body), `"shape":"`+shape+`"`) {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}
	// localhost resolves anywhere; a cluster-local name says so when it does not.
	_, _, body := call(t, ts, "POST", "/v1/dns/resolve", map[string]any{"name": "localhost"}, "s1")
	if !strings.Contains(string(body), "127.0.0.1") && !strings.Contains(string(body), "::1") {
		t.Errorf("localhost did not resolve: %s", body)
	}
	if status, _, _ := call(t, ts, "POST", "/v1/dns/resolve", map[string]any{"name": "not a name"}, "s1"); status/100 == 2 {
		t.Error("a malformed name must be refused")
	}
}

// fakeGCP serves one bucket, one instance and one forwarding rule.
func fakeGCP(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gcp-token" {
			http.Error(w, "no token", 401)
			return
		}
		switch {
		case r.URL.Path == "/storage/v1/b/ledger-exports":
			w.Write([]byte(`{"name":"ledger-exports","location":"US","versioning":{"enabled":false},"retentionPolicy":{"retentionPeriod":"2592000","isLocked":false},"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true},"publicAccessPrevention":"enforced"}}`))
		case r.URL.Path == "/storage/v1/b/ledger-exports/iam":
			w.Write([]byte(`{"bindings":[{"role":"roles/storage.objectViewer","members":["serviceAccount:reader@acme.iam.gserviceaccount.com"]}]}`))
		case strings.HasPrefix(r.URL.Path, "/storage/v1/b/"):
			http.Error(w, "not found", 404)
		case r.URL.Path == "/compute/v1/projects/acme-data/aggregated/instances":
			w.Write([]byte(`{"items":{"zones/us-central1-a":{"instances":[{"name":"envoy-1","zone":"projects/acme-data/zones/us-central1-a","status":"RUNNING","labels":{"tier":"envoy"},"networkInterfaces":[{"networkIP":"10.9.0.4","network":"projects/acme-data/global/networks/buffer"}]}]}}}`))
		case r.URL.Path == "/compute/v1/projects/acme-data/aggregated/forwardingRules":
			w.Write([]byte(`{"items":{"regions/us-central1":{"forwardingRules":[{"name":"api-ilb","IPAddress":"10.8.0.10","loadBalancingScheme":"INTERNAL","backendService":"projects/acme-data/regions/us-central1/backendServices/api","network":"projects/acme-data/global/networks/prod","region":"projects/acme-data/regions/us-central1"}]}}}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCloudGetGCP(t *testing.T) {
	gcp := fakeGCP(t)
	t.Setenv("GCP_TOKEN_TEST", "gcp-token")
	t.Setenv("PROXY_TOKEN", "proxy-secret")
	cfg := &Config{Token: "env:PROXY_TOKEN", Slots: Slots{Cloud: &CloudSlot{GCP: &GCPSlot{Projects: []string{"acme-data"}, Auth: "env:GCP_TOKEN_TEST", API: gcp.URL}}}}
	cfg.defaults()
	srv, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	cases := map[string]string{
		`{"provider":"gcp","resource":"gs://ledger-exports","attribute":"protection"}`: `"retention_seconds":"2592000"`,
		`{"provider":"gcp","resource":"gs://ledger-exports","attribute":"iam"}`:        `roles/storage.objectViewer`,
		`{"provider":"gcp","resource":"gs://nope","attribute":"exists"}`:               `"exists":false`,
		`{"provider":"gcp","resource":"ip:10.9.0.4","attribute":"owner"}`:              `"owner":"instance"`,
		`{"provider":"gcp","resource":"ip:10.8.0.10","attribute":"owner"}`:             `"owner":"forwarding_rule"`,
		`{"provider":"gcp","resource":"ip:10.0.0.1","attribute":"owner"}`:              `"owner":"not found"`,
	}
	for in, want := range cases {
		var body map[string]any
		_ = json.Unmarshal([]byte(in), &body)
		status, _, out := call(t, ts, "POST", "/v1/cloud/get", body, "s1")
		if status != 200 || !strings.Contains(string(out), want) {
			t.Errorf("%s → %d %s (want %s)", in, status, out, want)
		}
	}
	if status, _, _ := call(t, ts, "POST", "/v1/cloud/get", map[string]any{"provider": "aws", "resource": "x", "attribute": "exists"}, "s1"); status != 400 {
		t.Errorf("an unbound provider must be refused, got %d", status)
	}
}
