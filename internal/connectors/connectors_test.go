package connectors

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/subganapathy/paved-road-agent/internal/proxy"
)

// tarball builds a GitHub-style archive: one top-level dir, files under it,
// plus a path-traversal entry that must be dropped.
func tarball(t *testing.T) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.WriteHeader(&tar.Header{Name: "acme-hello-abc123/", Typeflag: tar.TypeDir, Mode: 0o755})
	add("acme-hello-abc123/go.mod", "module hello\n")
	add("acme-hello-abc123/.paved-agent/discover.yaml", "service: hello\n")
	add("acme-hello-abc123/../escape.txt", "nope\n")
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func fakes(t *testing.T) (prom, gh *httptest.Server) {
	prom = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/label/__name__/values":
			w.Write([]byte(`{"status":"success","data":["kube_pod_info","istio_requests_total"]}`))
		default:
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tarball/main"):
			w.Write(tarball(t))
		case r.URL.Path == "/search/code":
			w.Write([]byte(`{"total_count":0,"items":[]}`))
		default:
			http.Error(w, r.URL.Path, 404)
		}
	}))
	t.Cleanup(prom.Close)
	t.Cleanup(gh.Close)
	return
}

func newProxy(t *testing.T) *httptest.Server {
	prom, gh := fakes(t)
	cfg := &proxy.Config{
		Fleet: proxy.Fleet{ClusterLabel: "cluster", Clusters: []proxy.Cluster{{ID: "prod", Env: "prod"}}},
		Slots: proxy.Slots{Metrics: &proxy.MetricsSlot{Kind: "promql", Endpoint: prom.URL}, SCM: &proxy.SCMSlot{Kind: "github", Org: "acme", API: gh.URL}},
	}
	// Defaults are applied by Load; mirror the ones the tests rely on.
	cfg.Limits.ResultBytesMax = 256 << 10
	cfg.Limits.CallsPerSessionMax = 100
	cfg.Limits.QueryRangeMax = 7 * 24 * 3600e9
	cfg.Limits.TarballBytesMax = 10 << 20
	srv, err := proxy.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func run(t *testing.T, reg []anthropic.BetaTool, name string, in any) string {
	for _, tool := range reg {
		if tool.Name() != name {
			continue
		}
		b, _ := json.Marshal(in)
		out, err := tool.Execute(context.Background(), b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(out) == 0 || out[0].OfText == nil {
			t.Fatalf("%s: no text result", name)
		}
		return out[0].OfText.Text
	}
	t.Fatalf("no tool %s", name)
	return ""
}

func TestToolsThroughTheProxy(t *testing.T) {
	ts := newProxy(t)
	work := t.TempDir()
	reg, err := Tools(&Client{Base: ts.URL, Session: "s1"}, work)
	if err != nil {
		t.Fatal(err)
	}
	if out := run(t, reg, Fleet, emptyIn{}); !strings.Contains(out, `"env":"prod"`) {
		t.Errorf("fleet: %s", out)
	}
	if out := run(t, reg, MetricsLabelVals, labelValuesIn{Label: "__name__"}); !strings.Contains(out, "istio_requests_total") {
		t.Errorf("label_values: %s", out)
	}
	out := run(t, reg, SCMMount, mountIn{Repo: "hello", Ref: "main"})
	if !strings.Contains(out, "mounted at "+filepath.Join(work, "hello")) || !strings.Contains(out, ".paved-agent/") {
		t.Errorf("mount: %s", out)
	}
	if _, err := os.Stat(filepath.Join(work, "hello", ".paved-agent", "discover.yaml")); err != nil {
		t.Errorf("unpacked file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "escape.txt")); err == nil {
		t.Error("path traversal entry was written")
	}
}

func TestUnavailableIsEvidenceNotAnError(t *testing.T) {
	reg, err := Tools(&Client{Base: "http://127.0.0.1:1", Session: "s1"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := run(t, reg, MetricsQuery, queryIn{Query: "up"})
	if !strings.HasPrefix(out, "unavailable:") {
		t.Errorf("want an 'unavailable' result the model can reason about, got %q", out)
	}
}

func TestDefinitionsMatchSets(t *testing.T) {
	reg, err := Tools(&Client{Base: "http://proxy", Session: "s"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for agent, names := range Sets {
		defs, err := Definitions(reg, names)
		if err != nil {
			t.Fatalf("%s: %v", agent, err)
		}
		if len(defs) != len(names) {
			t.Errorf("%s: %d definitions for %d names", agent, len(defs), len(names))
		}
		for _, d := range defs {
			if d.OfCustom == nil || d.OfCustom.Name == "" || d.OfCustom.Description == "" {
				t.Errorf("%s: incomplete definition %+v", agent, d)
			}
		}
	}
}
