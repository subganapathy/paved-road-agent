package proxy

import (
	"context"
	"fmt"
	"strings"
)

// cloudSlot answers one generic question — cloud_get(provider, resource,
// attribute) — through provider adapters. Nothing above the slot knows a
// provider's API; nothing in the slot can write.
type cloudSlot struct {
	adapters map[string]cloudAdapter
}

// cloudAdapter is one provider. Resources are named in the provider's own
// terms (gs://bucket, ip:10.0.0.4, instance:name, project:id); attributes
// are the small common vocabulary: exists, iam, labels, protection, owner.
type cloudAdapter interface {
	Get(ctx context.Context, resource, attribute string) (any, error)
	Attributes() []string
}

// CloudSlot is the configuration: one entry per provider.
type CloudSlot struct {
	GCP *GCPSlot `yaml:"gcp,omitempty"`
}

// GCPSlot binds the GCP adapter.
type GCPSlot struct {
	Projects []string `yaml:"projects"`
	// Auth is "adc" (Application Default Credentials on the proxy host —
	// the operator's `gcloud auth application-default login` on a laptop,
	// a federated service account in a cluster) or a credential source
	// holding an access token for tests.
	Auth string `yaml:"auth"`
	// API overrides the Google API base, for tests.
	API string `yaml:"api,omitempty"`
}

func newCloud(cfg *CloudSlot) (*cloudSlot, error) {
	s := &cloudSlot{adapters: map[string]cloudAdapter{}}
	if cfg.GCP != nil {
		a, err := newGCP(cfg.GCP)
		if err != nil {
			return nil, err
		}
		s.adapters["gcp"] = a
	}
	return s, nil
}

type cloudGetReq struct {
	Provider  string
	Resource  string
	Attribute string
}

func (s *cloudSlot) get(ctx context.Context, r cloudGetReq) (any, int, error) {
	a, ok := s.adapters[strings.ToLower(r.Provider)]
	if !ok {
		var names []string
		for k := range s.adapters {
			names = append(names, k)
		}
		return nil, 400, fmt.Errorf("provider %q is not bound; bound: %s", r.Provider, strings.Join(names, ", "))
	}
	if r.Resource == "" || r.Attribute == "" {
		return nil, 400, fmt.Errorf("resource and attribute are required; attributes: %s", strings.Join(a.Attributes(), ", "))
	}
	out, err := a.Get(ctx, r.Resource, r.Attribute)
	if err != nil {
		return nil, 502, err
	}
	return out, 200, nil
}
