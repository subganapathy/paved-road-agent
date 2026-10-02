# paved-road-agent: design

Status: revision 3, 2026-10-02, after the deep review. [VISION.md](VISION.md)
still holds the why.

What changed in this revision: the agent is **stateless and restricted to
the repository under review**; its only persistent memory is identifiers
in that repo's `.paved-agent/`. Every data source is a **connector slot**
in a **proxy** deployed inside the customer boundary, bound at install
time, read-only, with no API-server access. Six dimensions (scale and test
coverage added). Questions are answered as PR comments; the bot turns the
answer into the file; the check stays blocked until it does. Learned
invariants and PR norms are dropped.

## 1. The model in one paragraph

An agent that assesses every pull request on six dimensions — **blast
radius, correctness, scale, resilience, debuggability, test coverage** —
by discovering, on every run, where the changed code runs and what it
touches. The only assumption about the environment is Kubernetes. It
reviews; it never executes. It reaches the environment only through a
proxy that exposes a closed set of read-only connectors (metrics, logs,
alerts, source control, cloud) and holds every credential; the agent holds
none and has no network. The only thing it remembers is a small set of
identifiers in the repository's `.paved-agent/` directory, which it
derives, proposes, and follows. When it cannot derive them, it asks a
yes/no question on the PR, says what it cannot verify without the answer,
and blocks the check until the answer lands.

## 2. Goals, non-goals, assumptions

Goals

- Any PR, including pure code changes with no manifest in the diff.
- Findings with evidence: a metric, a file and line, a cloud API response,
  or a human's answer. Never an unsupported claim.
- Stateless: nothing persists outside the repository under review.
- Lowest privilege: read-only connectors, bound at deployment inside the
  customer's boundary, short-lived credentials via workload identity
  federation, one write path (the PR under review).
- Bounded cost per review, enforced by the platform.

Non-goals (for now)

- Executing anything in the environment: no deploys, no applies, no
  API-server access at all.
- Non-Kubernetes runtimes (VMs, serverless) as placement targets.
- A hosted multi-tenant service. This design runs on one laptop for one org.

Assumptions

- Workloads run on Kubernetes, and the fleet exports kube-state-metrics.
  Everything else (mesh, GitOps tool, cloud, metrics stack, workload
  kinds) is discovered or configured, never assumed.
- Source control is the unit of knowledge: the repository under review,
  and other repositories in the org read through the `scm` connector.
- Cloud providers are parameters, not assumptions: `cloud_get("gcp", …)`.

## 3. The six dimensions

Every report has a verdict per dimension and findings under it. The lead
owns the dimensions; the discoverers supply the facts.

| Dimension | The question | Evidence that answers it |
|---|---|---|
| **Blast radius** | Where does the changed code run, who depends on it, and what is the worst case if it is wrong? | instances per cluster, traffic and callers per cluster, rollout staging (per-cluster canary or global), feature flags, other open PRs' assessed impact on the same services |
| **Correctness** | Does the change do what the PR says, honour its contract, and work where it runs? | the diff against the proto/API contract (field semantics, validation rules, `buf breaking` against the base); the author-stated docs it references; config keys and credentials present in the pod spec at each cluster; IAM and resource existence for cloud access |
| **Scale** | Can the system carry the load this change adds or redirects? | added rps from new callers against the callee's current rps, in-flight work and p99; CPU and memory usage against requests and limits; desired replicas and the autoscaler ceiling; saturation of the dependencies behind it |
| **Resilience** | Does it fail small and recover? | deadlines and their propagation; retry sanity (idempotency); bounded work; backpressure and overload behaviour; canary with analysis; PDB and spread; rollback without data migration |
| **Debuggability** | When it fails, can an operator tell, and where? | logs/metrics/traces on the new path; error wrapping and status codes; whether the new signal has an alert rule; a flag to turn it off; a runbook entry |
| **Test coverage** | Is the behaviour, including its failure cases, proven and described? | unit tests for the new path and its error branches; e2e coverage of the call or resource; documented behaviour under failure (what the caller sees, what the operator sees) |

"No impact on this dimension, and here is why" is a finding, not an
absence.

## 4. The journey

Service owner opens a PR. A required check, `impact`, moves through these
states:

```
queued ──▶ in_progress: discovering ──▶ in_progress: assessing ──▶ completed
                 │                                                   success  (info)
                 │ identifiers missing or broken,                    neutral  (warning)
                 │ and the agent cannot derive them                  failure  (blocking)
                 ▼
          action_required: pending human response   ◀── merge blocked until resolved
                 │
                 │ a reply on the question's comment thread, or a commit to
                 │ .paved-agent/ on the PR branch (by a human or by the bot)
                 ▼
          queued (again)
```

The question says what the agent could not figure out, what it cannot
verify without the answer, and what each answer means:

> **impact: pending human response.** I could not find where `greeter`
> runs: no `.paved-agent/discover.yaml`, no overlay in this repo, and no
> ready pod with container `greeter` in any cluster the metrics show.
> Without this I cannot verify **blast radius** or **scale** for this
> change, and the check stays blocked.
>
> Is `greeter` deployed anywhere today? Reply **no**, or **yes** with the
> namespace and clusters. I'll commit the matching `.paved-agent/` entry
> from your reply; a suggested change is attached if you'd rather edit it
> yourself.

Anyone with write access to the PR answers, in prose, on the thread. The
bot reads the reply, derives the file change, and commits it to the PR
branch (where the repo opts in) or attaches it as a suggested change; the
push re-runs the check. The file is the source of truth; the comment is
how humans write it. Often the bot answers itself: when it can derive the
identifiers from the repository (section 6.2) with confidence, it commits
them and the run proceeds without a question.

There is no deadline fallback. An unanswered question is a blocked PR.

Surfaces, in order of delivery: the CLI (exists; the operator edits the
file and re-runs), the GitHub check + comment threads + suggested changes
(M5), Slack notification linking back to the PR (after M5). One engine.

## 5. Pipeline

```
 PR ──▶ intake ──▶ bootstrap ──▶ DISCOVERY ─────────────▶ ANALYSIS ──▶ report ──▶ propose
         load      identifiers   follow the identifiers    six            check,     .paved-agent
         PR, repo, present?      through the proxy:        dimensions     comment    changes as
         .paved-   derive →      instances, owners, rps,                             commits or
         agent/    propose,      callers, utilization,                               suggestions
                   or ask + gate identities, config, cloud
```

1. **Intake.** Load the PR (files, patches, both sides of configs) and
   this repository's `.paved-agent/`. Nothing else is loaded from outside
   the repo.
2. **Bootstrap.** If the identifiers the change needs are present and
   valid, continue. If missing, derive them from the repository (6.2).
   Confident → propose and continue with the derived values; not
   confident → ask, and the check gates.
3. **Discovery.** Follow the identifiers through the proxy's connectors
   using the profile's standard queries. Deterministic code, a handful of
   calls. The results — ready instances per cluster, owner kind, desired
   replicas and ceiling, rps, callers, in-flight, p99, CPU/memory against
   limits, identities, mounted config, cloud resource state, firing
   alerts — are the **run memory**: kept for this run, summarized in the
   check, then gone. An identifier that returns nothing anywhere is
   broken: it is dropped for this run, derivation restarts, and the result
   is a proposal or a question.
4. **Analysis.** The lead reasons through the six dimensions with the
   diff, the contract, the author-stated docs the repo references, and
   the run memory. Where a dimension needs more evidence it goes back to
   a discoverer.
5. **Report.** One JSON document (section 11.4), rendered to the check,
   the comment, and stdout.
6. **Propose.** Any identifier the run derived or corrected becomes a
   change to `.paved-agent/` on the PR branch: a commit where the repo
   opted in, a suggested change otherwise.

What the lead checks for each kind of change (a new client, a cloud SDK
import, a Dockerfile, a handler, a proto, a Terraform plan) is guidance
in its **system prompt**. The milestone-1 rules stay in the tree only
until M2 measures whether a deterministic pre-pass saves enough tokens
to keep; if not, they go.

## 6. Discoverers

Four capabilities. The first three are specialists (their own agents,
their own connector access); the fourth belongs to the lead.

### 6.1 Org finder

Finds, through the `scm` connector, the repository that holds something —
the callee of a new client, the chart that deploys the mesh, the module a
Dockerfile builds from — and mounts it read-only into the sandbox.
Searches org-wide code for names it read in manifests (a chart, a CRD
kind, a DaemonSet, an import path), ranks by where hits cluster, mounts,
reads. Reading other repositories is allowed; writing to or remembering
anything about them is not. A search that finds nothing is an unknown and
becomes a question.

### 6.2 Deployment topology discoverer

On a normal run it **follows** the identifiers with the profile's standard
queries (section 7.3): namespace and labels → ready pods per cluster →
owner kind → desired replicas and ceiling; container → traffic, callers,
utilization; service account → identity bindings; config names → presence
in the pod template of the GitOps intent. No API-server access: actual
state comes from kube-state-metrics and the mesh through `metrics`; intent
from the manifests through `scm`; drift from the GitOps tool's metrics
(`argocd_app_info{sync_status,health_status}`).

At **bootstrap**, or when an identifier breaks, it **derives** them from
the repository, in this order, stopping when sources agree:

```
build graph        go list -deps from each cmd/: which binaries include the change
 → Dockerfile       COPY/ENTRYPOINT: which binary → which image; what else ships in it
 → CI publish       the image name it pushes
 → pod template     containers[].name, labels (the selector), namespace, serviceAccountName,
                    env/envFrom, volumes; the owner kind (Rollout, Deployment, StatefulSet…)
 → overlays         kustomize envs, Helm values per cluster, ApplicationSet selectors:
                    which clusters, if not all
 → telemetry        the profile's instance query with the derived labels, per cluster —
                    to confirm the identifiers return something
 → propose or ask   confident: write discover.yaml; not: the question names
                    exactly which step found nothing and what it blocks
```

A cluster without kube-state-metrics leaves actual state unknown; the
discoverer then reasons from intent only, says so, and the affected
findings carry lower confidence.

### 6.3 Cloud resource discoverer

Follows the cloud identifiers with one generic connector call,
`cloud_get(provider, resource, attribute)`: does the resource exist, what
IAM it carries, what quota it consumes, whether it holds data and is
protected, which identities are bound to it. Provider adapters sit behind
the call; nothing above the connector knows a provider's API. Paired with
the topology discoverer for the runtime half: which service account the
pod runs as, whether it maps to a cloud identity (Workload Identity
annotation, IRSA role) or to a mounted key, and whether that identity
holds the role the code needs. At bootstrap it derives references from
code and config (an env var that names a bucket, a topic in a constant,
a project in a flag). A provider or project the proxy is not bound to is
an unknown and becomes a question.

### 6.4 Yes/no questioner

A capability of the lead, so questions are batched and deduplicated.
Specialists return `unknown` with what they tried; the lead turns each
into one question that can be answered **yes** or **no** (with the short
"where/which" a *yes* needs), states the dimensions it cannot verify
without the answer, and attaches the file change each answer implies.
Answers arrive as comment replies; the bot derives the file from the
reply and commits or suggests it; the gate lifts when the file exists and
discovery succeeds.

A fifth participant, the **code reader**, is the lead itself reading the
diff and the surrounding code — what a client constructor authenticates
with, which config key a new call reads, which resource name a change
introduces, whether the handler honours the proto — guided by the system
prompt's catalogue of change kinds.

## 7. Memory and configuration

Three layers, three owners. The agent owns none of them durably.

### 7.1 Identifiers: `.paved-agent/discover.yaml` in the repository

The only persistent memory, and it holds identifiers only — never
endpoints, never credentials, never state. Proposed by the bot, reviewed
like code.

```yaml
# .paved-agent/discover.yaml
service: hello
workloads:
  - namespace: hello
    selector: app.kubernetes.io/name=hello     # labels identifying the pods in any cluster
    container: hello                            # telemetry's join key
    clusters: all                               # or a list of cluster ids from the proxy's fleet
    service_account: hello
calls: [frontend]                               # known callers/callees, to skip the org finder
cloud:
  - {provider: gcp, resource: gs://hello-exports}
config:
  - {kind: configmap, name: hello}
  - {kind: secret, name: hello-api-keys}
logs:
  topic: hello                                  # the logs connector's selector for this service
docs:                                           # author-stated, referenced and read live, never copied
  - CLAUDE.md
  - docs/runbook.md
```

Rules:

- **Identifiers only.** A lint rejects URLs, hostnames and anything that
  looks like a secret.
- **Followed every run.** Resolved instances are never written back.
- **Self-healing.** A selector that matches nothing in any cluster, or a
  query that returns empty, marks the entry broken for the run; derivation
  restarts; the outcome is a proposed fix or a question. The report says
  which entry broke and why.
- **Not deployed is an entry too**: `workloads: []` with a probe
  (`probe: {container: greeter}`) that, if it ever returns something,
  re-opens the question.
- **Docs are referenced, not copied.** The lead reads them live each run,
  so they cannot go stale in memory.

No learned invariants, no PR norms. Both go stale and both can be judged
live: the lead reads the surrounding code and the referenced docs at
review time, which is cheaper than maintaining a summary of them.

### 7.2 Run memory

Everything discovery produced this run. It lives in the session, is
summarized in the check's `discovery` block, and is gone after. If a
value seems worth keeping, the right place is an identifier that
re-derives it.

### 7.3 Proxy configuration: bound at install time, inside the boundary

The proxy is deployed by the platform team inside the customer's network
and configured once. The repositories never see any of this.

```yaml
# proxy.yaml — one per deployment of the proxy
profile: kube-state-metrics+istio          # how to turn identifiers into standard queries
fleet:
  cluster_label: cluster                   # the metric label that names a cluster
  clusters: [dev, staging, prod]           # the ids identifiers may refer to
slots:
  metrics: {kind: promql, endpoint: https://thanos.internal, auth: oidc:metrics-reader}
  logs:    {kind: gcp-logging, auth: wif:logs-reader}
  alerts:  {kind: pagerduty, auth: secret:pd-token}
  scm:     {kind: github, org: smallStepGiantLeap, auth: app:paved-agent, allow_bot_commits: [hello, echo]}
  cloud:
    gcp:   {projects: [acme-data, acme-prod], auth: wif:gcp-reviewer}
limits:   {query_range_max: 7d, result_bytes_max: 262144, calls_per_session_max: 200}
```

`auth` values name how the proxy obtains a credential (OIDC, workload
identity federation, a mounted secret), never the credential itself. On
the laptop in M2 this is user ADC with impersonation and a Keychain item;
in a cluster it is the proxy's service account federated to read-only
roles. Nothing changes above the connector when it moves.

The **profile** is code: a table of standard queries for a known stack,
selected by name. For `kube-state-metrics+istio`:

```
instances   count by (cluster) (kube_pod_info{namespace=$ns} * on (pod) group_left
              kube_pod_status_ready{condition="true"} * on (pod) group_left kube_pod_labels{$selector})
owner       kube_pod_owner{owner_kind="ReplicaSet"} → kube_replicaset_owner{owner_kind="Rollout"|"Deployment"}
            kube_pod_owner{owner_kind="StatefulSet"|"DaemonSet"|"Job"}
desired     Rollout → rollout_info_replicas_desired · Deployment → kube_deployment_spec_replicas
            StatefulSet → kube_statefulset_replicas · DaemonSet → kube_daemonset_status_desired_number_scheduled
            unknown → intent from the manifests, lower confidence
ceiling     kube_horizontalpodautoscaler_spec_max_replicas (KEDA creates an HPA) → else desired
image       kube_pod_container_info{container=$c}
traffic     sum by (cluster) (rate(istio_requests_total{destination_workload_namespace=$ns, destination_workload=$w}[5m]))
callers     sum by (cluster, source_workload) (rate(istio_requests_total{destination_workload=$w}[1h]))
p99         histogram_quantile(0.99, sum by (cluster, le) (rate(istio_request_duration_milliseconds_bucket{destination_workload=$w}[5m])))
cpu, mem    container_cpu_usage_seconds_total / kube_pod_container_resource_limits{resource="cpu"};
            container_memory_working_set_bytes / kube_pod_container_resource_limits{resource="memory"}
drift       argocd_app_info{name=$app, sync_status, health_status}
alerts      ALERTS{alertstate="firing", namespace=$ns}; alert rules mentioning $w (rules API)
```

A second profile (`kube-state-metrics+otel`) covers meshless fleets with
OTel/gRPC conventions; adding a stack is adding a profile, reviewed as
code.

## 8. Sample PRs, end to end (code only)

Fixtures live as open draft PRs in the demo org and double as the live
eval. Costs are estimates to be measured in M2; the cap is enforced.

### A. Business logic: `hello` rejects empty names

Diff: `internal/handler/hello.go` returns `InvalidArgument` when `name` is
empty. No manifest changes. `discover.yaml` exists.

- Discovery follows the identifiers: 3 clusters, 2 ready pods each, owner
  Rollout, desired 2, ceiling 4; rps ~5; callers `frontend` from all 3;
  CPU 12% of limit; no firing alerts; `argocd_app_info` synced/healthy.
- The lead reads the diff with the proto: `name` is a plain `string` with
  no comment or validation rule marking it required, so rejecting empty
  input is a behaviour change the contract did not promise; `buf breaking`
  passes (no wire change). The org finder mounts `frontend` (from
  `calls:`); its call site passes the query string through, so empty
  names are possible.
- **Blast radius:** warning — 3 clusters, 1 caller, canary with analysis
  in staging and prod, none in dev. **Correctness:** warning — the proto
  does not declare `name` required and `frontend` forwards user input
  unchecked, so empty names become `InvalidArgument` surfaced to users;
  either document it in the proto or validate in `frontend`. **Scale:**
  info — no load change. **Resilience:** info — the canary's analysis
  counts `InvalidArgument` as a client error, so it would not roll back
  on it; said so. **Debuggability:** info — the status code is counted by
  the platform's interceptor; no alert on it, none needed. **Test
  coverage:** warning — the handler test covers the new branch; no test
  in `frontend` for the error it now receives.
- Questions: none. Proposals: none.
- Verdict: warning. Cost estimate: $0.8–1.5.

### B. New client: `frontend` calls `ledger`

Diff: `internal/web/handler.go` constructs a `ledger` gRPC client and
calls `GetBalance` per page view. `service.yaml` untouched.

- The lead recognises a new client. Discovery follows `frontend`'s
  identifiers (30 rps, 3 clusters) and, via the org finder, mounts
  `ledger` and follows its identifiers (12 rps, owner Rollout, ceiling 3,
  in-flight 60 of threshold 80, CPU 70% of limit). 1 call per page view
  → +30 rps (+250%). The lead reads the constructor: dials
  `ledger.ledger.svc:8080` plain, relies on the mesh; no deadline. Reads
  `ledger`'s `service.yaml`: `frontend` not in `authorizedCallers`.
- **Blast radius:** warning — every page view now depends on `ledger`;
  no other open PR touches `ledger`. **Correctness:** blocking —
  `frontend`'s `egress` lacks `ledger` and `ledger` does not authorize
  `frontend`; the platform's access-request flow is the fix; the e2e
  would fail. **Scale:** blocking — +250% against a ceiling of 3 replicas
  already at 70% CPU and 75% of the in-flight threshold; `ledger` falls
  over before the autoscaler helps. **Resilience:** blocking — no
  deadline; no fallback when `ledger` is down, so a `ledger` outage is a
  `frontend` outage. **Debuggability:** warning — the call is
  instrumented by the platform's client interceptor but the handler
  swallows the error into a 500 without wrapping. **Test coverage:**
  warning — no test for `ledger` unavailable or slow.
- Questions: none. Proposals: `frontend`'s `calls:` gains `ledger`.
- Verdict: blocking. Cost estimate: $1.5–2.5.

### C. Cloud resource: `ledger` exports to GCS

Diff: `internal/export/gcs.go` writes daily exports to the bucket named by
`LEDGER_EXPORT_BUCKET`; `go.mod` adds `cloud.google.com/go/storage`.

- The lead recognises a cloud SDK import. Topology finds no
  `LEDGER_EXPORT_BUCKET` in the pod template or ConfigMap intent, and
  `serviceAccountName: ledger` with no Workload Identity annotation and no
  mounted key. The cloud discoverer has no resource to follow: `ledger`'s
  identifiers list none and the diff names none. Bootstrap cannot derive
  it → **question**, check `action_required`: *"`LEDGER_EXPORT_BUCKET` is
  set nowhere I can see, so I cannot verify correctness (IAM, identity)
  or resilience for the export. Which bucket and project? Reply with
  them and I'll commit the `cloud:` entry."* The author replies
  "gs://ledger-exports in acme-data"; the bot commits the entry; CI
  re-runs.
- Second run: `cloud_get("gcp", "gs://ledger-exports", "iam")` → no
  binding for any `ledger` identity; `protection` → retention 30 days,
  no deletion protection.
- **Blast radius:** info — 3 clusters, a scheduled path. **Correctness:**
  blocking — config absent at runtime; no credential path; no IAM grant.
  **Scale:** info. **Resilience:** warning — no timeout on the upload; no
  retry; a 2 GB export streamed from memory. **Debuggability:** warning —
  errors logged without the bucket or object name. **Test coverage:**
  warning — no test for the upload failing.
- Verdict: blocking. Cost estimate: $1.5–2.5 across the two runs.

### D. Unknown placement: `greeter`

Diff: any handler change in `greeter`. No `.paved-agent/`.

- Bootstrap: the derivation chain finds a Dockerfile and a pod template
  but no overlay, and the instance query returns no ready pod with
  container `greeter` in any cluster → not confident → the question in
  section 4, check `action_required`.
- The author replies "no". The bot commits `workloads: []` with a probe.
  Second run: blast radius zero; correctness against the contract and
  docs only; the rest "not deployed".
- Verdict: info. Cost: $0.3–0.6 for the first run, under $0.3 after.

### E. Refactor: rename a private function in `echo`

- The lead reads the diff: behaviour-preserving, tests unchanged and
  passing in CI. Discovery follows `echo`'s identifiers (cheap: a handful
  of queries) so the report still states where it runs.
- Verdict: info, six dimensions "no impact" with the reason. Cost: under
  $0.3. This case keeps the cheap path cheap.

## 9. Connectors and tools

Everything the agent can do outside the sandbox is a connector call
through the proxy. Five slots, one closed interface each.

| Slot | Operations | Fills with | Read / write |
|---|---|---|---|
| `metrics` | `query`, `query_range`, `series`, `label_values`, `rules` | Prometheus, Thanos, Mimir, AMP, GMP, Victoria | read |
| `logs` | `search(topic, query, range)` → bounded lines | Loki, Cloud Logging, CloudWatch Insights, OpenSearch | read |
| `alerts` | `firing(selector)`, `incidents(service, range)` | Alertmanager, PagerDuty | read |
| `scm` | `search_code`, `read(repo, ref, path)`, `mount(repo, ref)`, `pr(…)`, `open_prs`, `comment_replies` | GitHub, GitLab | read; writes: `comment`, `check`, `propose` (`.paved-agent/` on the PR branch only) |
| `cloud` | `cloud_get(provider, resource, attribute)` with attributes `exists`, `iam`, `quota`, `protection`, `bindings`, `labels` | GCP, AWS adapters | read; credentials via workload identity federation |

A sixth slot, `traces`, is planned for debuggability (does the new path
appear; latency by span).

Inside the sandbox the agent has `read`, `glob`, `grep` and `bash` over
the mounted repositories. Bash is on, because the sandbox is a pod in a
kind cluster (section 10) whose only egress is the Anthropic API and the
proxy service: no credentials, no service-account token, non-root,
read-only root filesystem, CPU and memory limits. The worst bash can do
there is waste its own quota. `go list`, `git`, `buf` are simply in the
image.

The connector operations above are **custom tools registered on the
agents** (typed schemas, so the model gets validated inputs) and
**executed by the sandbox worker** as thin HTTP clients of the proxy
service. The model knows tool names; it does not know, and cannot
affect, who executes them or with what identity. `ask` and `propose` are
two of those tools, backed by the proxy's `scm` writes.

## 10. Runtime and security model

```
 customer boundary: a kind cluster in M2, any cluster later              Anthropic
 ┌──────────────────────────────────────────────────────────────┐    ┌──────────────┐
 │ ns paved-agent                                               │    │ Managed      │
 │ ┌──────────────────────────┐    ┌──────────────────────────┐ │    │ Agents       │
 │ │ sandbox-worker (pod)     │    │ proxy (pod)              │ │    │  sessions,   │
 │ │  SDK environment worker  │───▶│  proxy.yaml: slots,      │ │    │  lead +      │
 │ │  read/glob/grep/bash     │HTTP│  profile, fleet, limits  │ │    │  specialists │
 │ │  connector tools = HTTP  │    │  credentials via WIF/    │ │    │  budgets,    │
 │ │   clients of the proxy   │    │  OIDC/secret; policy;    │ │    │  trace       │
 │ │  repos: unpacked from    │    │  audit log               │ │    └──────┬───────┘
 │ │   the proxy's tarballs   │    │  controller: sessions,   │ │           │
 │ │  egress: Anthropic API,  │    │   check states, collect  │ │  session event
 │ │   proxy — nothing else   │    │                          │ │  stream (every
 │ └──────────────────────────┘    └─────┬─────────┬──────────┘ │  tool call)
 │                                       │         │            │
 └───────────────────────────────────────┼─────────┼────────────┘
                                read-only▼         ▼read + 3 writes to the PR
                        metrics · logs · alerts · cloud APIs    GitHub
```

- **One runner, not two.** The sandbox worker executes every tool the
  model calls: the built-ins locally, the connector tools by calling the
  proxy over the in-cluster network. The session event stream is how the
  model (at Anthropic) reaches tools in our environment at all, so every
  tool call crosses it once; what this layout removes is a second runner
  attached to the same session. The proxy is a plain internal HTTP
  service, which also makes it usable from a CLI or a notebook with the
  same policy.
- **The sandbox has one route out besides the Anthropic API: the proxy.**
  Enforced by a NetworkPolicy on the sandbox pod; no service-account token
  is mounted; the repositories arrive as tarballs the proxy serves after
  cloning with its own token. The sandbox holds no credential of any
  kind. If the model used bash to call the proxy directly instead of
  through a tool, it would get exactly what the tool gives it: policy
  lives in the proxy, not in the caller.
- **The proxy holds every credential and enforces policy.** It exposes
  only the slot interfaces; per call it enforces read-only methods,
  bounded ranges and result sizes, metric and label allowlists where the
  org wants them, per-session call and cost limits; it writes an audit
  line per call; it owns the single write path (`propose` checks the path
  is under `.paved-agent/` and the ref is the PR's head; `comment` and
  `check` are the other two writes). Branch protection still requires a
  human to merge. The controller that creates sessions, posts check
  states and collects reports runs in the same pod.
- **The PR under review is untrusted input.** Any PR can contain text
  aimed at the agent. The worst a fully subverted agent can do is read
  what the org granted the proxy and propose a file change on the very PR
  it is reviewing, which a human then reads. Wide in reach, narrow in
  power. The proxy is the only component worth hardening, and it is
  small: five adapters and a policy layer.
- **Credentials are short-lived.** The proxy pod's service account is
  federated to read-only cloud roles and metric/log readers at call time
  (Workload Identity in GKE; on kind in M2, the operator's ADC mounted
  into the proxy pod only). No long-lived keys; nothing in any
  repository.
- **Why kind and not Docker alone.** The same manifests (two Deployments,
  a NetworkPolicy, a Service) are the production deployment on the
  customer's cluster; kind is that deployment on the laptop. One shape to
  test and ship.
- **GCP for the eval**: a reviewer service account with `roles/viewer` and
  `roles/iam.securityReviewer` on the listed projects; the kind cluster
  carries kube-state-metrics and Istio for placement; a GKE Autopilot
  cluster only while testing Workload Identity (fixture C), created and
  deleted in the same session.

## 11. Implementation design

### 11.1 Packages

```
cmd/change-agent      setup | review | worker | proxy
deploy/               kind manifests: sandbox-worker and proxy Deployments, NetworkPolicy, Service
internal/change       the change (exists)
internal/findings     report contract: six dimensions, findings, unknowns, questions, proposals
internal/identifiers  .paved-agent/discover.yaml: schema, load, lint (identifiers only), derive helpers
internal/profile      standard queries per stack: kube-state-metrics+istio, kube-state-metrics+otel
internal/proxy        the HTTP service: slots (metrics, logs, alerts, scm, cloud), adapters, policy,
                      audit, config, repo tarballs; and the controller (sessions, check states, collect)
internal/connectors   the tools' side: typed schemas registered on the agents, HTTP clients of the proxy
internal/discover     follow identifiers through the proxy with a profile; owner-kind resolution
internal/sandbox      the worker: workdir, unpack tarballs, agenttoolset + connector tools
internal/agents       lead, org-finder, topology, cloud (definitions as code; the change-kind
                      catalogue lives in the lead's system prompt)
internal/session      create, watch, collect the report
internal/github       PRs → changes (exists); checks, comment threads, suggestions, branch commits
internal/classify     milestone-1 rules; kept only until M2 measures a pre-pass
evals/                fixtures (PR refs + expectations), recorded metric/scm responses, judge rubric
```

### 11.2 Session lifecycle

1. `review` (the controller in the proxy pod, or the CLI) loads the PR
   and `.paved-agent/`, lints it, clones the repository with the proxy's
   token, and creates the session: lead agent version, environment,
   budget, initial message = the brief (change, identifiers, referenced
   docs' paths, the roster).
2. The sandbox worker claims the session, fetches the repository tarball
   from the proxy, and serves every tool: built-ins locally, connector
   tools by calling the proxy. The lead delegates.
3. **Discovery** follows the identifiers through the proxy; broken ones
   are reported to the lead as such.
4. **Question**: the lead ends with a report whose `questions` are
   non-empty. The proxy posts each as a comment with its answer-to-file
   mapping and suggested change, sets the check to `action_required`, and
   the session ends. A reply on the thread triggers a short **answer run**
   (lead only, no discovery) that turns the prose into the file and
   commits or suggests it; the push is the next full run. In the CLI the
   operator edits the file and re-runs.
5. **Collect**: a report with no questions is validated (every finding has
   evidence; every dimension has a verdict), attached to the check,
   summarized in a comment; its `proposals` are applied.
6. **Concurrency**: one run per PR; a new head SHA cancels the running
   session and starts over. Nothing is shared between runs except git.

### 11.3 Bootstrap command

`review --bootstrap` (or the first run of a repo with no `.paved-agent/`)
is the same session with derivation allowed to spend more: it reads the
tree, build graph, Dockerfile, manifests and CI, derives `discover.yaml`,
confirms it against the fleet through the proxy, and proposes it. There
is no separate `learn`: nothing else is learned.

### 11.4 Report contract

```json
{"change":"org/repo#N@sha",
 "dimensions":{
   "blast_radius":{"verdict":"warning","summary":"…"},
   "correctness":{"verdict":"blocking","summary":"…"},
   "scale":{"verdict":"blocking","summary":"…"},
   "resilience":{"verdict":"blocking","summary":"…"},
   "debuggability":{"verdict":"warning","summary":"…"},
   "test_coverage":{"verdict":"warning","summary":"…"}},
 "discovery":{"followed":["workloads[0]","calls","cloud[0]"],"broken":[],
              "instances":{"dev":2,"staging":2,"prod":2},"owner":"Rollout","ceiling":{"prod":4},
              "rps":{"prod":4.8},"callers":{"prod":["frontend"]},"cpu_of_limit":{"prod":0.12}},
 "findings":[{"dimension":"scale","severity":"blocking","claim":"…",
   "evidence":[{"kind":"metric","source":"metrics.query","query":"…","value":"…"}],
   "recommendation":"…","confidence":0.85,"studied":["frontend","ledger"]}],
 "unknowns":[{"what":"…","tried":["…"],"blocks":["blast_radius","scale"]}],
 "questions":[{"id":"q1","text":"…","blocks":["blast_radius","scale"],
               "answers":{"yes":{"path":".paved-agent/discover.yaml","content":"…"},
                          "no":{"path":".paved-agent/discover.yaml","content":"…"}}}],
 "proposals":[{"path":".paved-agent/discover.yaml","content":"…","reason":"…"}],
 "verdict":"blocking","summary":"…"}
```

The proxy applies `proposals` and posts `questions`; the model never
writes to git directly.

### 11.5 Agents

| Agent | Model | Connector access | Owns |
|---|---|---|---|
| `impact-lead` | claude-opus-5, effort high | sandbox read tools, `change_diff`, `scm.open_prs`, `metrics.rules`, `alerts`, `ask`, `propose`; roster below | code reading, what to discover, the six dimensions, questions, proposals, the report |
| `org-finder` | claude-sonnet-5 | `scm.search_code`, `scm.mount`, sandbox read tools | finding and mounting the defining repository |
| `topology` | claude-sonnet-5 | sandbox read tools, `repo_cmd`, `metrics`, `logs` | following and deriving workload identifiers; owner kinds, scale inputs, identity, config presence |
| `cloud` | claude-sonnet-5 | `cloud_get` | following and deriving cloud identifiers; identity and IAM |

Analysis stays with the lead in M2 (one Opus context holds all the facts;
fewer hand-offs). If reports get long or slow, split the six dimensions
into Sonnet analysts fed by the lead's fact sheet; the contract does not
change.

## 12. Costs

Model usage is the only material cost; the laptop and kind are free; GCP
is cents outside the Autopilot hours.

| Operation | Tokens (est.) | Cost (est., at Opus $5/$25 and Sonnet $3/$15 per M in/out; verify against the price list) | Cap |
|---|---|---|---|
| Review with identifiers (A, B) | lead ~300k cached + 40k fresh in, 10k out; 2–3 specialists ~150k in, 6k out each | $1–2.5 | $3 |
| Review, cheap path (E, later runs of D) | lead only; discovery is a handful of queries | < $0.3 | $1 |
| Bootstrap run (no identifiers, or a broken one) | adds the derivation chain, ~+200k Sonnet in | +$0.5–1 | $4 |
| Answer run (prose → file) | lead only, tiny | < $0.1 | $0.5 |
| Fixture eval run (5 PRs, 7 runs) | | $6–12 | $15 |
| GKE Autopilot for fixture C | ~2 pods for 2 h | < $1 | delete after |

Levers, in order: identifiers plus a profile turn discovery into a
handful of deterministic queries (the derivation chain runs once per
repo, not per PR); Sonnet for everything that reads, Opus only where it
reasons; prompt caching of the identifiers and referenced docs; a hard
fan-out limit (at most 4 specialists per review); a question ends the
session, so waiting costs nothing; the platform budget as the backstop.
Monthly development target: under $50; `review` prints the session's
cost on exit and the proxy keeps a running monthly total and refuses
past the cap.

## 13. Test plan

| Layer | What | How | Pass |
|---|---|---|---|
| Unit | identifier schema and lint; profile query rendering; owner-kind resolution (Rollout, Deployment, StatefulSet, DaemonSet, unknown); report validation; check-state transitions; proxy policy (read-only methods, path restriction on `propose`, limits) | `go test` | green |
| Recorded | `discover` against recorded kube-state-metrics/Istio/Argo responses from the sgrpc fleet; `cloud_get` against recorded GCP responses | fixtures under `evals/recorded`, no model | deterministic |
| Derivation | the bootstrap chain on each demo repo reproduces the hand-written `discover.yaml` | `go test` with the repos vendored as fixtures, plus one live run per repo | equal, or a documented difference |
| Live fixtures | the five PRs of section 8 as open drafts in the demo org | `make eval-live` (budgeted) | per fixture: required findings present (by dimension, entity, severity), no forbidden claims, cost ≤ cap, questions == expected, proposals == expected files |
| Self-healing | break `hello`'s selector in a fixture branch | eval-live | reported broken, derivation proposes the fix, no false findings |
| Gate | D: first run asks exactly one question and gates; a prose reply produces the file; second run passes with no question | eval-live, CLI first, GitHub check in M5 | state sequence as in section 4 |
| Judge | claim quality against a rubric (evidence matches claim; recommendation actionable; no unsupported numbers) | Sonnet judge over the report JSON, scores stored | ≥ baseline; drift flagged |
| Safety | every connector refuses non-read operations; `propose` only touches `.paved-agent/` on the PR head; the sandbox pod can reach only the Anthropic API and the proxy (NetworkPolicy test from inside the pod), mounts no service-account token, runs non-root on a read-only root; event and proposal scan for `github_pat_`, `ya29.`, bearer tokens; a PR containing instructions to the agent produces no action beyond a proposal on itself | unit + scan + one adversarial fixture in eval-live | zero hits |
| Cost | per-fixture ceilings; later runs of a repo cheaper than its bootstrap; monthly cap | eval-live | CI fails on breach |

Golden reports are kept per fixture; a change to a prompt or a profile
re-runs the fixtures and diffs the judge scores before it is merged.

## 14. Milestones

- **M2 — identifiers, profile, proxy.** `discover.yaml` schema and lint;
  the `kube-state-metrics+istio` profile; the proxy service with
  `metrics` and `scm` slots, policy and tarballs; connector tools as HTTP
  clients; `discover` incl. owner kinds; the derivation chain; the
  questioner on the CLI; the sandbox worker and proxy deployed on kind
  with the NetworkPolicy; kube-state-metrics and Istio metrics on the
  sgrpc fleet; hand-written identifiers for the demo repos; fixtures A,
  D, E. Exit: A reviewed with six dimensions; D
  asks exactly one question and passes after the file lands; E under
  $0.3; the derivation test matches the hand-written files.
- **M3 — cross-repo and signals.** Org finder, `scm.open_prs`, `logs` and
  `alerts` slots, fixture B. Exit: B blocks for correctness, scale and
  resilience with metric and policy evidence.
- **M4 — cloud.** `cloud_get` with the GCP adapter and WIF, fixture C with
  a short-lived Autopilot cluster. Exit: C asks for the bucket once, then
  finds the missing config, identity and IAM grant.
- **M5 — surfaces.** GitHub check with the state machine of section 4,
  question threads with prose answers turned into commits, opt-in bot
  commits. Then Slack notifications linking back to the PR.

## 15. Open for review

1. Six dimensions as in section 3 — scale and test coverage in, and
   "capacity" folded into scale. Agree?
2. Dropping learned invariants and PR norms entirely (judged live from
   the diff, surrounding code and referenced docs) — agree?
3. `allow_bot_commits` per repo in `proxy.yaml`, suggested changes
   elsewhere — agree?
4. The `kube-state-metrics+istio` profile first; which second?
5. Analysis in the lead (one Opus context) vs. six Sonnet analysts — lead
   for M2, then measure.
6. Bash on, inside the kind-deployed sandbox pod with the NetworkPolicy
   — agree that's the boundary we rely on?
7. Anything in section 8 that does not match how you'd expect the reviewer
   to think.
