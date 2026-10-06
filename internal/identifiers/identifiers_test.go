package identifiers

import (
	"strings"
	"testing"
)

const good = `
service: hello
workloads:
  - namespace: hello
    selector: app.kubernetes.io/name=hello
    container: hello
    clusters: all
    stack:
      mesh: {is: "sidecar mesh, mTLS strict", verify: "a proxy sidecar in the pod template; the mesh's request metric reports destination hello"}
      deploy: {is: "staged rollout with analysis in prod", verify: "owner chain pod -> ReplicaSet -> progressive rollout object"}
    measure:
      instances: count by (cluster) (kube_pod_info{namespace="hello"} * on (namespace,pod) group_left kube_pod_status_ready{condition="true"})
      rps: sum by (cluster) (rate(istio_requests_total{destination_workload="hello",reporter="destination"}[5m]))
docs:
  - CLAUDE.md
  - docs/runbook.md
answers:
  - cloud: {provider: gcp, resource: gs://hello-exports}
  - {question: q3, answer: "no", detail: "there is no staging environment"}
`

func TestParseGood(t *testing.T) {
	f, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if f.Service != "hello" || len(f.Workloads) != 1 {
		t.Fatalf("parsed %+v", f)
	}
	w := f.Workloads[0]
	if got := strings.Join(w.Clusters, ","); got != "all" {
		t.Errorf("clusters = %q", got)
	}
	if got := strings.Join(w.Missing(), ","); got != "admission,enforcer,autoscaler,metrics" {
		t.Errorf("missing = %q", got)
	}
	out, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(out); err != nil {
		t.Errorf("round trip: %v\n%s", err, out)
	}
}

func TestClustersAsList(t *testing.T) {
	f, err := Parse([]byte(`
service: hello
workloads:
  - {namespace: hello, selector: app=hello, container: hello, clusters: [prod-us, prod-eu]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Workloads[0].Clusters, ","); got != "prod-us,prod-eu" {
		t.Errorf("clusters = %q", got)
	}
}

func TestNotDeployedNeedsProbe(t *testing.T) {
	if _, err := Parse([]byte("service: greeter\nworkloads: []\n")); err == nil {
		t.Error("expected a lint error without a probe")
	}
	if _, err := Parse([]byte("service: greeter\nworkloads: []\nprobe: {container: greeter}\n")); err != nil {
		t.Errorf("probe should satisfy the lint: %v", err)
	}
}

func TestLintRejectsWhatMustNotBeHere(t *testing.T) {
	cases := map[string]string{
		"endpoint": `
service: hello
workloads:
  - {namespace: hello, selector: app=hello, container: hello, clusters: all}
answers:
  - note: metrics at https://thanos.internal
`,
		"hostname": `
service: hello
workloads:
  - {namespace: hello, selector: app=hello, container: hello, clusters: all}
answers:
  - note: use prom.acme.internal
`,
		"secret": `
service: hello
workloads:
  - {namespace: hello, selector: app=hello, container: hello, clusters: all}
answers:
  - note: token=abcdefghijklmnopqrstuvwxyz
`,
		"bad selector": `
service: hello
workloads:
  - {namespace: hello, selector: "app hello", container: hello, clusters: all}
`,
		"half binding": `
service: hello
workloads:
  - namespace: hello
    selector: app=hello
    container: hello
    clusters: all
    stack:
      mesh: {is: "sidecar mesh"}
`,
		"doc outside repo": `
service: hello
workloads:
  - {namespace: hello, selector: app=hello, container: hello, clusters: all}
docs: ["../secrets.md"]
`,
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected a lint error", name)
		}
	}
}

func TestBucketNamesAreNotEndpoints(t *testing.T) {
	src := `
service: hello
workloads:
  - {namespace: hello, selector: app=hello, container: hello, clusters: all}
answers:
  - cloud: {provider: gcp, resource: gs://hello-exports}
`
	if _, err := Parse([]byte(src)); err != nil {
		t.Errorf("gs:// is a resource name, not an endpoint: %v", err)
	}
}
