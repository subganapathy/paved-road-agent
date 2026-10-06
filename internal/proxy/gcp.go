package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// gcpAdapter reads GCP through its REST APIs with a read-only scope.
// Resources it understands:
//
//	gs://<bucket>        exists | iam | labels | protection
//	ip:<address>         owner   — which instance or forwarding rule holds it
//	instance:<name>      exists | labels
//	project:<id>         iam
//
// It never lists more than it must, never writes, and says "not found" as
// a result rather than an error, because absence is evidence.
type gcpAdapter struct {
	projects []string
	api      string
	tokens   oauth2.TokenSource
	http     *http.Client
}

const gcpReadScope = "https://www.googleapis.com/auth/cloud-platform.read-only"

func newGCP(cfg *GCPSlot) (*gcpAdapter, error) {
	if len(cfg.Projects) == 0 {
		return nil, fmt.Errorf("slots.cloud.gcp.projects is required")
	}
	a := &gcpAdapter{projects: cfg.Projects, api: trimSlash(cfg.API), http: &http.Client{Timeout: 30 * time.Second}}
	if a.api == "" {
		a.api = "https://www.googleapis.com"
	}
	switch {
	case cfg.Auth == "" || cfg.Auth == "adc":
		ts, err := google.DefaultTokenSource(context.Background(), gcpReadScope)
		if err != nil {
			return nil, fmt.Errorf("gcp: application default credentials: %w", err)
		}
		a.tokens = ts
	default:
		tok, err := Credential(cfg.Auth)
		if err != nil {
			return nil, err
		}
		a.tokens = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok})
	}
	return a, nil
}

func (a *gcpAdapter) Attributes() []string {
	return []string{"exists", "iam", "labels", "protection", "owner"}
}

func (a *gcpAdapter) get(ctx context.Context, path string, out any) (int, error) {
	tok, err := a.tokens.Token()
	if err != nil {
		return 0, fmt.Errorf("gcp credentials: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", a.api+path, nil)
	if err != nil {
		return 0, err
	}
	tok.SetAuthHeader(req)
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == 404 {
		return 404, nil
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("gcp %s: HTTP %d: %s", path, resp.StatusCode, firstLineOf(b))
	}
	return resp.StatusCode, json.Unmarshal(b, out)
}

// Get dispatches on the resource's prefix.
func (a *gcpAdapter) Get(ctx context.Context, resource, attribute string) (any, error) {
	switch {
	case strings.HasPrefix(resource, "gs://"):
		return a.bucket(ctx, strings.TrimPrefix(resource, "gs://"), attribute)
	case strings.HasPrefix(resource, "ip:"):
		if attribute != "owner" {
			return nil, fmt.Errorf("for an ip the attribute is owner")
		}
		return a.ipOwner(ctx, strings.TrimPrefix(resource, "ip:"))
	case strings.HasPrefix(resource, "instance:"):
		return a.instance(ctx, strings.TrimPrefix(resource, "instance:"), attribute)
	case strings.HasPrefix(resource, "project:"):
		return a.project(ctx, strings.TrimPrefix(resource, "project:"), attribute)
	}
	return nil, fmt.Errorf("resource %q: want gs://bucket, ip:address, instance:name or project:id", resource)
}

func (a *gcpAdapter) bucket(ctx context.Context, name, attribute string) (any, error) {
	name = strings.SplitN(name, "/", 2)[0]
	switch attribute {
	case "exists", "labels", "protection":
		var b struct {
			Name            string                 `json:"name"`
			Location        string                 `json:"location"`
			Labels          map[string]string      `json:"labels"`
			Versioning      struct{ Enabled bool } `json:"versioning"`
			RetentionPolicy *struct {
				RetentionPeriod string `json:"retentionPeriod"`
				IsLocked        bool   `json:"isLocked"`
			} `json:"retentionPolicy"`
			IAMConfiguration struct {
				UniformBucketLevelAccess struct{ Enabled bool } `json:"uniformBucketLevelAccess"`
				PublicAccessPrevention   string                 `json:"publicAccessPrevention"`
			} `json:"iamConfiguration"`
			SoftDeletePolicy *struct {
				RetentionDurationSeconds string `json:"retentionDurationSeconds"`
			} `json:"softDeletePolicy"`
		}
		status, err := a.get(ctx, "/storage/v1/b/"+url.PathEscape(name), &b)
		if err != nil {
			return nil, err
		}
		if status == 404 {
			return map[string]any{"resource": "gs://" + name, "exists": false}, nil
		}
		out := map[string]any{"resource": "gs://" + name, "exists": true, "location": b.Location}
		if attribute == "labels" {
			out["labels"] = b.Labels
		}
		if attribute == "protection" {
			out["versioning"] = b.Versioning.Enabled
			out["uniform_bucket_level_access"] = b.IAMConfiguration.UniformBucketLevelAccess.Enabled
			out["public_access_prevention"] = b.IAMConfiguration.PublicAccessPrevention
			if b.RetentionPolicy != nil {
				out["retention_seconds"] = b.RetentionPolicy.RetentionPeriod
				out["retention_locked"] = b.RetentionPolicy.IsLocked
			}
			if b.SoftDeletePolicy != nil {
				out["soft_delete_seconds"] = b.SoftDeletePolicy.RetentionDurationSeconds
			}
		}
		return out, nil
	case "iam":
		var p struct {
			Bindings []struct {
				Role    string   `json:"role"`
				Members []string `json:"members"`
			} `json:"bindings"`
		}
		status, err := a.get(ctx, "/storage/v1/b/"+url.PathEscape(name)+"/iam", &p)
		if err != nil {
			return nil, err
		}
		if status == 404 {
			return map[string]any{"resource": "gs://" + name, "exists": false}, nil
		}
		return map[string]any{"resource": "gs://" + name, "bindings": p.Bindings}, nil
	}
	return nil, fmt.Errorf("bucket attribute %q: want exists, iam, labels or protection", attribute)
}

// ipOwner finds what holds an address: an instance's interface or NAT IP,
// or a forwarding rule (a load balancer's front). It walks the configured
// projects; absence is a result.
func (a *gcpAdapter) ipOwner(ctx context.Context, ip string) (any, error) {
	for _, project := range a.projects {
		var inst struct {
			Items map[string]struct {
				Instances []struct {
					Name   string            `json:"name"`
					Zone   string            `json:"zone"`
					Status string            `json:"status"`
					Labels map[string]string `json:"labels"`
					NICs   []struct {
						NetworkIP string `json:"networkIP"`
						Network   string `json:"network"`
						AccessCfg []struct {
							NatIP string `json:"natIP"`
						} `json:"accessConfigs"`
					} `json:"networkInterfaces"`
				} `json:"instances"`
			} `json:"items"`
		}
		if _, err := a.get(ctx, fmt.Sprintf("/compute/v1/projects/%s/aggregated/instances?maxResults=500", url.PathEscape(project)), &inst); err != nil {
			return nil, err
		}
		for _, scope := range inst.Items {
			for _, in := range scope.Instances {
				for _, nic := range in.NICs {
					hit := nic.NetworkIP == ip
					for _, ac := range nic.AccessCfg {
						hit = hit || ac.NatIP == ip
					}
					if hit {
						return map[string]any{"ip": ip, "owner": "instance", "project": project, "name": in.Name, "zone": last(in.Zone), "status": in.Status, "network": last(nic.Network), "labels": in.Labels}, nil
					}
				}
			}
		}
		var fr struct {
			Items map[string]struct {
				ForwardingRules []struct {
					Name                string `json:"name"`
					IPAddress           string `json:"IPAddress"`
					LoadBalancingScheme string `json:"loadBalancingScheme"`
					Target              string `json:"target"`
					BackendService      string `json:"backendService"`
					Network             string `json:"network"`
					Region              string `json:"region"`
				} `json:"forwardingRules"`
			} `json:"items"`
		}
		if _, err := a.get(ctx, fmt.Sprintf("/compute/v1/projects/%s/aggregated/forwardingRules?maxResults=500", url.PathEscape(project)), &fr); err != nil {
			return nil, err
		}
		for _, scope := range fr.Items {
			for _, r := range scope.ForwardingRules {
				if r.IPAddress == ip {
					return map[string]any{"ip": ip, "owner": "forwarding_rule", "project": project, "name": r.Name, "scheme": r.LoadBalancingScheme, "target": last(r.Target), "backend_service": last(r.BackendService), "network": last(r.Network), "region": last(r.Region)}, nil
				}
			}
		}
	}
	return map[string]any{"ip": ip, "owner": "not found", "searched_projects": a.projects}, nil
}

func (a *gcpAdapter) instance(ctx context.Context, name, attribute string) (any, error) {
	for _, project := range a.projects {
		var inst struct {
			Items map[string]struct {
				Instances []struct {
					Name   string            `json:"name"`
					Zone   string            `json:"zone"`
					Status string            `json:"status"`
					Labels map[string]string `json:"labels"`
				} `json:"instances"`
			} `json:"items"`
		}
		if _, err := a.get(ctx, fmt.Sprintf("/compute/v1/projects/%s/aggregated/instances?filter=%s&maxResults=50", url.PathEscape(project), url.QueryEscape("name = "+name)), &inst); err != nil {
			return nil, err
		}
		for _, scope := range inst.Items {
			for _, in := range scope.Instances {
				out := map[string]any{"resource": "instance:" + name, "exists": true, "project": project, "zone": last(in.Zone), "status": in.Status}
				if attribute == "labels" {
					out["labels"] = in.Labels
				}
				return out, nil
			}
		}
	}
	return map[string]any{"resource": "instance:" + name, "exists": false, "searched_projects": a.projects}, nil
}

func (a *gcpAdapter) project(ctx context.Context, id, attribute string) (any, error) {
	if attribute != "iam" {
		return nil, fmt.Errorf("for a project the attribute is iam")
	}
	var p struct {
		Bindings []struct {
			Role    string   `json:"role"`
			Members []string `json:"members"`
		} `json:"bindings"`
	}
	// getIamPolicy is a POST in the API, but a read in effect.
	tok, err := a.tokens.Token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/v1/projects/%s:getIamPolicy", strings.Replace(a.api, "www.googleapis.com", "cloudresourcemanager.googleapis.com", 1), url.PathEscape(id)), strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	tok.SetAuthHeader(req)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("gcp getIamPolicy %s: HTTP %d: %s", id, resp.StatusCode, firstLineOf(b))
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return map[string]any{"resource": "project:" + id, "bindings": p.Bindings}, nil
}

func last(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func firstLineOf(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}
