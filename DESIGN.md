# paved-road-agent: design

Status: draft for review, 2026-10-01. Supersedes the aspect-routed design in
the first milestone; [VISION.md](VISION.md) still holds the why.

## 1. The model in one paragraph

An agent that **learns a repository and an environment by itself**, and uses
what it learned to assess every pull request on four dimensions: **blast
radius, correctness, debuggability, resilience**. The only assumption about
the environment is Kubernetes. It reviews; it never executes. When it
cannot find an answer in code, manifests, telemetry or memory, it asks a
human a yes/no question, remembers the answer as a rule, and does not ask
again. Each review makes the next one cheaper.

## 2. Goals, non-goals, assumptions

Goals

- Any PR, including pure code changes with no manifest in the diff.
- Findings with evidence: a metric, a file and line, a cloud API response,
  or a human's answer. Never an unsupported claim.
- Self-learning: repository structure and conventions, where software runs,
  what it depends on, who owns what — discovered, confirmed once, reused.
- Bounded cost per review and per onboarding, enforced by the platform.

Non-goals (for now)

- Executing anything: no deploys, no applies, no writes to the environment.
- Non-Kubernetes runtimes (VMs, serverless) as placement targets.
- A hosted multi-tenant service. This design runs on one laptop for one org.

Assumptions

- Workloads run on Kubernetes. Everything else (mesh, GitOps tool, cloud,
  metrics stack) is discovered, not assumed.
- The GitHub org is the unit of knowledge: repos, PRs, CODEOWNERS.
- The cloud is GCP first; the cloud discoverer is an interface.
- The reviewer has read-only identities for GitHub, the clusters, metrics
  and GCP. The model never holds any of them.

## 3. The four dimensions

Every report has a verdict per dimension and findings under it. The lead
owns the dimensions; the triage specialists supply the facts.

| Dimension | The question | Evidence that answers it |
|---|---|---|
| **Blast radius** | Where does the changed code run, who depends on it, and what is the worst case if it is wrong? | placement (sites, replicas, node pools), traffic and callers per site, rollout staging (per-site canary or global), feature flags, pending impacts from other open PRs |
| **Correctness** | Does the change do what the PR says, and will it work where it runs? | the diff against the repo's invariants and PR norms; contract compatibility with callers; config keys and credentials present in the pod spec at each site; IAM and resource existence for cloud access; tests that cover the new path |
| **Debuggability** | When it fails, can an operator tell, and where? | logs/metrics/traces on the new path; error wrapping and status codes; whether the new counter has a dashboard or alert; a flag to turn it off; a runbook entry |
| **Resilience** | Does it fail small and recover? | deadlines and their propagation; retry sanity (idempotency); bounded work; backpressure and overload behaviour; canary with analysis; PDB and spread; rollback without data migration |

"No impact on this dimension, and here is why" is a finding, not an
absence.

## 4. The journey

Service owner opens a PR. Within a minute a check appears: *impact:
assessing*. A few minutes later either the report lands as a check result
with a comment, or a question lands as a comment first:

> **Question (1 of 1).** `greeter` appears in no fleet file and no cluster
> reports a container named `greeter`. Is `greeter` deployed anywhere today?
> Reply **yes** with where, or **no**.

The owner, or whoever CODEOWNERS names for the topic, replies. The check
resumes and the report lands. The answer is remembered for every later PR,
as a rule when the answer describes one ("it runs wherever the checkout cell
runs").

Surfaces, in order of delivery: the CLI (exists; questions are answered at
the prompt), the GitHub check + comments (M5), Slack (`/impact owner/repo#N`
and a thread for questions; after M5). One engine behind all three.

Who answers what: placement and platform questions go to the platform
owners; config, credentials and intent questions go to the PR author. In
the CLI both are the operator. In GitHub, CODEOWNERS for `deploy/` and the
fleet repo vs. CODEOWNERS for the changed files.

## 5. Pipeline

```
 PR ──▶ intake ──▶ hint ──▶ TRIAGE ──────────────────────▶ ANALYSIS ──▶ report ──▶ learn
         load      rules    what is this change?            four dimensions   check +   memory
         PR,       (cheap   where does it run?              lead reasons,     comment   updates
         repo      aspect   what does it touch?             specialists
         memory    hints)   what is unknown? → ask          verify
```

1. **Intake.** Load the PR (files, patches, both sides of configs), the
   repo brief from memory (or run *learn* first if the repo is new), and
   the environment graph entries for the entities the repo brief names.
2. **Hint.** The deterministic rules from milestone 1, demoted to hints:
   "touches a gRPC client", "touches a Dockerfile", "has a Terraform plan".
   They tell the lead which deep checks to run; they no longer route.
3. **Triage.** The lead reads the diff with the repo brief in hand and
   decides what it needs to know, then delegates to the four triage
   capabilities (section 6). Unknowns become questions. The session
   pauses on questions and resumes on answers.
4. **Analysis.** With the facts in hand, the lead reasons through the four
   dimensions. Where a dimension needs more evidence (a metric, a cloud
   lookup), it goes back to a specialist. Parallel across dimensions when
   the facts are independent.
5. **Report.** One JSON document (section 11.4), rendered to the surface.
6. **Learn.** Measured facts and confirmed answers go to memory; the
   assessment is stored; the PR's claimed impact is recorded as *pending*
   so other open PRs see it.

## 6. Triage capabilities

Four, as named in review. The first three are specialists (their own
agents, with their own tools); the fourth is a capability of the lead.

### 6.1 GitHub org finder

Finds the repository that defines something — the callee of a new client,
the chart that deploys the mesh, the module a Dockerfile builds from, the
fleet inventory — and makes it readable. Searches org-wide code (names it
read in manifests: a chart, a CRD kind, a DaemonSet, an import path), ranks
by where hits cluster, mounts the repository into the session, reads it. A
search that finds nothing is an unknown: *"Where is SPIRE's deployment
defined?"*, whose answer is a rule: *"platform components live in
`infra/platform-<component>`"*.

### 6.2 Deployment topology discoverer

Answers "where does this run" for a package, binary, image or workload, in
this order, stopping when sources agree:

```
memory (rules resolved fresh; sets re-verified)
 → build graph        go list -deps from each cmd/: which binaries include the change
 → Dockerfile         COPY/ENTRYPOINT: which binary → which image; what else ships in it
 → CI publish         the image name and digest it pushes
 → pod template       containers[].image, containers[].name (telemetry's join key),
                      serviceAccountName, env/envFrom, volumes, nodeSelector,
                      affinity, tolerations, topologySpreadConstraints
 → overlays/selectors kustomize envs, Helm values per cluster, ApplicationSet
                      generators and label selectors, fleet inventory files
 → telemetry          kube_pod_info{container=…} by cluster; up{job=…}; the mesh's
                      request metrics by destination
 → ask                "Does pricing run in every cluster labeled tier=edge? (yes/no)"
```

The first four are deterministic reads and rarely need a human. Companies
diverge at the overlay/selector level; that is where rules live. Output: a
placement record (sites, replicas, node pools, identity, mounted config)
with one source per field, and the caller/callee edges telemetry shows.

### 6.3 Cloud resource discoverer

Given a resource reference found in code or config (a bucket name from an
env var, a topic, a Cloud SQL instance, a secret path): which project, does
it exist, what IAM it carries, what quota it consumes, whether it holds
data and is protected. Uses a read-only identity per project
(`roles/viewer` + `roles/iam.securityReviewer`, impersonated). Paired with
the topology discoverer for the runtime half: which KSA the pod runs as,
whether it maps to a GSA (Workload Identity annotation) or to a mounted
key, and whether that identity holds the role the code needs. No access to
the project is an unknown: *"I cannot read IAM in project `acme-data`.
Does `ledger@acme-prod.iam` have `roles/storage.objectCreator` on
`gs://ledger-exports`? (yes/no)"*.

### 6.4 Yes/no questioner

A capability of the lead, not of specialists, so questions are batched,
deduplicated and routed. Specialists return `unknown` with what they tried;
the lead turns each into a question that can be answered **yes** or **no**
(with a short "where/which" when the answer is no), because confirmable
questions get answered and are parseable. The lead parses the answer into a
set or a rule with a resolver, confirms a rule's resolution once ("that
resolves to 7 clusters today: …; is that the set?"), and stores it with the
human as source. Questions have an owner (section 4), a deadline (24h, after
which the report ships with the unknowns as warnings), and a log.

A fifth participant, the **code reader**, is not triage: it is the lead
itself reading the diff and the surrounding code — what a client
constructor authenticates with, which config key a new call reads, which
resource name a change introduces — and the hint rules that pre-chew it.

## 7. Memory

Three stores, different lifetimes, one directory per org. Append-only
files locally (a table later). One process writes; the bridge serializes.

### 7.1 Repository memory

What the agent learned by **reading the repo deeply once**, refreshed
incrementally on every merged PR.

- **Brief** (`repos/<repo>/brief.md`, markdown the agent writes for itself,
  human-editable): structure (where `cmd/`, `deploy/`, manifests, tests
  live, or that manifests live in another repo), build and image, how it is
  deployed and by what, the services it defines and calls, and pointers to
  everything author-stated: `CLAUDE.md`, `AGENTS.md`, `.cursorrules`,
  `CONTRIBUTING`, ADRs, runbooks. Author-stated documents are the
  highest-trust source of invariants and are quoted, not paraphrased.
- **Invariants** (`repos/<repo>/invariants.jsonl`): conventions the repo
  keeps, each with evidence and confidence: "every RPC handler increments
  `vikrant_rpcs_total`" (12/12 handlers), "config is read via `envFrom` a
  ConfigMap named after the service", "every outbound call sets a
  deadline". Correctness and debuggability check the diff against them.
- **PR norms** (`repos/<repo>/norms.json`): from the last N merged PRs —
  typical size, files that change together (`service.yaml` with
  `e2e/run.sh`), description template, labels, required checks — and
  **review norms**: what reviewers recur on in comments ("missing
  deadline", "no test for the error path"). A PR that breaks a norm is
  flagged with the norm's evidence; a repo with no norms gets none.

Populated by `learn --repo`, which reads: tree, build graph, Dockerfile,
manifests, CI, docs, and the last 30 merged PRs (titles, files, review
comments). Cost-capped (section 12).

### 7.2 Environment graph

Entities and relations with provenance and validity. The entity set is
open: service, binary, image, workload, site, node pool, identity (KSA,
GSA), config object, cloud resource, platform component.

```json
{"entity":"pricing","relation":"placed-in","kind":"rule",
 "rule":"clusters with label tier=edge","resolver":"fleet-inventory",
 "source":{"kind":"human","by":"@alice","at":"2026-10-01","via":"hello#12"},
 "confirmed":{"at":"2026-10-01","resolved_to":["us-east1","eu-west1","ap-south1"]},
 "last_resolved":{"at":"2026-10-01","to":["us-east1","eu-west1","ap-south1"],"agrees_with_telemetry":true}}

{"entity":"pricing","relation":"calls","object":"tax","kind":"set",
 "source":{"kind":"metric","query":"sum by (destination) (rate(istio_requests_total{source=\"pricing\"}[1h]))","at":"2026-10-01"},
 "valid":{"ttl_hours":24}}

{"entity":"pricing","relation":"depends-on","object":"spire","kind":"set",
 "source":{"kind":"repo","ref":"infra/platform-mesh@3fa1c2","path":"charts/spire/values.yaml"},
 "valid":{"until_change_of":"infra/platform-mesh"}}
```

Rules of the store:

- **Rules resolve fresh every run**; the resolved set is recorded, never
  trusted as the rule.
- **Measured facts** expire by TTL or when the entity's deployed revision
  changes (the fleet commit or image digest tells us).
- **Human facts** do not expire; they are re-asked when a measured source
  contradicts them, or when a rule resolves to something surprising
  (empty, or changed more than the fleet usually changes between runs).
- **Repo-derived facts** are valid until the defining repo changes.
- **Pending impacts** (`pending.jsonl`): what open PRs would do to an
  entity ("frontend#9 would add ~40 rps to ledger"), dropped when the PR
  closes, turned into an expectation when it merges. Two PRs each fine
  alone and together over capacity is the case this exists for.

### 7.3 Assessments

`assessments/<repo>/<pr>-<sha>.json`: the report, immutable; a new push is
a new assessment. One assessment per PR at a time; the newest head cancels
the older run. Also `questions.jsonl`: every question asked, to whom, the
answer, and what it became.

### 7.4 Platform memory store

Managed Agents offers a per-agent memory store that the self-hosted worker
syncs to disk. Evaluate it in M2 for the repo brief (free-text, agent
written); keep the graph and assessments in our own format because they
need structure (rules, resolvers, validity).

## 8. Sample PRs, end to end (code only)

Fixtures live as open draft PRs in the demo org and double as the live
eval. Costs are estimates to be measured in M2; the cap is enforced.

### A. Business logic: `hello` rejects empty names

Diff: `internal/handler/hello.go` returns `InvalidArgument` when `name` is
empty. No manifest changes.

- Intake: repo brief for `hello` (learned); graph: `hello placed-in {dev,
  staging, prod}` (source: `fleet/*/services.yaml`, confirmed by
  `kube_pod_info` per context); `frontend calls hello` (metric).
- Hint: none ("handler code").
- Triage: topology confirms placement from memory (one `kube_get` per site
  to re-verify replicas); org finder mounts `frontend` because the graph
  says it calls `hello`; the lead reads `frontend`'s call site: does it ever
  send an empty name? (It can: it passes the query string through.)
- Analysis. **Blast radius:** 3 sites, 2 replicas each, 1 caller, ~5 rps;
  rollout has a canary with error-rate analysis in staging and prod, none
  in dev. **Correctness:** warning — `frontend` forwards user input
  unchecked, so empty names become `InvalidArgument` surfaced to users;
  the handler test covers the new branch (norm: tests change with
  handlers — met). **Debuggability:** info — the status code is counted by
  `vikrant_rpcs_total{code="InvalidArgument"}` (invariant); no alert on
  it, and none needed. **Resilience:** info — the canary's analysis
  counts `InvalidArgument` as a client error, so the canary would not
  roll back on it; say so.
- Questions: none. Memory written: assessment; `hello` replicas
  re-measured.
- Verdict: warning. Cost estimate: $0.8–1.5.

### B. New client: `frontend` calls `ledger`

Diff: `internal/web/handler.go` constructs a `ledger` gRPC client and
calls `GetBalance` per page view. `service.yaml` untouched.

- Hint: "new gRPC client (`ledgerv1.NewLedgerServiceClient`)".
- Triage: topology places both; telemetry gives `ledger` 12 rps and
  `frontend` 30 rps, 1 call per request → +30 rps (+250%). The lead reads
  the constructor: dials `ledger.ledger.svc:8080` plain, relies on the
  mesh for mTLS; no deadline on the call; config key none. Org finder
  mounts `ledger` to read its `service.yaml`: `frontend` not in
  `authorizedCallers`. The mesh's placement rule resolves to all 3 sites;
  `istio_requests_total` reports from all 3.
- Analysis. **Blast radius:** every page view now depends on `ledger`;
  pending impacts show none. **Correctness:** blocking — `frontend`'s
  `egress` lacks `ledger` (NetworkPolicy + Sidecar block the connection)
  and `ledger` does not authorize `frontend` (`PERMISSION_DENIED`); the
  platform's access-request flow is the fix; the e2e would fail. **Debug-
  gability:** warning — the call is instrumented by the platform's client
  interceptor (invariant) but the handler swallows the error into a 500
  without wrapping. **Resilience:** blocking — no deadline; `ledger` has
  a KEDA ceiling of 3 replicas at threshold 80 in-flight, +250% load
  exceeds it; no fallback when `ledger` is down, so a `ledger` outage is a
  `frontend` outage.
- Questions: none. Memory: `frontend calls ledger` pending (+30 rps);
  `ledger` capacity measured.
- Verdict: blocking. Cost estimate: $1.5–2.5.

### C. Cloud resource: `ledger` exports to GCS

Diff: `internal/export/gcs.go` writes daily exports to the bucket named by
`LEDGER_EXPORT_BUCKET`; `go.mod` adds `cloud.google.com/go/storage`.

- Hint: "cloud SDK import (`cloud.google.com/go/storage`)".
- Triage: the lead finds the env var; topology finds no `LEDGER_EXPORT_BUCKET`
  in the pod template or ConfigMap at any site, and `serviceAccountName:
  ledger` with no Workload Identity annotation and no mounted key. Cloud
  discoverer: the bucket name is not in the diff (it comes from config
  that doesn't exist) → unknown. One question: *"No site sets
  `LEDGER_EXPORT_BUCKET`. Is the export bucket `gs://ledger-exports` in
  project `acme-data`? (yes/no; if no, which?)"*. Answer: yes. Cloud
  discoverer: bucket exists, IAM has no binding for any `ledger` identity,
  retention 30 days, no deletion protection.
- Analysis. **Blast radius:** 3 sites; a scheduled path, not request
  path. **Correctness:** blocking — config absent at runtime; no
  credential path (no WI binding, no key); no IAM grant. **Debuggability:**
  warning — errors logged without the bucket or object name. **Resilience:**
  warning — no timeout on the upload; no retry; a 2 GB export streamed from
  memory (bounded work?).
- Memory: `ledger uses gs://ledger-exports` (human); `ledger identity ksa
  ledger → no GSA` (measured); question log.
- Verdict: blocking. Cost estimate: $1.5–2.5 plus the pause.

### D. Unknown placement: `greeter`

Diff: any handler change in `greeter`.

- Triage: no fleet entry, no overlay, no container named `greeter` in any
  context, no metrics. Question: *"Is `greeter` deployed anywhere today?
  (yes/no; if yes, where)"*. Answer: no.
- Analysis: blast radius zero; correctness reviewed against invariants
  only; the rest "not deployed".
- Memory: `greeter placed-in ∅` (human, re-asked if a container appears).
- Verdict: info. Cost: $0.3–0.6. Second run: zero questions, under $0.3.

### E. Refactor: rename a private function in `echo`

- Hint: none. The lead reads the diff with the brief: behaviour-preserving,
  tests unchanged and passing in CI. Topology from memory, no re-measure.
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
| `gh_search_code` (org scope) | host | GitHub read | org finder |
| `gh_repo_mount` (clone at a ref into the session) | host | GitHub read | org finder |
| `gh_pr_history` (merged PRs, files, review comments) | host | GitHub read | learn |
| `fleet_inventory` (contexts/clusters with labels) | host | kubeconfigs | topology |
| `kube_get` (get/list, allow-listed kinds, any site) | host | read-only RBAC | topology |
| `prom_query` (with a site label) | host | metrics read | topology, lead |
| `logs_query` | host | logging read | lead (M4) |
| `gcp_resource` (describe), `gcp_iam_policy`, `gcp_quota`, `gcp_sa_bindings` | host | impersonated viewer SA | cloud |
| `memory_get`, `memory_put` (measured facts only), `memory_resolve` (a rule) | host | none | all |
| `ask` | host | none | lead only |

`bash` is off in M2: the sandbox has no network and no credentials, but a
general shell on the operator's laptop is not a boundary I want to rely on.
`repo_cmd` covers what reading needs. Bash returns when the worker runs in
a container (section 10).

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
 │  credentials: Keychain (GitHub),         │           each runner owns
 │  ADC + SA impersonation (GCP),           │           its tool names)
 │  kubeconfigs (read-only contexts),       │
 │  PROMETHEUS_URL                          │
 │  memory: ~/.paved-road/memory/<org>/     │
 └──────────────────────────────────────────┘
        │ read-only                │ read-only
        ▼                          ▼
   kind cluster(s) sgrpc       GCP project (bucket, SA, IAM; GKE Autopilot
   Prometheus (port-forward)   only while testing Workload Identity)
```

- **Environment**: one self-hosted environment per org, created by
  `setup`; its environment key lives in the Keychain. The worker claims
  session work items, clones the PR's repository into the session workdir
  at the head SHA, serves the sandbox tools, heartbeats. The bridge serves
  the host tools for the same session. The SDK's runner leaves tool names
  it does not own pending for their owner, so the two co-serve.
- **Default org**: the user's GitHub org, read through a fine-grained token
  with contents, metadata, pull requests (read), and, for the check surface
  in M5, pull requests (write). Code search uses the same token.
- **GCP**: a reviewer service account with `roles/viewer` and
  `roles/iam.securityReviewer` on the project; the bridge impersonates it
  via ADC; no key file.
- **Clusters**: kind for everything placement-related (free). A GKE
  Autopilot cluster only for Workload Identity tests (fixture C), created
  and deleted in the same session.
- **Boundaries**: the model holds nothing. The sandbox has the repo and no
  network. The bridge has read-only credentials. Every session's events
  are scanned for credential patterns in tests.

## 11. Implementation design

### 11.1 Packages

```
cmd/change-agent      setup | learn | review | worker | bridge | answer
internal/change       the change (exists)
internal/hint         the milestone-1 rules, as hints (rename of classify)
internal/findings     report contract: dimensions, findings, unknowns, questions
internal/memory       brief, invariants, norms, graph (rules + resolvers), pending, assessments, questions
internal/resolve      rule resolvers: fleet labels, ApplicationSet generators, telemetry
internal/tools        host tools by group: github, topology, cloud/gcp, telemetry, memory, ask
internal/sandbox      the worker: workdir, clone, repo_cmd, agenttoolset subset
internal/agents       lead, org-finder, topology, cloud (definitions as code)
internal/session      create, run both runners, pause on questions, resume, collect
internal/github       PRs → changes (exists)
evals/                fixtures (PR refs + expectations), replay transcripts, judge rubric
```

### 11.2 Session lifecycle

1. `review` creates the session: lead agent version, environment, budget,
   repo resource (the worker clones it), initial message = the brief
   (change, repo brief excerpt, graph entries, hints, roster).
2. The worker and the bridge attach; tool calls flow; the lead delegates.
3. **Pause**: the lead ends its turn with a `questions` block in its
   message. The bridge detects the idle with questions, posts them to the
   surface, records them, and exits the runner. The session stays idle;
   the budget is not consumed while idle.
4. **Resume**: `answer --session <id> "yes, …"` (CLI) or a comment reply
   (M5) sends a `user.message` with the answer; the runners re-attach; the
   lead parses the answer, writes memory via the bridge, continues.
5. **Collect**: on idle with a `report` block, parse, validate (every
   finding has evidence; every dimension has a verdict), store, render.
6. **Concurrency**: a per-PR lock; a new head SHA archives the running
   session and starts over; memory writes append; the bridge is the only
   writer.

### 11.3 Learn

`learn --repo org/repo [--prs 30]`: a session with the lead and the org
finder only, no environment tools. Reads the tree, build graph, Dockerfile,
manifests, CI, docs, and PR history; writes the brief, invariants with
evidence counts, norms. `learn --env`: no model; `fleet_inventory` plus
`kube_get` per context plus a few `prom_query`s populate the graph with
measured placements for every workload found. `learn --org`: no model; the
repo list with topics, languages, and whether manifests are present, so
the org finder has an index.

### 11.4 Report contract

```json
{"change":"org/repo#N@sha",
 "dimensions":{
   "blast_radius":{"verdict":"warning","summary":"…"},
   "correctness":{"verdict":"blocking","summary":"…"},
   "debuggability":{"verdict":"info","summary":"…"},
   "resilience":{"verdict":"blocking","summary":"…"}},
 "findings":[{"dimension":"correctness","severity":"blocking","claim":"…",
   "evidence":[{"kind":"config","source":"kube_get networkpolicy frontend/allow-egress@prod","value":"…"}],
   "recommendation":"…","confidence":0.85,"studied":["frontend","ledger"]}],
 "unknowns":[{"what":"…","tried":["…"],"asked":true}],
 "questions":[{"id":"q1","to":"platform-owners","text":"…","answer":null}],
 "memory_updates":[{"entity":"…","relation":"…","…":"…"}],
 "verdict":"blocking","summary":"…"}
```

The lead proposes `memory_updates` for human-derived facts; the bridge
applies them, so every memory write is attributable to a session and a
question. Measured facts are written directly by specialists through
`memory_put`, with their query as source.

### 11.5 Agents

| Agent | Model | Tools | Owns |
|---|---|---|---|
| `impact-lead` | claude-opus-5, effort high | sandbox read tools, `change_diff`, `memory_*`, `ask`; roster below | triage decisions, code reading, the four dimensions, questions, the report |
| `org-finder` | claude-sonnet-5 | `gh_search_code`, `gh_repo_mount`, sandbox read tools, `memory_*` | finding and mounting the defining repository |
| `topology` | claude-sonnet-5 | sandbox read tools, `repo_cmd`, `fleet_inventory`, `kube_get`, `prom_query`, `memory_*` | placement, identity, config presence, callers, capacity |
| `cloud` | claude-sonnet-5 | `gcp_*`, `memory_*` | resource existence, IAM, quota, protection |

Analysis stays with the lead in M2 (one Opus context holds all the facts;
fewer hand-offs). If reports get long or slow, split the four dimensions
into Sonnet analysts fed by the lead's fact sheet; the contract does not
change.

### 11.6 Configuration

`~/.paved-road/config.yaml`: org, GitHub token Keychain item, GCP project
and reviewer SA, kube contexts with site labels, Prometheus URL, budgets
(per review, per learn, monthly), question deadline, surfaces.

## 12. Costs

Model usage is the only material cost; the laptop and kind are free; GCP
is cents outside the Autopilot hours.

| Operation | Tokens (est.) | Cost (est., at Opus $5/$25 and Sonnet $3/$15 per M in/out; verify against the price list) | Cap |
|---|---|---|---|
| Review, typical (A, B) | lead ~300k cached + 40k fresh in, 10k out; 2–3 specialists ~150k in, 6k out each | $1–2.5 | $3 |
| Review, cheap path (D, E) | lead only, memory hits | < $0.3 | $1 |
| Learn a repo | Sonnet-heavy, ~1M in, 25k out | $3–6 | $10 |
| Learn env / org | no model | $0 | — |
| Fixture eval run (5 PRs) | | $5–10 | $15 |
| GKE Autopilot for fixture C | ~2 pods for 2 h | < $1 | delete after |

Levers, in order: memory hits (the second run of a repo must be cheaper —
tested), Sonnet for everything that reads, Opus only where it reasons,
prompt caching of the brief and graph excerpts, hard fan-out limit (one
task per unknown, at most 4 specialists per review), the idle session
costs nothing while waiting on a human, and the platform budget as the
backstop. Monthly development target: under $50; `review` prints the
session's cost on exit and the bridge keeps a running monthly total in
memory and refuses past the cap.

## 13. Test plan

| Layer | What | How | Pass |
|---|---|---|---|
| Unit | hint rules; report validation; memory: rule resolution, TTL and deploy invalidation, pending lifecycle; answer application (set vs rule); tool input guards | `go test` | green |
| Replay | host tools against recorded sessions: the same tool inputs produce the same outputs, no credential leaks in any event | transcripts under `evals/replay`, no model | deterministic |
| Live fixtures | the five PRs of section 8 as open drafts in the demo org | `make eval-live` (budgeted) | per fixture: required findings present (by dimension, entity, severity), no forbidden claims, cost ≤ cap, questions == expected |
| Judge | claim quality against a rubric (evidence matches claim; recommendation actionable; no unsupported numbers) | Sonnet judge over the report JSON, scores stored | ≥ baseline; drift flagged |
| Memory | run A twice; run D, answer, run D again | counts of tool calls, questions, cost | second run: 0 questions, ≥ 40% cheaper, graph entries reused |
| Question flow | fixture D end to end on the CLI, then (M5) on a PR comment | scripted answer | pause, resume, fact stored with human source |
| Safety | every tool refuses non-read verbs; sandbox confined to workdir; event scan for `github_pat_`, `ya29.`, bearer tokens | unit + scan in eval-live | zero hits |
| Cost | per-fixture ceilings; monthly cap in the bridge | eval-live | CI fails on breach |

Golden reports are kept per fixture; a change to a prompt re-runs the
fixtures and diffs the judge scores before it is merged.

## 14. Milestones

- **M2 — learn and place.** memory (7.1–7.3), `learn --repo/--env/--org`,
  topology discoverer, questioner on the CLI, self-hosted worker + bridge,
  fixtures A, D, E. Exit: A reviewed on the sgrpc fleet with the four
  dimensions; D asks exactly one question; E stays under $0.3; second runs
  reuse memory.
- **M3 — cross-repo.** org finder, pending impacts, fixture B. Exit: B
  blocks for the right two reasons with metric and policy evidence.
- **M4 — cloud.** GCP discoverer, `logs_query`, fixture C with a
  short-lived Autopilot cluster. Exit: C finds the missing config, the
  missing identity, and the missing IAM grant, with one question.
- **M5 — surfaces.** GitHub check + comments with question/answer by
  reply; CODEOWNERS routing. Then Slack.

## 15. Open for review

1. Analysis in the lead (one Opus context) vs. four Sonnet analysts — I
   propose the lead for M2 and to measure.
2. Bash off until the worker runs in a container — agree?
3. Question deadline and fallback (ship with warnings after 24h) — agree?
4. Repo brief in the platform memory store vs. our files — evaluate in M2,
   decide after.
5. The demo org as the default org, with fixtures as open draft PRs — fine
   for the eval; is it also where you want `learn --org` to run first?
6. Anything in section 8 that does not match how you'd expect the reviewer
   to think.
