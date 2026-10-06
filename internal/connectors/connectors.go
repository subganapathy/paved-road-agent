// Package connectors is the tools' side of the proxy: typed custom tools
// registered on the agents, executed by the sandbox worker as thin HTTP
// clients of the proxy. The model knows tool names and schemas; who
// executes them, and with what identity, is invisible to it.
package connectors

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// Client reaches the proxy for one session.
type Client struct {
	Base    string // http://proxy.paved-agent.svc:8080
	Token   string // the proxy's bearer; the sandbox pod gets it from the worker, never from the model
	Session string
	HTTP    *http.Client
}

// call posts JSON and returns the body, with the proxy's truncation flag
// turned into a trailing note the model can see.
func (c *Client) call(ctx context.Context, method, path string, in any) (string, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return "", err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", c.Session)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return "", fmt.Errorf("proxy unreachable: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("proxy %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	out := string(b)
	if resp.Header.Get("X-Truncated") == "true" {
		out += "\n… (result truncated by the proxy's size limit; narrow the query)"
	}
	return out, nil
}

// Inputs, with the schema descriptions the model reads.
type (
	emptyIn       struct{}
	labelValuesIn struct {
		Label string   `json:"label" jsonschema:"required" jsonschema_description:"The label whose values to list. Use __name__ to list every metric family the fleet exports: that is how you learn what is there before forming queries."`
		Match []string `json:"match,omitempty" jsonschema_description:"Optional series selectors to narrow the listing, e.g. {namespace=\"hello\"}"`
	}
	queryIn struct {
		Query string `json:"query" jsonschema:"required" jsonschema_description:"A PromQL instant query"`
		Time  string `json:"time,omitempty" jsonschema_description:"RFC 3339 or Unix seconds; default now"`
	}
	queryRangeIn struct {
		Query string `json:"query" jsonschema:"required"`
		Start string `json:"start" jsonschema:"required" jsonschema_description:"RFC 3339 or Unix seconds"`
		End   string `json:"end" jsonschema:"required"`
		Step  string `json:"step,omitempty" jsonschema_description:"e.g. 60s; default 60s"`
	}
	seriesIn struct {
		Match []string `json:"match" jsonschema:"required" jsonschema_description:"Series selectors, e.g. kube_pod_info{namespace=\"hello\"}"`
		Start string   `json:"start,omitempty"`
		End   string   `json:"end,omitempty"`
	}
	searchCodeIn struct {
		Query string `json:"query" jsonschema:"required" jsonschema_description:"Code search terms. Searched across the organisation's repositories. Search for names you read in manifests or code: a chart name, a custom resource kind, a container name, an import path."`
		Repo  string `json:"repo,omitempty" jsonschema_description:"Narrow to one repository (name or org/name)"`
	}
	readIn struct {
		Repo string `json:"repo" jsonschema:"required" jsonschema_description:"Repository name or org/name"`
		Ref  string `json:"ref,omitempty" jsonschema_description:"Branch, tag or commit; default the default branch"`
		Path string `json:"path" jsonschema:"required" jsonschema_description:"Path inside the repository"`
	}
	mountIn struct {
		Repo string `json:"repo" jsonschema:"required" jsonschema_description:"Repository name or org/name"`
		Ref  string `json:"ref,omitempty" jsonschema_description:"Branch, tag or commit; default the default branch"`
	}
	prIn struct {
		Repo   string `json:"repo" jsonschema:"required"`
		Number int    `json:"number" jsonschema:"required"`
	}
	openPRsIn struct {
		Repo string `json:"repo" jsonschema:"required"`
	}
)

// Names, as the agents refer to them.
const (
	Fleet             = "fleet"
	MetricsLabelVals  = "metrics_label_values"
	MetricsQuery      = "metrics_query"
	MetricsQueryRange = "metrics_query_range"
	MetricsSeries     = "metrics_series"
	MetricsRules      = "metrics_rules"
	SCMRepos          = "scm_repos"
	SCMSearchCode     = "scm_search_code"
	SCMRead           = "scm_read"
	SCMMount          = "scm_mount"
	SCMPR             = "scm_pr"
	SCMOpenPRs        = "scm_open_prs"
)

// Sets are which connector tools each agent gets.
var Sets = map[string][]string{
	"lead":       {Fleet, SCMPR, SCMOpenPRs, SCMRead, SCMMount, MetricsRules},
	"topology":   {Fleet, MetricsLabelVals, MetricsQuery, MetricsQueryRange, MetricsSeries, MetricsRules, SCMRead},
	"org-finder": {Fleet, SCMRepos, SCMSearchCode, SCMRead, SCMMount},
}

// Tools builds every connector tool bound to one session's client and the
// sandbox's work directory (where scm_mount unpacks repositories).
func Tools(c *Client, workdir string) ([]anthropic.BetaTool, error) {
	text := func(s string) (anthropic.BetaToolResultBlockParamContentUnion, error) {
		return anthropic.BetaToolResultBlockParamContentUnion{OfText: &anthropic.BetaTextBlockParam{Text: s}}, nil
	}
	fail := func(err error) (anthropic.BetaToolResultBlockParamContentUnion, error) {
		// Tool errors are evidence too: the model must see "unavailable",
		// not a plausible number. Returned as text, not as a runner error.
		return text("unavailable: " + err.Error())
	}
	var reg []anthropic.BetaTool
	add := func(t anthropic.BetaTool, err error) error {
		if err != nil {
			return err
		}
		reg = append(reg, t)
		return nil
	}
	for _, err := range []error{
		add(toolrunner.NewBetaToolFromJSONSchema(Fleet,
			"The clusters the environment knows, with their environment (dev, staging, prod) and labels, and the metric label that names a cluster. Identifiers may refer to clusters by id or by environment.",
			func(ctx context.Context, _ emptyIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				out, err := c.call(ctx, "GET", "/v1/fleet", nil)
				if err != nil {
					return fail(err)
				}
				return text(out)
			})),
		add(toolrunner.NewBetaToolFromJSONSchema(MetricsLabelVals,
			"List the values of a metric label. With label __name__ it lists every metric family the fleet's metrics backend has: start stack discovery here and recognise, from the names, which cluster-state exporter, mesh, deployment tool, progressive-delivery controller, autoscaler, policy enforcer and admission engine are present — or absent. Returns the backend's raw JSON.",
			func(ctx context.Context, in labelValuesIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				out, err := c.call(ctx, "POST", "/v1/metrics/label_values", in)
				if err != nil {
					return fail(err)
				}
				return text(out)
			})),
		add(toolrunner.NewBetaToolFromJSONSchema(MetricsQuery,
			"Run a PromQL instant query against the fleet's metrics. Returns one line per series: {label=value,…} value. Form queries from what metrics_label_values showed exists; do not assume a metric family is present. Issue every independent query you need in the same turn.",
			func(ctx context.Context, in queryIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				out, err := c.call(ctx, "POST", "/v1/metrics/query", in)
				if err != nil {
					return fail(err)
				}
				return text(compactVector(out, 60))
			})),
		add(toolrunner.NewBetaToolFromJSONSchema(MetricsQueryRange,
			"Run a PromQL range query (bounded by the proxy's maximum range). Use it for trends; prefer metrics_query for a current value.",
			func(ctx context.Context, in queryRangeIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				out, err := c.call(ctx, "POST", "/v1/metrics/query_range", in)
				if err != nil {
					return fail(err)
				}
				return text(out)
			})),
		add(toolrunner.NewBetaToolFromJSONSchema(MetricsSeries,
			"List the series matching selectors, one per line with their labels: the way to see which label values exist (which clusters, namespaces, workloads) before querying.",
			func(ctx context.Context, in seriesIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				out, err := c.call(ctx, "POST", "/v1/metrics/series", in)
				if err != nil {
					return fail(err)
				}
				return text(compactSeries(out, 80))
			})),
		add(toolrunner.NewBetaToolFromJSONSchema(MetricsRules,
			"The alerting and recording rules the metrics backend evaluates: the way to learn whether an alert watches a signal.",
			func(ctx context.Context, _ emptyIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				out, err := c.call(ctx, "POST", "/v1/metrics/rules", nil)
				if err != nil {
					return fail(err)
				}
				return text(out)
			})),
		add(toolrunner.NewBetaToolFromJSONSchema(SCMRepos,
			"List the organisation's repositories with description, topics, language and last push. Use it when code search returns nothing or reports incomplete results — many organisations are not indexed — and pick candidates by name and topic, then confirm by reading a known file.",
			func(ctx context.Context, _ emptyIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				out, err := c.call(ctx, "POST", "/v1/scm/repos", nil)
				if err != nil {
					return fail(err)
				}
				return text(out)
			})),
		add(toolrunner.NewBetaToolFromJSONSchema(SCMSearchCode,
			"Search code across the organisation's repositories. Returns repository and path per hit. Use names that cannot be renamed by mirroring: custom resource kinds, container names, chart structure, import paths.",
			func(ctx context.Context, in searchCodeIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				out, err := c.call(ctx, "POST", "/v1/scm/search_code", in)
				if err != nil {
					return fail(err)
				}
				return text(out)
			})),
		add(toolrunner.NewBetaToolFromJSONSchema(SCMRead,
			"Read one file from a repository at a ref, without mounting it. Costly: it returns the whole file into your context. When the repository is already mounted in the sandbox (yours or by another agent in this session), use grep and read on it instead; mount it with scm_mount if you will read more than two files.",
			func(ctx context.Context, in readIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				out, err := c.call(ctx, "POST", "/v1/scm/read", in)
				if err != nil {
					return fail(err)
				}
				return text(out)
			})),
		add(toolrunner.NewBetaToolFromJSONSchema(SCMMount,
			"Check out a repository at a ref into the sandbox so it can be read with the file tools. Returns the directory it was unpacked into and its top-level entries.",
			func(ctx context.Context, in mountIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				dir, entries, err := c.Mount(ctx, workdir, in.Repo, in.Ref)
				if err != nil {
					return fail(err)
				}
				return text(fmt.Sprintf("mounted at %s\n%s", dir, strings.Join(entries, "\n")))
			})),
		add(toolrunner.NewBetaToolFromJSONSchema(SCMPR,
			"A pull request as a change: title, description, files with their patches, and both sides of small configuration files.",
			func(ctx context.Context, in prIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				out, err := c.call(ctx, "POST", "/v1/scm/pr", in)
				if err != nil {
					return fail(err)
				}
				return text(out)
			})),
		add(toolrunner.NewBetaToolFromJSONSchema(SCMOpenPRs,
			"The repository's other open pull requests, for changes that are safe alone and unsafe together.",
			func(ctx context.Context, in openPRsIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				out, err := c.call(ctx, "POST", "/v1/scm/open_prs", in)
				if err != nil {
					return fail(err)
				}
				return text(out)
			})),
	} {
		if err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// Query runs one PromQL instant query through the proxy and returns the
// compact one-line-per-series form. The controller uses it to execute
// the measure: queries from .paved-agent/discover.yaml before a session
// starts, so the lead receives numbers instead of spending turns on them.
func (c *Client) Query(ctx context.Context, query string) (string, error) {
	out, err := c.call(ctx, "POST", "/v1/metrics/query", queryIn{Query: query})
	if err != nil {
		return "", err
	}
	return compactVector(out, 60), nil
}

// Mount fetches a repository tarball through the proxy and unpacks it
// under workdir/<repo>. The proxy's token does the fetch; this process
// holds only the proxy's bearer.
func (c *Client) Mount(ctx context.Context, workdir, repo, ref string) (string, []string, error) {
	name := repo
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if name == "" || strings.ContainsAny(name, "./\\") {
		return "", nil, fmt.Errorf("repository name %q is malformed", repo)
	}
	q := "?repo=" + repo
	if ref != "" {
		q += "&ref=" + ref
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.Base, "/")+"/v1/scm/tarball"+q, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("X-Session-ID", c.Session)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := h.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", nil, fmt.Errorf("tarball: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	dest := filepath.Join(workdir, name)
	if err := os.RemoveAll(dest); err != nil {
		return "", nil, err
	}
	if err := untar(resp.Body, dest); err != nil {
		return "", nil, err
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return "", nil, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return dest, names, nil
}

// untar extracts a GitHub-style tarball (one top-level directory, which
// is stripped) into dest, refusing paths that escape it.
func untar(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		parts := strings.SplitN(hdr.Name, "/", 2)
		if len(parts) < 2 || parts[1] == "" {
			continue // the top-level directory itself
		}
		rel := filepath.Clean(parts[1])
		if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			continue
		}
		target := filepath.Join(dest, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777|0o400)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, 64<<20)); err != nil {
				f.Close()
				return err
			}
			f.Close()
		default:
			// symlinks and specials are dropped: nothing in a review needs them
			// and a link out of the tree is exactly what we refuse.
		}
	}
}

// Definitions renders the named tools as agent configuration from the same
// registry the worker serves, so the two cannot drift.
func Definitions(reg []anthropic.BetaTool, names []string) ([]anthropic.BetaAgentNewParamsToolUnion, error) {
	var out []anthropic.BetaAgentNewParamsToolUnion
	for _, n := range names {
		var found *anthropic.BetaTool
		for i := range reg {
			if reg[i].Name() == n {
				found = &reg[i]
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("no connector tool named %s", n)
		}
		schema := (*found).InputSchema()
		props := map[string]any{}
		if schema.Properties != nil {
			b, _ := json.Marshal(schema.Properties)
			_ = json.Unmarshal(b, &props)
		}
		out = append(out, anthropic.BetaAgentNewParamsToolUnion{OfCustom: &anthropic.BetaManagedAgentsCustomToolParams{
			Name:        n,
			Description: (*found).Description(),
			InputSchema: anthropic.BetaManagedAgentsCustomToolInputSchemaParam{Properties: props, Required: schema.Required},
			Type:        anthropic.BetaManagedAgentsCustomToolParamsTypeCustom,
		}})
	}
	return out, nil
}

// compactVector renders a Prometheus instant-query result as one line per
// series, which is what the model needs and a fifth of the JSON.
func compactVector(raw string, maxSeries int) string {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil || resp.Status != "success" || resp.Data.ResultType != "vector" {
		return raw // not a vector (an error, a scalar, a range): pass through
	}
	if len(resp.Data.Result) == 0 {
		return "no data (empty result)"
	}
	var sb strings.Builder
	for i, r := range resp.Data.Result {
		if i >= maxSeries {
			fmt.Fprintf(&sb, "… %d more series; narrow the query or aggregate\n", len(resp.Data.Result)-maxSeries)
			break
		}
		sb.WriteString(labels(r.Metric))
		if len(r.Value) == 2 {
			fmt.Fprintf(&sb, " %v", r.Value[1])
		}
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

// compactSeries renders a series listing one per line.
func compactSeries(raw string, maxSeries int) string {
	var resp struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil || resp.Status != "success" {
		return raw
	}
	if len(resp.Data) == 0 {
		return "no series match"
	}
	var sb strings.Builder
	for i, m := range resp.Data {
		if i >= maxSeries {
			fmt.Fprintf(&sb, "… %d more series\n", len(resp.Data)-maxSeries)
			break
		}
		sb.WriteString(labels(m))
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

func labels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	first := true
	if n, ok := m["__name__"]; ok {
		sb.WriteString(n)
		first = false
	}
	for _, k := range keys {
		if k == "__name__" {
			continue
		}
		if !first {
			sb.WriteByte(',')
		}
		first = false
		sb.WriteString(k + "=" + m[k])
	}
	sb.WriteByte('}')
	return sb.String()
}
