# paved-road-agent: design

Status: revision 2, 2026-10-02, after the first review. Supersedes the
aspect-routed design in milestone 1; [VISION.md](VISION.md) still holds
the why.

What changed in this revision: memory is no longer a store the agent
keeps. It is **declarative breadcrumbs committed in the repository**
(`.paved-agent/`) that the agent follows on every run, plus ephemeral run
memory that holds what following them produced. Questions are answered by
changing the breadcrumbs in the PR, which re-triggers CI; the check gates
merge while a question is open. The milestone-1 rules move into the lead's
system prompt.

## 1. The model in one paragraph

An agent that **learns a repository and an environment by itself**, and
uses what it learned to assess every pull request on four dimensions:
**blast radius, correctness, debuggability, resilience**. The only
assumption about the environment is Kubernetes. It reviews; it never
executes. What it learns about *how to discover* the environment is
written down declaratively in the repository, reviewed like code, and
followed on every run; what it discovers is re-measured every run and
thrown away after. When the breadcrumbs are missing or stop answering, the
agent derives new ones from the repository and proposes them, or asks a
human a yes/no question in terms of what it could not figure out, and
gates CI until the answer lands as a file change.

## 2. Goals, non-goals, assumptions

Goals

- Any PR, including pure code changes with no manifest in the diff.
- Findings with evidence: a metric, a file and line, a cloud API response,
  or a human's answer. Never an unsupported claim.
- Self-learning: repository structure and conventions, where software runs,
  what it depends on — recorded as declarative breadcrumbs in the repo,
  followed on every run to find every instance; a breadcrumb whose query
  returns nothing is dropped and discovery restarts.
- Bounded cost per review and per onboarding, enforced by the platform.

Non-goals (for now)

- Executing anything in the environment: no deploys, no applies. The one
  write the agent may do is to `.paved-agent/` on the PR branch under
  review, and only where the repo opts in.
- Non-Kubernetes runtimes (VMs, serverless) as placement targets.
- A hosted multi-tenant service. This design runs on one laptop for one org.

Assumptions

- Workloads run on Kubernetes. Everything else (mesh, GitOps tool, cloud,
  metrics stack) is discovered, not assumed.
- The GitHub org is the unit of knowledge: repos and PRs.
- The cloud is GCP first; the cloud discoverer is an interface.
- The reviewer has read-only identities for GitHub, the clusters, metrics
  and GCP, held by the bridge process on the laptop. The model never holds
  any of them, and the repository never contains any of them.

## 3. The four dimensions

Every report has a verdict per dimension and findings under it. The lead
owns the dimensions; the discoverers supply the facts.

| Dimension | The question | Evidence that answers it |
|---|---|---|
| **Blast radius** | Where does the changed code run, who depends on it, and what is the worst case if it is wrong? | instances per site (from the workload breadcrumb), traffic and callers per site (from the metric breadcrumbs), rollout staging (per-site canary or global), feature flags, other open PRs' assessed impact on the same services |
| **Correctness** | Does the change do what the PR says, and will it work where it runs? | the diff against the repo's invariants and PR norms; contract compatibility with callers; config keys and credentials present in the pod spec at each site; IAM and resource existence for cloud access; tests that cover the new path |
| **Debuggability** | When it fails, can an operator tell, and where? | logs/metrics/traces on the new path; error wrapping and status codes; whether the new counter has a dashboard or alert; a flag to turn it off; a runbook entry |
| **Resilience** | Does it fail small and recover? | deadlines and their propagation; retry sanity (idempotency); bounded work; backpressure and overload behaviour; canary with analysis; PDB and spread; rollback without data migration |

"No impact on this dimension, and here is why" is a finding, not an
absence.

## 4. The journey

Service owner opens a PR. A required check, `impact`, appears and moves
through these states:

```
queued ──▶ in_progress: discovering ──▶ in_progress: assessing ──▶ completed
                 │                                                   success  (info)
                 │ breadcrumbs missing or broken,                    neutral  (warning)
                 │ and the agent cannot derive them                  failure  (blocking)
                 ▼
          action_required: pending human response   ◀── merge is blocked here
                 │
                 │ someone with write access to the PR (or the bot) adds or
                 │ fixes .paved-agent/… on the PR branch → push → CI re-runs
                 ▼
          queued (again, now with breadcrumbs)
```

The question is written in terms of what the agent could not figure out,
and it appears in three places: the check's output (the session log), a PR
comment, and Slack if configured:

> **impact: pending human response.** I could not find where `greeter`
> runs: no workload breadcrumb in `.paved-agent/`, no pod template or
> overlay in this repo, and no container named `greeter` in any cluster
> I can see. Is `greeter` deployed anywhere today?
>
> - **No** → add `.paved-agent/discover.yaml` with `workloads: []`
>   (I've attached one as a suggested change).
> - **Yes** → add the label selector and the cluster(s), as in the
>   suggestion, and I'll take it from there.

Whoever has permission on the PR answers, by committing the file. Often
the bot answers itself: when it can derive the breadcrumbs from the
repository (section 6.2) with confidence, it commits them to the PR branch
(opt-in per repo) or attaches them as a suggested change, and the
re-triggered run proceeds. Once the file exists, the next stage begins:
the actual targets are discovered, TPS is calculated, callers are listed,
cloud references are checked, and the four dimensions are assessed.

No routing rules about who should answer. The PR is the conversation; the
file is the answer; the gate lifts when discovery succeeds.

Surfaces, in order of delivery: the CLI (exists; the operator edits the
file and re-runs), the GitHub check + comments + suggested changes (M5),
Slack notification with a link back to the PR (after M5). One engine.

## 5. Pipeline

```
 PR ──▶ intake ──▶ bootstrap ──▶ DISCOVERY ─────────────▶ ANALYSIS ──▶ report ──▶ propose
         load      breadcrumbs   follow the breadcrumbs:   four           check,     .paved-agent
         PR, repo, present?      instances per site, TPS,  dimensions     comment    changes as
         .paved-   derive →      callers, identities,                                commits or
         agent/    propose,      config presence, cloud                              suggestions
                   or ask + gate
```

1. **Intake.** Load the PR (files, patches, both sides of configs) and
   `.paved-agent/` from the repo and from the org-level breadcrumb repo.
2. **Bootstrap.** If the breadcrumbs the change needs are present and
   valid, continue. If missing, the topology and cloud discoverers try to
   *derive* them from the repository (section 6.2's chain). Confident →
   propose and continue with the derived values; not confident → ask, and
   the check gates.
3. **Discovery.** Follow the breadcrumbs. Each one is a selector or a query
   with a declared source; following it is deterministic code, not
   reasoning. The results — instances per site, replicas, rps, callers,
   in-flight work, identities, mounted config, cloud resource state — are
   the **run memory**: the actuation of the declarative state, kept for
   this run only. A breadcrumb that returns nothing is broken: it is
   dropped for this run, derivation restarts from the repo, and the result
   is a proposal or a question.
4. **Analysis.** The lead reasons through the four dimensions with the
   diff, the repo's invariants and norms, and the run memory. Where a
   dimension needs more evidence it goes back to a discoverer.
5. **Report.** One JSON document (section 11.4), rendered to the check,
   the comment, and stdout.
6. **Propose.** Any breadcrumb the run derived or corrected, and any
   update to the learned repo knowledge, becomes a change to
   `.paved-agent/` on the PR branch: a commit where the repo opted in, a
   suggested change otherwise. Humans review it like any other diff.

What the lead checks for each kind of change (a new client, a cloud SDK
import, a Dockerfile, a handler, a Terraform plan) is guidance in its
**system prompt**, not a rule engine in front of it. The milestone-1 rules
stay in the repo only until M2 measures whether a cheap deterministic
pre-pass saves enough tokens to keep; if not, they go.

## 6. Discoverers

The four capabilities named in review. The first three are specialists
(their own agents and tools); the fourth is a capability of the lead.

### 6.1 GitHub org finder

Finds the repository that holds something — the callee of a new client,
the org-level breadcrumb, the chart that deploys the mesh, the module a
Dockerfile builds from — and makes it readable. Searches org-wide code
(names it read in manifests or breadcrumbs: a chart, a CRD kind, a
DaemonSet, an import path), ranks by where hits cluster, mounts the
repository into the session, reads it. A search that finds nothing is an
unknown: *"Where is SPIRE's deployment defined?"* — answered by a
breadcrumb in the org file: `components: {spire: {repo: infra/platform-mesh,
path: charts/spire}}`.

### 6.2 Deployment topology discoverer

On a normal run it **follows** the workload and metric breadcrumbs: label
selector → `kube_get` per cluster in the fleet; container name → the
metric filters → `prom_query` per site; identity and config → the pod
spec at each site. Deterministic, a handful of calls.

At **bootstrap**, or when a breadcrumb breaks, it **derives** them from
the repository, in this order, stopping when sources agree:

```
build graph        go list -deps from each cmd/: which binaries include the change
 → Dockerfile       COPY/ENTRYPOINT: which binary → which image; what else ships in it
 → CI publish       the image name and digest it pushes
 → pod template     containers[].image, containers[].name (telemetry's join key),
                    labels (the selector), serviceAccountName, env/envFrom, volumes,
                    nodeSelector, affinity, tolerations, topologySpreadConstraints
 → overlays         kustomize envs, Helm values per cluster, ApplicationSet
                    generators and label selectors, fleet inventory files
 → telemetry        kube_pod_info{container=…} by cluster; the mesh's request
                    metrics by destination — to confirm the derived selector and
                    filters actually return something
 → propose or ask   confident: write discover.yaml; not: the question names
                    exactly which step found nothing
```

The first four are deterministic reads of the repo. Companies diverge at
the overlay level; that is where a human is most likely needed, once.

### 6.3 Cloud resource discoverer

Follows the cloud breadcrumbs (project, named resources) with a read-only
identity per project (`roles/viewer` + `roles/iam.securityReviewer`,
impersonated by the bridge): does the resource exist, what IAM it carries,
what quota it consumes, whether it holds data and is protected. Paired
with the topology discoverer for the runtime half: which KSA the pod runs
as, whether it maps to a GSA (Workload Identity annotation) or a mounted
key, and whether that identity holds the role the code needs. At
bootstrap it derives references from code and config (an env var that
names a bucket, a topic in a constant, a project in a flag). No access to
a project is an unknown: *"I cannot read IAM in project `acme-data`. Does
`ledger@acme-prod.iam` have `roles/storage.objectCreator` on
`gs://ledger-exports`?"* — answered by adding the project to the org
breadcrumb's credential notes, or by a yes/no in a breadcrumb the agent
cannot verify and says so in the report.

### 6.4 Yes/no questioner

A capability of the lead, not of specialists, so questions are batched and
deduplicated. Specialists return `unknown` with what they tried; the lead
turns each into one question that can be answered **yes** or **no**, with
the file change each answer implies, and attaches the suggested change
when it has a candidate. The answer is the file: the agent never parses
prose, it reads `.paved-agent/` on the next run. Questions have a deadline
(24h; after it the report ships with the unknowns as warnings and the
check stays `action_required`) and are logged in the check output.

A fifth participant, the **code reader**, is the lead itself reading the
diff and the surrounding code — what a client constructor authenticates
with, which config key a new call reads, which resource name a change
introduces — guided by the system prompt's catalogue of change kinds.

## 7. Memory

Two kinds, two lifetimes. **Declarative** memory lives in git and says how
to discover. **Run** memory lives in the session and holds what discovery
found. Nothing else persists.

### 7.1 Breadcrumbs: `.paved-agent/` in the repository

Declarative, reviewed like code, proposed by the bot, never containing a
credential. Per repository:

```yaml
# .paved-agent/discover.yaml — how to find this service in the environment
service: hello
workloads:
  - kubernetes:
      selector: app.kubernetes.io/name=hello     # identifies the workload in any cluster
      container: hello                            # telemetry's join key
      clusters: fleet                             # every cluster the org breadcrumb lists,
                                                  # or an explicit list, or a cluster label
metrics:
  rps:      sum by (cluster) (rate(vikrant_rpcs_total{service="hello"}[5m]))
  inflight: max by (cluster) (vikrant_inflight_rpcs{service="hello"})
  p99:      histogram_quantile(0.99, sum by (cluster, le) (rate(vikrant_rpc_duration_seconds_bucket{service="hello"}[5m])))
  callers:  sum by (cluster, source) (rate(istio_requests_total{destination_service=~"hello\\..*"}[1h]))
cloud:
  gcp:
    project: acme-data
    resources: [gs://hello-exports]                # by name; the bridge has the identity
config:
  # where the service reads its configuration, so presence can be checked per site
  - envFrom: configmap/hello
  - secret: hello-api-keys
```

```yaml
# .paved-agent/org.yaml — in the org's breadcrumb repo (the fleet repo); how to reach the environment
fleet:
  clusters:                                       # or: discover: {kubeconfig-contexts: "kind-*"}
    - {name: dev,     context: kind-sgrpc-dev,     labels: {tier: dev}}
    - {name: staging, context: kind-sgrpc-staging, labels: {tier: staging}}
    - {name: prod,    context: kind-sgrpc-prod,    labels: {tier: prod}}
metrics:
  endpoint: http://localhost:9090                  # prod: https://thanos.internal
  credential: metrics-reader                       # a NAME the bridge maps to its own identity
logs:
  endpoint: gcp-logging
  credential: logs-reader
gcp:
  projects: [acme-data, acme-prod]
  credential: gcp-reviewer                         # "impersonate reviewer@acme-ops.iam"
components:
  spire: {repo: infra/platform-mesh, path: charts/spire}
```

Rules:

- **Never credentials.** Credential fields are names; the bridge maps a
  name to a Keychain item or an impersonation target in its own config. A
  lint rejects any value that looks like a secret.
- **Followed every run.** The resolved instances are never written back
  as if they were the rule.
- **Self-healing.** A selector that matches nothing anywhere, or a query
  that returns empty, marks the breadcrumb broken for this run: discovery
  restarts from the repository (6.2), and the outcome is a proposed fix or
  a question. The report says which breadcrumb broke and why.
- **Not deployed is a breadcrumb too**: `workloads: []` with a probe
  (`probe: kube_pod_info{container="greeter"}`) that, if it ever returns
  something, re-opens the question.
- **Reviewed like code.** The bot's proposals are commits on the PR branch
  (per-repo opt-in) or suggested changes. A human merging the PR accepts
  the breadcrumbs with it.

### 7.2 Learned repository knowledge, also in `.paved-agent/`

What the agent learned by reading the repo deeply once, kept next to the
breadcrumbs and refreshed by proposal when merged PRs change it:

- `brief.md`: structure (where `cmd/`, `deploy/`, manifests, tests live, or
  that manifests live in another repo), build and image, how it is
  deployed and by what, the services it defines and calls, and pointers
  to everything author-stated: `CLAUDE.md`, `AGENTS.md`, `.cursorrules`,
  `CONTRIBUTING`, ADRs, runbooks. Author-stated documents are the
  highest-trust source of invariants and are quoted, not paraphrased.
- `invariants.yaml`: conventions the repo keeps, each with evidence and a
  count: "every RPC handler increments `vikrant_rpcs_total`" (12/12),
  "config is read via `envFrom` a ConfigMap named after the service",
  "every outbound call sets a deadline". Correctness and debuggability
  check the diff against them.
- `norms.yaml`: from the last N merged PRs — typical size, files that
  change together (`service.yaml` with `e2e/run.sh`), description
  template, required checks — and review norms: what reviewers recur on
  ("missing deadline", "no test for the error path"). A PR that breaks a
  norm is flagged with the norm's evidence; a repo with no norms gets none.

Produced by `learn --repo` as a PR to the repo (section 11.3): the bot's
first contribution is its own understanding, for the team to correct.

### 7.3 Run memory

Everything following the breadcrumbs produced: instances per site,
replicas, rps, callers, in-flight work, identities, mounted config, cloud
resource state, and which breadcrumbs were followed or broke. It lives in
the session and is summarized in the assessment attached to the check.
It is lost after the run by design: the next run re-measures. If a value
is worth keeping, the right place for it is a breadcrumb that re-derives
it.

### 7.4 Across PRs

Two PRs each fine alone and together over capacity: the agent lists the
org's other open PRs whose assessments touch the same services (each
assessment is a check artifact) and includes their claimed deltas in the
blast-radius reasoning. No shared store; the PRs are the store. M3.

## 8. Sample PRs, end to end (code only)

Fixtures live as open draft PRs in the demo org and double as the live
eval. Costs are estimates to be measured in M2; the cap is enforced.

### A. Business logic: `hello` rejects empty names

Diff: `internal/handler/hello.go` returns `InvalidArgument` when `name` is
empty. No manifest changes. `.paved-agent/discover.yaml` exists.

- Intake: breadcrumbs for `hello` and the org file. Discovery follows
  them: selector → 3 clusters, 2 replicas each; `rps` → ~5; `callers` →
  `frontend` from all 3 sites.
- The lead reads the diff with `brief.md` and `invariants.yaml`; the
  org finder mounts `frontend` (a caller) and the lead reads its call
  site: it passes the query string through, so empty names are possible.
- Analysis. **Blast radius:** 3 sites, 1 caller, ~5 rps; rollout has a
  canary with error-rate analysis in staging and prod, none in dev.
  **Correctness:** warning — `frontend` forwards user input unchecked, so
  empty names become `InvalidArgument` surfaced to users; the handler test
  covers the new branch (norm met). **Debuggability:** info — the status
  code is counted by `vikrant_rpcs_total{code="InvalidArgument"}`
  (invariant); no alert on it, none needed. **Resilience:** info — the
  canary's analysis counts `InvalidArgument` as a client error, so the
  canary would not roll back on it; said so.
- Questions: none. Proposals: none.
- Verdict: warning. Cost estimate: $0.8–1.5.

### B. New client: `frontend` calls `ledger`

Diff: `internal/web/handler.go` constructs a `ledger` gRPC client and
calls `GetBalance` per page view. `service.yaml` untouched.

- The lead recognises a new client (prompt catalogue). Discovery follows
  `frontend`'s breadcrumbs (30 rps, 3 sites) and, via the org finder,
  mounts `ledger` and follows its breadcrumbs (12 rps, KEDA ceiling 3
  replicas at threshold 80 in-flight). 1 call per request → +30 rps
  (+250%). The lead reads the constructor: dials `ledger.ledger.svc:8080`
  plain, relies on the mesh for mTLS; no deadline; reads `ledger`'s
  `service.yaml`: `frontend` not in `authorizedCallers`. The org file's
  `components.spire` points at the mesh repo; its placement resolves to all
  3 sites and `up{job="spire-agent"}` confirms.
- Analysis. **Blast radius:** every page view now depends on `ledger`;
  no other open PR touches `ledger`. **Correctness:** blocking —
  `frontend`'s `egress` lacks `ledger` (NetworkPolicy + Sidecar block the
  connection) and `ledger` does not authorize `frontend`; the platform's
  access-request flow is the fix; the e2e would fail. **Debuggability:**
  warning — the call is instrumented by the platform's client interceptor
  (invariant) but the handler swallows the error into a 500 without
  wrapping. **Resilience:** blocking — no deadline; +250% exceeds the
  KEDA ceiling; no fallback when `ledger` is down, so a `ledger` outage is
  a `frontend` outage.
- Questions: none. Proposals: `frontend`'s `discover.yaml` gains
  `calls: [ledger]` (so later runs know without the org finder).
- Verdict: blocking. Cost estimate: $1.5–2.5.

### C. Cloud resource: `ledger` exports to GCS

Diff: `internal/export/gcs.go` writes daily exports to the bucket named by
`LEDGER_EXPORT_BUCKET`; `go.mod` adds `cloud.google.com/go/storage`.

- The lead recognises a cloud SDK import. Topology follows `ledger`'s
  breadcrumbs and finds no `LEDGER_EXPORT_BUCKET` in the pod template or
  ConfigMap at any site, and `serviceAccountName: ledger` with no Workload
  Identity annotation and no mounted key. The cloud discoverer has no
  bucket name to follow: `ledger`'s breadcrumbs list no cloud resources and
  the diff names none. Bootstrap cannot derive it → **question**, check
  `action_required`: *"`LEDGER_EXPORT_BUCKET` is set nowhere I can see.
  Which bucket and project? Add them under `cloud.gcp` in
  `.paved-agent/discover.yaml` (suggestion attached)."* The author commits
  `project: acme-data, resources: [gs://ledger-exports]`; CI re-runs.
- Second run: cloud discoverer: bucket exists, IAM has no binding for any
  `ledger` identity, retention 30 days, no deletion protection.
- Analysis. **Blast radius:** 3 sites; a scheduled path. **Correctness:**
  blocking — config absent at runtime; no credential path (no WI binding,
  no key); no IAM grant. **Debuggability:** warning — errors logged
  without the bucket or object name. **Resilience:** warning — no timeout
  on the upload; no retry; a 2 GB export streamed from memory.
- Verdict: blocking. Cost estimate: $1.5–2.5 across the two runs.

### D. Unknown placement: `greeter`

Diff: any handler change in `greeter`. No `.paved-agent/`.

- Bootstrap: the derivation chain finds a Dockerfile and a pod template
  but no overlay, no fleet entry, and no container named `greeter` in any
  cluster → not confident → question (the one in section 4), check
  `action_required`, suggested `discover.yaml` with `workloads: []` and a
  probe attached.
- The author commits the suggestion. Second run: `workloads: []` →
  blast radius zero; correctness against invariants only; the rest "not
  deployed".
- Verdict: info. Cost: $0.3–0.6 for the first run, under $0.3 for the
  second and every later one.

### E. Refactor: rename a private function in `echo`

- The lead reads the diff with the brief: behaviour-preserving, tests
  unchanged and passing in CI. Discovery follows `echo`'s breadcrumbs
  (cheap: four queries) so the report still states where it runs.
- Verdict: info, four dimensions "no impact" with the reason. Cost: under
  $0.3. This case keeps the cheap path cheap.

## 9. Tools

By side: **sandbox** tools run where the repository is (the self-hosted
worker's working directory, confined); **host** tools run in the bridge
process with the read-only credentials. The model sees results only.

| Tool | Side | Credential | Used by |
|---|---|---|---|
| `read`, `glob`, `grep` | sandbox | none | all |
| `repo_cmd` — `go list`, `git log/show`, allow-listed, timed | sandbox | none | topology, lead |
| `change_diff` | host | none | all |
| `breadcrumbs` — the parsed, validated `.paved-agent/` of the PR repo and the org repo | host | none | all |
| `gh_search_code` (org scope) | host | GitHub read | org finder |
| `gh_repo_mount` (clone at a ref into the session) | host | GitHub read | org finder |
| `gh_open_prs` (open PRs and their assessments, M3) | host | GitHub read | lead |
| `gh_pr_history` (merged PRs, files, review comments) | host | GitHub read | learn |
| `kube_get` (get/list, allow-listed kinds, any fleet cluster) | host | read-only RBAC | topology |
| `prom_query` (a breadcrumb filter, per site) | host | `metrics.credential` | topology, lead |
| `logs_query` | host | `logs.credential` | lead (M4) |
| `gcp_resource`, `gcp_iam_policy`, `gcp_quota`, `gcp_sa_bindings` | host | `gcp.credential` (impersonated) | cloud |
| `propose` — a change to `.paved-agent/` on the PR branch, as a commit (opt-in) or suggested change | host | GitHub write to the PR | lead only |
| `ask` — a yes/no question with the file change each answer implies | host | GitHub write to the PR (comment, check) | lead only |

`propose` and `ask` are the only writes, both to the pull request under
review, both visible in the PR. `bash` is off in M2: the sandbox has no
network and no credentials, but a general shell on the operator's laptop
is not a boundary I want to rely on. `repo_cmd` covers what reading
needs. Bash returns when the worker runs in a container.

## 10. Runtime: self-hosted on the laptop, GCP as the cloud

```
 laptop                                              Anthropic
 ┌──────────────────────────────────────────┐        ┌──────────────────┐
 │ change-agent worker                      │ poll   │ Managed Agents   │
 │  self-hosted environment worker          │◀──────▶│  sessions, lead  │
 │  (SDK EnvironmentWorker + agenttoolset)  │ events │  + specialists   │
 │  workdir: ~/.paved-road/work/<session>/  │        │  budgets, trace  │
 │  tools: read/glob/grep/repo_cmd          │        └──────────────────┘
 │                                          │
 │ change-agent bridge                      │ events
 │  SessionToolRunner for host tools        │◀──────▶  (same session;
 │  credential names → Keychain (GitHub),   │           each runner owns
 │    ADC + SA impersonation (GCP),         │           its tool names)
 │    kubeconfig contexts, metrics token    │
 │  no memory of its own                    │
 └──────────────────────────────────────────┘
        │ read-only                │ read-only
        ▼                          ▼
   kind cluster(s) sgrpc       GCP project (bucket, SA, IAM; GKE Autopilot
   Prometheus (port-forward)   only while testing Workload Identity)
```

- **Environment**: one self-hosted environment per org, created by
  `setup`; its environment key lives in the Keychain. The worker claims
  session work items, clones the PR's repository (and the org breadcrumb
  repo) into the session workdir at the head SHA, serves the sandbox
  tools, heartbeats. The bridge serves the host tools for the same
  session. The SDK's runner leaves tool names it does not own pending for
  their owner, so the two co-serve.
- **Default org**: the user's GitHub org, read through a fine-grained
  token with contents, metadata, pull requests (read); pull requests
  (write) for `ask` and `propose`, and checks (write) for the gate in M5.
- **GCP**: a reviewer service account with `roles/viewer` and
  `roles/iam.securityReviewer` on each listed project; the bridge
  impersonates it via ADC; no key file.
- **Clusters**: kind for everything placement-related (free); the org
  breadcrumb's `fleet.clusters` lists the contexts. A GKE Autopilot cluster
  only for Workload Identity tests (fixture C), created and deleted in the
  same session.
- **Boundaries**: the model holds nothing; the repo holds names, never
  secrets; the bridge holds read-only credentials and the PR write. Every
  session's events and every proposed `.paved-agent/` change are scanned
  for credential patterns.

## 11. Implementation design

### 11.1 Packages

```
cmd/change-agent      setup | learn | review | worker | bridge
internal/change       the change (exists)
internal/findings     report contract: dimensions, findings, unknowns, questions, proposals
internal/breadcrumbs  schema, load, validate (incl. the no-secrets lint), follow (selector →
                      kube_get, filter → prom_query, cloud ref → gcp_*), derive-at-bootstrap helpers
internal/tools        host tools by group: github, topology, cloud/gcp, telemetry, propose, ask
internal/sandbox      the worker: workdir, clone, repo_cmd, agenttoolset subset
internal/agents       lead, org-finder, topology, cloud (definitions as code; the change-kind
                      catalogue lives in the lead's system prompt)
internal/session      create, run both runners, collect the report, gate state
internal/github       PRs → changes (exists); checks, comments, suggestions, branch commits
internal/classify     milestone-1 rules; kept only until M2 measures a pre-pass, then deleted or kept
evals/                fixtures (PR refs + expectations), replay transcripts, judge rubric
```

### 11.2 Session lifecycle

1. `review` loads the PR and both `.paved-agent/` directories, validates
   them (schema, no secrets), and creates the session: lead agent version,
   environment, budget, repo resources (the worker clones them), initial
   message = the brief (change, the breadcrumbs, `brief.md`, invariants
   and norms excerpts, the roster).
2. The worker and the bridge attach; tool calls flow; the lead delegates.
3. **Discovery** follows the breadcrumbs through host tools; broken ones
   are reported to the lead as such.
4. **Question**: the lead ends with a report whose `questions` are
   non-empty. The bridge posts each as a comment with its suggested
   change, sets the check to `action_required`, and the session ends.
   There is no pause/resume: the answer is a push, and the push is a new
   run. In the CLI, the operator edits the file and re-runs.
5. **Collect**: a report with no questions is validated (every finding
   has evidence; every dimension has a verdict), attached to the check,
   summarized in a comment, and its `proposals` are applied as a commit
   or suggested changes.
6. **Concurrency**: one run per PR; a new head SHA cancels the running
   session and starts over. Nothing is shared between runs except git.

### 11.3 Learn

`learn --repo org/repo [--prs 30]`: a session with the lead and the org
finder, plus discovery tools for the confirmation step. Reads the tree,
build graph, Dockerfile, manifests, CI, docs, and PR history; derives
`discover.yaml`, confirms it against the fleet and telemetry, writes
`brief.md`, `invariants.yaml`, `norms.yaml`; opens a PR to the repo with
`.paved-agent/`. `learn --org`: no model; writes `org.yaml` from the
bridge's kubeconfig contexts and configured endpoints, as a PR to the org
breadcrumb repo. Both cost-capped.

### 11.4 Report contract

```json
{"change":"org/repo#N@sha",
 "dimensions":{
   "blast_radius":{"verdict":"warning","summary":"…"},
   "correctness":{"verdict":"blocking","summary":"…"},
   "debuggability":{"verdict":"info","summary":"…"},
   "resilience":{"verdict":"blocking","summary":"…"}},
 "discovery":{"followed":["workloads[0]","metrics.rps","metrics.callers"],
              "broken":[],"instances":{"dev":2,"staging":2,"prod":2},"rps":{"prod":4.8}},
 "findings":[{"dimension":"correctness","severity":"blocking","claim":"…",
   "evidence":[{"kind":"config","source":"kube_get networkpolicy frontend/allow-egress@prod","value":"…"}],
   "recommendation":"…","confidence":0.85,"studied":["frontend","ledger"]}],
 "unknowns":[{"what":"…","tried":["…"]}],
 "questions":[{"id":"q1","text":"…","answers":{"yes":{"path":".paved-agent/discover.yaml","content":"…"},
                                               "no":{"path":".paved-agent/discover.yaml","content":"…"}}}],
 "proposals":[{"path":".paved-agent/discover.yaml","content":"…","reason":"…"}],
 "verdict":"blocking","summary":"…"}
```

The bridge applies `proposals` and posts `questions`; the model never
writes to git directly.

### 11.5 Agents

| Agent | Model | Tools | Owns |
|---|---|---|---|
| `impact-lead` | claude-opus-5, effort high | sandbox read tools, `change_diff`, `breadcrumbs`, `gh_open_prs`, `propose`, `ask`; roster below | code reading, what to discover, the four dimensions, questions, proposals, the report |
| `org-finder` | claude-sonnet-5 | `gh_search_code`, `gh_repo_mount`, sandbox read tools, `breadcrumbs` | finding and mounting the defining repository |
| `topology` | claude-sonnet-5 | sandbox read tools, `repo_cmd`, `breadcrumbs`, `kube_get`, `prom_query` | following and deriving workload/metric/config breadcrumbs |
| `cloud` | claude-sonnet-5 | `breadcrumbs`, `gcp_*` | following and deriving cloud breadcrumbs; identity and IAM |

Analysis stays with the lead in M2 (one Opus context holds all the facts;
fewer hand-offs). If reports get long or slow, split the four dimensions
into Sonnet analysts fed by the lead's fact sheet; the contract does not
change.

### 11.6 Configuration

`~/.paved-road/config.yaml` holds only what cannot live in git: the org,
the org breadcrumb repo, the credential bindings (`metrics-reader` →
Keychain item, `gcp-reviewer` → impersonation target, kube contexts →
kubeconfig), budgets (per review, per learn, monthly), and the question
deadline. Everything about *what* to discover is in the repos.

## 12. Costs

Model usage is the only material cost; the laptop and kind are free; GCP
is cents outside the Autopilot hours.

| Operation | Tokens (est.) | Cost (est., at Opus $5/$25 and Sonnet $3/$15 per M in/out; verify against the price list) | Cap |
|---|---|---|---|
| Review with breadcrumbs (A, B) | lead ~300k cached + 40k fresh in, 10k out; 2–3 specialists ~150k in, 6k out each | $1–2.5 | $3 |
| Review, cheap path (E, second runs of D) | lead only; discovery is four queries | < $0.3 | $1 |
| Bootstrap run (first run of a repo, or a broken breadcrumb) | adds the derivation chain, ~+200k Sonnet in | +$0.5–1 | within the review cap |
| Learn a repo | Sonnet-heavy, ~1M in, 25k out | $3–6 | $10 |
| Learn org | no model | $0 | — |
| Fixture eval run (5 PRs, 7 runs) | | $6–12 | $15 |
| GKE Autopilot for fixture C | ~2 pods for 2 h | < $1 | delete after |

Levers, in order: breadcrumbs turn discovery into a handful of
deterministic queries (the bootstrap chain runs once per repo, not per
PR); Sonnet for everything that reads, Opus only where it reasons; prompt
caching of the brief, invariants and breadcrumbs; a hard fan-out limit
(at most 4 specialists per review); a question ends the session, so
waiting costs nothing; the platform budget as the backstop. Monthly
development target: under $50; `review` prints the session's cost on
exit and the bridge keeps a running monthly total and refuses past the
cap.

## 13. Test plan

| Layer | What | How | Pass |
|---|---|---|---|
| Unit | breadcrumb schema and the no-secrets lint; `follow` (selector → calls, filter → calls) against fakes; report validation; check-state transitions; tool input guards | `go test` | green |
| Derivation | the bootstrap chain on each demo repo produces the hand-written `discover.yaml` (same selector, container, filters) | `go test` with the repos vendored as fixtures, plus one live run per repo | equal, or a documented difference |
| Replay | host tools against recorded sessions: same inputs, same outputs, no credential in any event or proposal | transcripts under `evals/replay`, no model | deterministic |
| Live fixtures | the five PRs of section 8 as open drafts in the demo org | `make eval-live` (budgeted) | per fixture: required findings present (by dimension, entity, severity), no forbidden claims, cost ≤ cap, questions == expected, proposals == expected files |
| Self-healing | break `hello`'s selector in a fixture branch | eval-live | the breadcrumb is reported broken, derivation proposes the fix, no false findings |
| Gate | D: first run asks exactly one question and gates; the suggested file is applied; second run passes with no question | eval-live, CLI first, GitHub check in M5 | state sequence as in section 4 |
| Judge | claim quality against a rubric (evidence matches claim; recommendation actionable; no unsupported numbers) | Sonnet judge over the report JSON, scores stored | ≥ baseline; drift flagged |
| Safety | every tool refuses non-read verbs; `propose` only touches `.paved-agent/`; sandbox confined to workdir; event and proposal scan for `github_pat_`, `ya29.`, bearer tokens | unit + scan in eval-live | zero hits |
| Cost | per-fixture ceilings; the second run of a repo cheaper than the first; monthly cap | eval-live | CI fails on breach |

Golden reports are kept per fixture; a change to a prompt re-runs the
fixtures and diffs the judge scores before it is merged.

## 14. Milestones

- **M2 — breadcrumbs and discovery.** Schema, lint, `follow`, the
  derivation chain, `learn --repo/--org` as PRs, the questioner on the CLI
  (gate = the CLI exit code and the file to edit), self-hosted worker +
  bridge, hand-written breadcrumbs for the demo repos, fixtures A, D, E.
  Exit: A reviewed on the sgrpc fleet with the four dimensions; D asks
  exactly one question and passes after the file lands; E under $0.3;
  the derivation test matches the hand-written files.
- **M3 — cross-repo.** Org finder, `gh_open_prs` for cross-PR impact,
  fixture B. Exit: B blocks for the right two reasons with metric and
  policy evidence.
- **M4 — cloud.** GCP discoverer, `logs_query`, fixture C with a
  short-lived Autopilot cluster. Exit: C asks for the bucket once, then
  finds the missing config, identity and IAM grant.
- **M5 — surfaces.** GitHub check with the state machine of section 4,
  comments with suggested changes, opt-in bot commits. Then Slack
  notifications linking back to the PR.

## 15. Open for review

1. **Bot commits to the PR branch**: opt-in per repo (a key in
   `org.yaml`), suggested changes otherwise — agree?
2. **Where the org breadcrumb lives**: the fleet repo, as proposed, or a
   dedicated `<org>/.paved-agent` repo?
3. **Change-kind catalogue in the system prompt** replaces the rule
   engine; the milestone-1 classifier is kept only until M2 measures
   whether a pre-pass pays — agree to delete it if it doesn't?
4. Analysis in the lead (one Opus context) vs. four Sonnet analysts —
   lead for M2, then measure.
5. Bash off until the worker runs in a container.
6. Question deadline 24h, after which the report ships with warnings and
   the gate stays — agree?
7. Anything in section 8 that does not match how you'd expect the reviewer
   to think.
