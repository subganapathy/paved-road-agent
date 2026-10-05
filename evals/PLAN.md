# Evaluation plan: environments × pull requests

Status: for review, 2026-10-05. Locks the test matrix before code.

The test is: the same program (`qualities.yaml`, no product names) run
against **four environments** built on kind, each a coherent, realistic
stack, with **ten pull requests** whose expected outcome is known per
environment. The lead must *discover* the stack — which mesh, which
policy enforcer and whether it enforces, which admission engine, which
deployment tool, which workload kinds, which metrics exist — and then
judge the PR against it. A finding that names the right product in the
*report* is fine; a prompt that names one fails the build.

## 1. Environments

Each environment is one kind cluster (E1 has two, to exercise grouping
by environment). Built by `hack/env-up.sh <id>`, torn down by
`hack/env-down.sh <id>`. They run **sequentially** by default — each is
3–4 GB of RAM with a mesh — and in parallel on a machine that can hold
them.

| Id | Mesh | Network policy enforcer | Admission policy | Deployment tool / workload kind | Metrics | What it exercises |
|---|---|---|---|---|---|---|
| **E1 `istio-argo`** (2 clusters: `e1-dev`, `e1-prod`) | Istio, sidecar, STRICT mTLS | Calico | Gatekeeper (require limits, deny privileged, require labels) | Argo CD + Argo Rollouts (canary with analysis in prod, none in dev); KEDA | Prometheus + kube-state-metrics + Istio standard metrics + Rollouts and Argo CD metrics | the stack vikrant generates; env grouping; staged rollout present |
| **E2 `linkerd-flux`** | Linkerd, STRICT (default) | Cilium (`CiliumNetworkPolicy`) | Kyverno (same three rules) | Flux (`Kustomization`), plain Deployments + HPA; **no canary** | Prometheus + KSM + Linkerd proxy metrics + Flux metrics | a second mesh dialect; no staged rollout → blast-radius finding; Cilium drop metrics as evidence |
| **E3 `ambient-plain`** | Istio ambient (ztunnel; one waypoint for `ledger`) | Cilium | none; namespaces labelled Pod Security `restricted` | `kubectl apply` from CI — **no GitOps controller**, StatefulSet for `ledger` | Prometheus + KSM + ztunnel/waypoint metrics | no drift signal (intent only); L4-only metrics where no waypoint; StatefulSet owner chain; PSA rejects instead of a policy engine |
| **E4 `bare`** | none | kind default (kindnet): NetworkPolicy objects exist **but are not enforced** | none | Argo CD, Deployments, no autoscaler | Prometheus **without** kube-state-metrics | plaintext traffic → data-protection finding; "policy present but not enforced"; capability probe degrades to intent-only; ceiling = desired |

Every environment runs the same three services and the same traffic
generator (a `curl` loop from `frontend`'s namespace at ~5 rps) so the
metric queries have something to return.

## 2. Fixture repositories

Three small Go gRPC services in the demo org, written for this purpose
(the take-home repos stay untouched while they are being graded):

```
smallStepGiantLeap/pra-frontend   HTTP → gRPC gateway; calls hello; exposed via the ingress gateway
smallStepGiantLeap/pra-hello      Hello(name) → greeting; calls nothing
smallStepGiantLeap/pra-ledger     GetBalance(account); holds state (StatefulSet in E3); the target of the new-dependency PRs
smallStepGiantLeap/pra-infra      Terraform: Kubernetes provider resources on the kind cluster (namespaces, PVCs, quotas) + one recorded GCP plan
```

Each service repository has one code base and one `deploy/` directory
per environment:

```
deploy/istio-argo/      Rollout, AuthorizationPolicy, NetworkPolicy (Calico-enforced), ScaledObject, AnalysisTemplate
deploy/linkerd-flux/    Deployment, HPA, Server + AuthorizationPolicy (Linkerd), CiliumNetworkPolicy, Kustomization
deploy/ambient-plain/   Deployment or StatefulSet, waypoint binding, AuthorizationPolicy (ambient), CiliumNetworkPolicy
deploy/bare/            Deployment, NetworkPolicy (unenforced), Argo Application
```

Each environment's deployment tool points at its own directory, so
*which overlay a cluster runs* is something the lead derives from the
GitOps intent (the Argo `Application` path, the Flux `Kustomization`
path, the CI workflow's `kubectl apply -k`), not something it is told.
`.paved-agent/discover.yaml` is hand-written for each service and also
produced by the derivation test.

The PRs below are opened once per service repository as **draft PRs that
stay open**; the eval runs each against each environment by pointing the
proxy at that environment's cluster(s).

## 3. Pull requests and expected outcomes

"Expected" means: the verdict, and the findings that must be present
(quality, severity, the entity named). Extra findings are allowed; a
missing required finding or a forbidden claim fails the fixture.

| Id | PR | E1 istio-argo | E2 linkerd-flux | E3 ambient-plain | E4 bare |
|---|---|---|---|---|---|
| **P1** backward-incompatible contract: `hello.proto` renames `name` → `person` and changes the response type | compatibility **blocking** (`buf breaking`; `frontend` deployed at the old proto in both clusters); blast radius notes the staged rollout cannot save a wire break | same, plus blast radius **blocking**: no staged rollout | same as E2 | same as E2 |
| **P2** new dependency: `frontend` starts calling `ledger` without onboarding | correctness **blocking**: no `AuthorizationPolicy` for `frontend` on `ledger`, Calico `NetworkPolicy` denies; evidence from intent and from `istio_requests_total{response_code="403"}` after a probe call | correctness **blocking**: no Linkerd `AuthorizationPolicy`; `CiliumNetworkPolicy` denies; evidence `hubble_drop_total` | correctness **blocking**: waypoint policy denies; Cilium denies | correctness **warning**, not blocking: the call *works* — the NetworkPolicy exists but kindnet does not enforce it; **correctness/data-protection blocking**: plaintext, no identity, for account data |
| **P3** bad infrastructure change (`pra-infra`): PVC storage class change forces replacement; a quota lowered below current usage | resilience **blocking** (replace = delete then create, data on the PVC is lost); scalability **blocking** (quota below running usage) — environment-independent, run once on E1 | — | — | — |
| **P4** business-logic change in `hello` that could error on existing input | blast radius **warning** (staged rollout with analysis in prod); correctness warning (contract silent on the input) | blast radius **blocking**: high worst case, no staged rollout | same as E2 | same as E2 |
| **P5** load: `frontend` adds a second `hello` call per request and `hello` is pinned low | scalability **blocking** against the KEDA ceiling | scalability **blocking** against the HPA max | scalability **blocking** against the StatefulSet's fixed replicas | scalability **blocking**: no autoscaler, ceiling = desired |
| **P6** hardening regression: `ledger` manifest drops the readiness probe and sets `runAsUser: 0` | correctness **blocking**: Gatekeeper constraint will reject the admission (cite the constraint); resilience: probe gone | correctness **blocking**: Kyverno policy rejects | correctness **blocking**: Pod Security `restricted` rejects `runAsUser: 0` | **deploys**; resilience **blocking**: root + no probe, nothing stops it |
| **P7** unknown placement: a new service `pra-greeter` with code but no manifests anywhere | one **question**, gate; after "no": info | same | same | same |
| **P8** refactor: rename a private function in `hello` | info on all seven, under $0.30 | same | same | same |
| **P9** debuggability: `ledger` adds a new error path with no metric and swallows the cause | debuggability **warning** (no signal, no alert rule mentions it) | same | same | same |
| **P10** dynamic configuration: `ledger`'s ConfigMap changes a timeout from 2s to 200ms | resilience **warning** (which calls this bounds; takes effect on next pod restart only — cite the mount type); blast radius names the env | same | same (and notes no GitOps controller will roll it) | same |

Discovery expectations per environment (checked on every run, any PR):

| | E1 | E2 | E3 | E4 |
|---|---|---|---|---|
| mesh and mTLS | Istio sidecar, STRICT | Linkerd, mTLS on | Istio ambient, L4 everywhere, L7 at `ledger` | none, plaintext |
| policy enforcer | Calico, enforcing | Cilium, enforcing | Cilium, enforcing | kindnet, **not enforcing** |
| admission | Gatekeeper + 3 constraints | Kyverno + 3 policies | Pod Security `restricted` | none |
| deployment tool / drift signal | Argo CD, `argocd_app_info` | Flux, `gotk_reconcile_condition` | none — intent only | Argo CD |
| workload kinds | Rollout | Deployment + HPA | Deployment, StatefulSet | Deployment |
| metrics capabilities | KSM, Istio, Rollouts, KEDA | KSM, Linkerd | KSM, ztunnel, waypoint | **no KSM** → intent-only placement |
| environments | dev, prod (grouped) | one | one | one |

## 4. Cost and tiers

Full matrix: 4 environments × 10 PRs, minus the once-only ones (P3, P7,
P8) ≈ 31 live runs. At ~$1.5 per Opus run that is ~$45 per full
release run — a weekly event, not a per-commit one.

| Tier | Runs | Model | Cost | When |
|---|---|---|---|---|
| Unit + recorded | discovery against recorded metrics from each environment; derivation against the fixture repos; no model | none | $0 | every commit |
| Smoke | E1 × {P2, P8} | Haiku | ~$0.05 | every prompt change |
| Dev | E1 × all ten | Sonnet | ~$3 | before pushing a prompt or profile change |
| Cross-stack | {E2, E3, E4} × {P2, P4, P5, P6} | Sonnet | ~$6 | when discovery, profiles or the derivation chain change |
| Release | the full matrix | Opus | ~$45 | merge to main, weekly |

Recorded metrics are captured once per environment with
`hack/record.sh <id>` (every profile query against the live cluster,
saved under `evals/recorded/<id>/`), so the $0 tier covers discovery for
all four stacks without any cluster running.

## 5. Build order

1. **E1** first (closest to what exists), the three fixture repos with
   `deploy/istio-argo/`, `hack/env-up.sh e1`, traffic generator,
   `hack/record.sh`. P2, P4, P7, P8 as the first four PRs.
2. **E4** second — it is the cheapest to build and the most instructive
   (unenforced policy, no mesh, no KSM).
3. **E2**, then **E3**.
4. **P3** and `pra-infra` last, since Terraform against the Kubernetes
   provider needs its own state setup.

## 6. What this plan deliberately leaves out

- Multi-cluster beyond E1's two. Env grouping is proven once.
- AWS-flavoured stacks (App Mesh, VPC CNI policies): a third mesh dialect
  adds little the first two do not.
- Crossplane as the infrastructure tool: a later addition to `pra-infra`.
- Real cloud resources: P3's GCP case is a recorded plan JSON, not a live
  project, until M4.
