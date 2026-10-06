# E5: two clusters and an appliance between them

The server is in one kind cluster, the client in another, and the only
path between them is **not Kubernetes**: a stock Envoy container on the
shared Docker network, reached by a name that a programmed DNS answers.

```
 kind e5-client                 docker network "kind"                 kind e5-server
 client (ns client) ──dials──▶ api.e5.internal ──▶ e5-appliance:8443 ──▶ 172.18.0.8:30051 ──▶ hello (ns hello, 2 pods)
                                (dnsmasq, 172.18.0.11)  stock Envoy       NodePort on the
 CoreDNS forwards e5.internal ─┘   (172.18.0.10)         server node
 Prometheus (ns monitoring) scrapes: own KSM, server KSM via NodePort 30080, hello metrics via 30090, the appliance's /stats/prometheus
```

The appliance's limits, which the review must find in `pra-infra` and
measure in Prometheus: a **20 rps local rate limit** (then `429`), a
**3-retry policy** toward the server, 200 in-flight / 64 connections.
The server has 2 replicas and no autoscaler.

Fixture repositories in the demo org: `pra-infra` (the path: appliance,
DNS, exposure, Prometheus) and `pra-client` (the workload, with its
identifiers but **no `path` line** — deriving it is the test). Fixture PR:
`pra-client#1`, call rate 5 → 50 rps.

Bring it up (E1's `sgrpc` nodes must be stopped first; Docker has ~7.6 GB):

    kind create cluster --config hack/e5/kind-server.yaml
    kind create cluster --config hack/e5/kind-client.yaml
    kubectl --context kind-e5-server create ns monitoring; kubectl --context kind-e5-server apply -f hack/e1/kube-state-metrics.yaml -f hack/e5/server.yaml
    docker run -d --name e5-appliance --network kind -v $PWD/hack/e5/envoy.yaml:/etc/envoy/envoy.yaml:ro envoyproxy/envoy:v1.31-latest -c /etc/envoy/envoy.yaml
    docker run -d --name e5-dns --network kind -p 15353:53/udp -p 15353:53/tcp -v $PWD/hack/e5/dnsmasq.conf:/etc/dnsmasq.conf:ro jpillora/dnsmasq
    kubectl --context kind-e5-client create ns monitoring client; kubectl --context kind-e5-client apply -f hack/e1/kube-state-metrics.yaml
    kubectl --context kind-e5-client -n kube-system create cm coredns --from-file=Corefile=hack/e5/coredns.Corefile --dry-run=client -o yaml | kubectl --context kind-e5-client apply -f -; kubectl --context kind-e5-client -n kube-system rollout restart deploy/coredns
    kubectl --context kind-e5-client -n client create cm hello-proto --from-file=hello.proto=hack/e1/hello.proto; kubectl --context kind-e5-client apply -f hack/e5/client.yaml -f hack/e5/prometheus.yaml
    kubectl --context kind-e5-client -n monitoring port-forward svc/prometheus 9091:9090

IPs above are what Docker assigned on this laptop; adjust `envoy.yaml`,
`dnsmasq.conf`, `coredns.Corefile` and `prometheus.yaml` if they differ.
`proxy.e5.yaml` points the proxy at Prometheus on 9091 and the DNS on
15353.
