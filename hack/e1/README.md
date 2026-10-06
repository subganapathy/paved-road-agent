# E1 on the existing `sgrpc` kind cluster

E1 is the stack vikrant generates: a sidecar mesh with strict mTLS, a
progressive-delivery controller, a GitOps controller, an event-driven
autoscaler, and Prometheus scraping annotated pods. The `sgrpc` cluster
already runs all of that; these files add what the reviewer needs on top:

- `kube-state-metrics.yaml` — cluster state as metrics (pod labels for
  `app`, `app.kubernetes.io/name`, `version` are exposed so selectors in
  `.paved-agent/discover.yaml` can be matched in queries).
- `prometheus.yml` — the scrape config, with a dedicated `honor_labels`
  job for kube-state-metrics (the generic pods job would clobber its
  `namespace` and `pod` labels) and a `cluster` label stamped on every
  series by relabeling (`external_labels` only reach federation and
  remote write, not local queries).
- `traffic.yaml` + `hello.proto` — a ~5 rps generator with `frontend`'s
  identity calling `hello`, hardened for the restricted Pod Security
  Standard the namespaces enforce, sending the proto explicitly because
  the authorization policy denies reflection.

Apply:

    kubectl --context kind-sgrpc apply -f hack/e1/kube-state-metrics.yaml
    kubectl --context kind-sgrpc -n monitoring create cm prometheus --from-file=prometheus.yml=hack/e1/prometheus.yml --dry-run=client -o yaml | kubectl --context kind-sgrpc apply -f -
    kubectl --context kind-sgrpc -n monitoring rollout restart deploy/prometheus
    kubectl --context kind-sgrpc -n frontend create cm hello-proto --from-file=hello.proto=hack/e1/hello.proto
    kubectl --context kind-sgrpc apply -f hack/e1/traffic.yaml
    kubectl --context kind-sgrpc -n monitoring port-forward svc/prometheus 9090:9090

What the reviewer should discover here, with no help: kube-state-metrics
present; a sidecar mesh with strict mTLS; workloads owned by a
progressive-rollout object; an autoscaler ceiling from the HPA the
event-driven autoscaler creates; **no** network policy enforcement (the
default kind CNI), despite NetworkPolicy objects being present; **no**
admission policy engine beyond the Pod Security Standard labels; the
GitOps and rollout controllers are not scraped, so drift and rollout
state come from the intent and the owner chain, not from metrics.
