package proxy

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// dnsSlot resolves names from inside the boundary: the first hop of any
// path that leaves a cluster is "what does this name resolve to here".
// Read-only by nature; the resolver is the proxy host's.
type dnsSlot struct {
	resolver *net.Resolver
}

func newDNS() *dnsSlot { return &dnsSlot{resolver: net.DefaultResolver} }

// Resolved is what a name resolves to, as the client would see it.
type Resolved struct {
	Name  string   `json:"name"`
	CNAME string   `json:"cname,omitempty"`
	IPs   []string `json:"ips"`
	// Shape classifies the target for the discoverer: cluster-local
	// (.svc / .svc.cluster.local), clusterset (.svc.clusterset.local, the
	// multi-cluster Services API), or external (anything else).
	Shape string `json:"shape"`
	Note  string `json:"note,omitempty"`
}

type resolveReq struct{ Name string }

func (d *dnsSlot) resolve(ctx context.Context, r resolveReq) (any, int, error) {
	name := strings.TrimSuffix(strings.TrimSpace(r.Name), ".")
	if name == "" || strings.ContainsAny(name, " /\\@") {
		return nil, 400, fmt.Errorf("a bare DNS name is required")
	}
	out := Resolved{Name: name, Shape: shape(name)}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if cname, err := d.resolver.LookupCNAME(ctx, name); err == nil && strings.TrimSuffix(cname, ".") != name {
		out.CNAME = strings.TrimSuffix(cname, ".")
	}
	addrs, err := d.resolver.LookupIPAddr(ctx, name)
	if err != nil {
		out.Note = "does not resolve from the proxy's network: " + err.Error()
		return out, 200, nil
	}
	for _, a := range addrs {
		out.IPs = append(out.IPs, a.IP.String())
	}
	sort.Strings(out.IPs)
	if out.Shape == "cluster-local" {
		out.Note = "a cluster-local name; it resolves only inside its cluster, so this answer (if any) is the proxy host's view, not the client's"
	}
	return out, 200, nil
}

func shape(name string) string {
	switch {
	case strings.HasSuffix(name, ".svc.clusterset.local"):
		return "clusterset"
	case strings.HasSuffix(name, ".svc"), strings.HasSuffix(name, ".svc.cluster.local"), strings.Contains(name, ".svc."):
		return "cluster-local"
	}
	return "external"
}
