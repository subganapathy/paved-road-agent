package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// metricsSlot speaks the PromQL HTTP API. Five read operations, nothing
// else: the admin and write endpoints of the backend do not exist here.
type metricsSlot struct {
	endpoint string
	token    string
	http     *http.Client
}

func newMetrics(cfg *MetricsSlot) (*metricsSlot, error) {
	tok, err := Credential(cfg.Auth)
	if err != nil {
		return nil, err
	}
	return &metricsSlot{endpoint: trimSlash(cfg.Endpoint), token: tok, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Requests, as the connector tools send them.
type (
	queryReq      struct{ Query, Time string }
	queryRangeReq struct{ Query, Start, End, Step string }
	seriesReq     struct {
		Match      []string
		Start, End string
	}
	labelValuesReq struct {
		Label    string
		Match    []string
		Contains string
	}
)

// labelValuesMax is how many values come back as a plain list; above it a
// __name__ listing is grouped by family prefix, because a thousand metric
// names cost the model six thousand tokens and it reads only the prefixes.
const labelValuesMax = 150

// get performs one read against the backend's API and returns the raw
// body: the model reads the backend's JSON directly, which is both the
// most faithful evidence and the least code.
func (m *metricsSlot) get(ctx context.Context, path string, q url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", m.endpoint+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return b, resp.StatusCode, err
}

func (m *metricsSlot) query(ctx context.Context, r queryReq) ([]byte, int, error) {
	if r.Query == "" {
		return nil, 0, fmt.Errorf("query is required")
	}
	q := url.Values{"query": {r.Query}}
	if r.Time != "" {
		q.Set("time", r.Time)
	}
	return m.get(ctx, "/api/v1/query", q)
}

func (m *metricsSlot) queryRange(ctx context.Context, p *Policy, r queryRangeReq) ([]byte, int, error) {
	if r.Query == "" || r.Start == "" || r.End == "" {
		return nil, 0, fmt.Errorf("query, start and end are required")
	}
	start, end, err := parseRange(r.Start, r.End)
	if err != nil {
		return nil, 0, err
	}
	if err := p.rangeOK(start, end); err != nil {
		return nil, 0, err
	}
	step := r.Step
	if step == "" {
		step = "60s"
	}
	return m.get(ctx, "/api/v1/query_range", url.Values{"query": {r.Query}, "start": {r.Start}, "end": {r.End}, "step": {step}})
}

func (m *metricsSlot) series(ctx context.Context, p *Policy, r seriesReq) ([]byte, int, error) {
	if len(r.Match) == 0 {
		return nil, 0, fmt.Errorf("match is required")
	}
	q := url.Values{"match[]": r.Match}
	if r.Start != "" && r.End != "" {
		start, end, err := parseRange(r.Start, r.End)
		if err != nil {
			return nil, 0, err
		}
		if err := p.rangeOK(start, end); err != nil {
			return nil, 0, err
		}
		q.Set("start", r.Start)
		q.Set("end", r.End)
	}
	return m.get(ctx, "/api/v1/series", q)
}

// labelValues with label "__name__" is how the model learns what the
// fleet exports: the first step of stack discovery. The result is
// compacted: filtered by Contains, and when still long, grouped by the
// prefix before the first underscore with a few examples each, so the
// model sees which exporters are present and narrows with Contains.
func (m *metricsSlot) labelValues(ctx context.Context, r labelValuesReq) ([]byte, int, error) {
	if r.Label == "" {
		return nil, 0, fmt.Errorf("label is required")
	}
	q := url.Values{}
	for _, mt := range r.Match {
		q.Add("match[]", mt)
	}
	body, status, err := m.get(ctx, "/api/v1/label/"+url.PathEscape(r.Label)+"/values", q)
	if err != nil || status/100 != 2 {
		return body, status, err
	}
	var resp struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Status != "success" {
		return body, status, nil
	}
	values := resp.Data
	if r.Contains != "" {
		kept := values[:0:0]
		for _, v := range values {
			if strings.Contains(v, r.Contains) {
				kept = append(kept, v)
			}
		}
		values = kept
	}
	sort.Strings(values)
	out := map[string]any{"label": r.Label, "count": len(values)}
	if r.Contains != "" {
		out["contains"] = r.Contains
	}
	if len(values) <= labelValuesMax {
		out["values"] = values
	} else {
		groups := map[string][]string{}
		var order []string
		for _, v := range values {
			prefix := v
			if i := strings.IndexByte(v, '_'); i > 0 {
				prefix = v[:i]
			}
			if _, ok := groups[prefix]; !ok {
				order = append(order, prefix)
			}
			groups[prefix] = append(groups[prefix], v)
		}
		summary := make([]string, 0, len(order))
		for _, prefix := range order {
			g := groups[prefix]
			examples := g
			if len(examples) > 6 {
				examples = examples[:6]
			}
			summary = append(summary, fmt.Sprintf("%s (%d): %s", prefix, len(g), strings.Join(examples, ", ")))
		}
		out["families"] = summary
		out["note"] = fmt.Sprintf("%d values; grouped by prefix with up to six examples each. List one family with contains=<prefix>.", len(values))
	}
	b, _ := json.Marshal(out)
	return b, status, nil
}

func (m *metricsSlot) rules(ctx context.Context) ([]byte, int, error) {
	return m.get(ctx, "/api/v1/rules", url.Values{})
}

// parseRange accepts RFC 3339 or Unix seconds, as the API does.
func parseRange(s, e string) (time.Time, time.Time, error) {
	start, err := parseTime(s)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("start: %w", err)
	}
	end, err := parseTime(e)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("end: %w", err)
	}
	return start, end, nil
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Unix(int64(f), 0), nil
	}
	return time.Time{}, fmt.Errorf("%q is neither RFC 3339 nor Unix seconds", s)
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
