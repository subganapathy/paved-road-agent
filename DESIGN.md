# paved-road-agent: design

Status: revision 5, 2026-10-05. [VISION.md](VISION.md) holds the why;
[subbu-thoughts.md](subbu-thoughts.md) holds the thesis this revision
adopts.

Changes in this revision: the agent's program is stated as **obligations**
— what must be established for each kind of change, what evidence
establishes it, and what it costs when it cannot be — rather than as
prose instructions (§5.1); the minimal library is three connectors
(metrics, source control, logs), with cloud deferred and alerts folded
into metrics (§9); data protection, deployment hardening and infra apply
semantics join the obligations; and a section on how the agent improves
(§15). Revision 4 added the vocabulary, prose-first diagrams, join keys
and answers, the capability probe, environments, retry amplification and
the hosted path.

## 1. The model in one paragraph

An agent that assesses every pull request on seven qualities — **blast
radius, correctness, scale, resilience, debuggability, test coverage** —
by discovering, on every run, where the changed code runs and what it
touches. The only assumption about the environment is Kubernetes. It
reviews; it never executes. It reaches the environment only through a
**proxy** that exposes a closed set of read-only **connectors** (metrics,
logs, alerts, source control, cloud) and holds every credential; the
agent holds none and has no network. The only thing it remembers is a
small set of **identifiers** in the repository's `.paved-agent/`
directory: the join keys between the code and the environment, which it
derives, proposes, and follows. When it cannot derive them, it asks a
yes/no **question** on the PR, says what it cannot verify without the
answer, and blocks the check until the answer lands.

## 1.1 Vocabulary

Terms as they are used in the rest of this document.

| Term | Meaning |
|---|---|
| **Managed Agents** | Anthropic's hosted agent runtime. It runs the models, their turns, delegation between agents, budgets, and keeps the trace. It does not run our tools; it asks us to. |
| **Agent** | A versioned configuration on Managed Agents: a model, a system prompt, a list of tools. We define five. |
| **Lead** | The agent that owns a review: reads the diff, decides what must be discovered, delegates to specialists, judges the seven qualities, writes the report. Runs on the strongest model. |
| **Specialist** | An agent the lead delegates one task to and that returns facts: the *org finder*, the *topology discoverer*, the *cloud discoverer*. Run on a smaller model. |
| **Session** | One review: the lead's conversation plus its specialists' sub-conversations, with a dollar budget. |
| **Tool call** | An agent asking for something to be done outside the model: read a file, run a query. Managed Agents emits it as an event on the session's stream and waits for a result. |
| **Sandbox** | The pod where tool calls are executed: it holds the checked-out repositories and nothing else. |
| **Worker** | The process in the sandbox that watches the session's event stream, executes each tool call, and posts the result. |
| **Proxy** | The service that holds every credential and talks to the real systems. Every connector call from the sandbox goes through it. |
| **Connector (slot)** | One of five typed, read-only interfaces the proxy exposes: `metrics`, `logs`, `alerts`, `scm` (source control), `cloud`. Each slot is *bound* at install time to a concrete backend. |
| **Profile** | Code in the proxy that knows how to turn identifiers into queries for a particular metrics stack (e.g. kube-state-metrics + Istio). |
| **Identifiers** | The small file `.paved-agent/discover.yaml` in the repository: namespace, labels and container name that identify this service's pods anywhere, plus answers to past questions. |
| **Discovery** | Following the identifiers through the connectors to learn what runs where, how much traffic it carries, who calls it, and what it depends on. Deterministic. |
| **Run memory** | What discovery produced in this session. Kept for the run, summarized in the report, then gone. |
| **Quality / dimension** | One of the seven properties every safe change has. The program defines them; the report gives each a verdict. |
| **Finding** | One claim with evidence, under one dimension, with a severity: `info`, `warning`, `blocking`. |
| **Question** | What the lead asks when it cannot derive an identifier: yes/no, with the file change each answer implies. |
| **Proposal** | A change to `.paved-agent/` the agent wants made, applied by the proxy as a commit or a suggested change. |
| **Check** | The GitHub status on the PR (`impact`) that carries the verdict and gates merge. |
| **Fixture** | A sample PR with a known expected outcome, used as a live test. |
| **Obligation** | What a quality means for one specific change: derived by the lead at the start of a review from the quality's definition, with the evidence that establishes it and the severity when it cannot be. |

## 2. Goals, non-goals, assumptions

Goals

- Any production modification that arrives as a PR: code, Kubernetes
  manifests, cloud infrastructure (Terraform, Pulumi, Crossplane), dynamic
  configuration — including pure code changes with no manifest in the diff.
- The agent's program is a reviewable specification — seven qualities
  with their evidence rules, instantiated per change — not prose. A stronger model runs the same program
  better; a human can read and amend it.
- Findings with evidence: a metric, a file and line, a cloud API response,
  or a human's answer. Never an unsupported claim.
- Stateless: nothing persists outside the repository under review.
- Lowest privilege: read-only connectors, bound at deployment inside the
  customer's boundary, short-lived credentials via workload identity
  federation, one write path (the PR under review).
- Bounded cost per review, enforced by the platform.
- A design that becomes a hosted, multi-tenant service without rework
  (§10.3–10.4), even though this project runs on one laptop for one org.

Scope of this project: single tenant, self-hosted on kind, deployed and
reviewing the fixtures end to end, with the Managed Agents / SDK split
understood well enough to write it up publicly. The multi-tenant sandbox
orchestration in §10.3–10.4 is recorded here as the design for the next
project, not built in this one.

Non-goals (for now)

- Executing anything in the environment: no deploys, no applies, no
  API-server access at all.
- Non-Kubernetes runtimes (VMs, serverless) as placement targets.

Assumptions

- Workloads run on Kubernetes. Which metrics stack describes them is
  **detected, not assumed**: at start the proxy probes the metrics
  backend for the metric families it knows (section 7.4) and selects or
  rejects a profile accordingly.
- Source control is the unit of knowledge: the repository under review,
  and other repositories in the org read through the `scm` connector.
- Cloud providers are parameters, not assumptions: `cloud_get("gcp", …)`.

## 3. The seven qualities (the report's dimensions)

Every report has a verdict per quality and findings under it. The lead
instantiates and judges the qualities (§5.1); the specialists supply the
facts.

| Quality | The question | Evidence that answers it |
|---|---|---|
| **Correctness** | Does the change do what the PR says, and does the system around it let it? | the diff against the contract it relies on (signature, field semantics, validation, documented decisions, `buf breaking` against the base); a new dependency's onboarding (network policy, authorization, mesh membership); whether authentication, authorization and encryption in transit match the sensitivity of the data a new call carries; configuration and credentials present where the code runs; IAM and resource existence for cloud access |
| **Compatibility** | Do old callers, old data and old callees still work against this, during rollout and after rollback? | wire compatibility of the contract; handling of absent or extra fields; data written by the old version read by the new and vice versa; mixed-version behaviour during a staged rollout |
| **Blast radius** | What is the worst case if this is wrong, and does the rollout make it small? | instances per cluster and environment; callers and external exposure; what fails and whether it is contained; staged rollout with automatic analysis, per cluster; rollback without data migration; other open PRs on the same services |
| **Scalability** | Can the system carry the load this adds or redirects? | added rate against current rate, in-flight work and p99; CPU and memory against limits; desired replicas and the autoscaler ceiling; internal versus external callers |
| **Resilience** | Do service objectives hold when something fails? | deadlines and their propagation; retry budgets and amplification; bounded work; backpressure and load shedding; per-caller limits; what the caller sees when the callee is gone; disruption budgets and spread; hardening the change makes matter (probes, security standards, capabilities, limits); for infrastructure, how the provider applies the change and what a partial apply leaves |
| **Debuggability** | If it does not have the intended effect, how would anyone know, and find out why? | signals on the new path (logs, metrics, traces); error wrapping and status codes; an alert on the new signal; a flag to turn it off; a runbook entry |
| **Testability** | Is the behaviour, including its failure cases, proven and described? | unit tests for the new branches; end-to-end coverage of the call or resource; documented behaviour under failure |

"No impact on this quality, and here is why" is a finding, not an
absence. Blast radius sets the weight of the others: a change whose worst
case is trivial needs less proof, not none.

## 4. The journey

### 4.1 What the service owner experiences

1. They open a PR. A required check named `impact` appears, in progress.
2. A few minutes later, one of two things happens:
   - **The report lands.** The check completes with a verdict and a
     comment summarizes the seven qualities. `info` passes, `warning`
     passes with a visible note, `blocking` fails the check.
   - **A question lands.** The agent could not work out something it
     needs — most often, where the service runs. The check turns to
     *action required*, which blocks merge, and a comment states the
     question, what the agent cannot verify without the answer, and what
     each answer means.
3. Anyone with write access to the PR replies on the comment thread in
   plain words. The agent reads the reply, writes the matching line into
   `.paved-agent/discover.yaml`, and either pushes that commit to the PR
   branch (if the repository allows bot commits) or attaches it as a
   GitHub *suggested change* the author applies with one click.
4. The push re-runs the check from the start, now with the answer in the
   file. The question does not come back on later PRs: the file is in
   the repository.

Often step 2 produces neither: the agent derives the identifiers from the
repository itself, commits or suggests them, and proceeds to the report in
the same run. Questions are for the cases it cannot derive.

An unanswered question is a blocked PR. There is no timeout that lets it
through. There is one way around it: a **second person** — anyone with
write access other than the PR author — can resolve the question's
thread without answering it. That waives the question: the check re-runs,
the unknown stays in the report as a warning marked *waived by @who*, and
the gate lifts. The author resolving their own question does nothing;
the check stays blocked and says so. Questions are therefore posted as
review-comment threads (which GitHub can resolve, and records who
resolved), not plain issue comments.

### 4.2 The check's states

| State shown on the PR | What is happening | What moves it on |
|---|---|---|
| `impact — queued` | The controller received the PR event and is creating the session. | Session created. |
| `impact — discovering` | Specialists are following the identifiers (or deriving them). | Discovery done, or an identifier could not be derived. |
| `impact — assessing` | The lead is judging the seven qualities. | Report written. |
| `impact — action required: pending human response` | A question is open. Merge is blocked. | A reply on the thread, or a commit to `.paved-agent/` on the PR branch, which re-queues; or someone other than the author resolves the thread, which waives it and re-queues. |
| `impact — passed` / `passed with warnings` / `failed` | The report is attached and summarized. | A new push re-queues. |

### 4.3 Surfaces

In order of delivery: the CLI (exists; the operator edits the file and
re-runs), the GitHub check with comment threads and suggested changes
(M5), a Slack notification that links back to the PR (after M5). One
engine behind all of them.

## 5. Pipeline

A review is six steps. The first and last are deterministic code; the
middle four involve the agents.

1. **Intake.** The controller loads the PR (files, patches, both sides of
   configuration files) and this repository's `.paved-agent/`. Nothing
   else is loaded from outside the repository.
2. **Bootstrap.** If the identifiers the change needs are present, go on.
   If not, the topology and cloud discoverers try to **derive** them from
   the repository (section 6.2). Derived with confidence → proposed, and
   the run continues with the derived values. Not derivable → a question,
   and the check gates.
3. **Discovery.** The specialists follow the identifiers through the
   proxy using the profile's standard queries. This is deterministic and
   cheap: a handful of calls. The results — ready instances per cluster,
   the workload kind, desired replicas and the autoscaler ceiling, rps,
   callers, in-flight work, p99, CPU and memory against limits, exposure
   outside the cluster, identities, mounted configuration, cloud resource
   state, firing alerts — are the run memory. An identifier that returns
   nothing anywhere is treated as broken: derivation restarts, and the
   result is a proposal or a question.
4. **Analysis.** The lead reasons through the seven qualities with the
   diff, the contract, the author-stated documents the repository
   references, and the run memory. Where a dimension needs more evidence,
   it goes back to a specialist.
5. **Report.** One JSON document (section 11.4), rendered to the check,
   the comment, and stdout.
6. **Propose.** Any identifier the run derived or corrected becomes a
   change to `.paved-agent/` on the PR branch — a commit where the
   repository allows it, a suggested change otherwise.

```
 PR ──▶ intake ──▶ bootstrap ──▶ discovery ──▶ analysis ──▶ report ──▶ propose
        (code)    (derive or    (specialists, (lead,       (code:      (code:
                   ask)          determin-     seven        check,      commit or
                                 istic)        dimensions)  comment)    suggestion)
```

There is no rule engine in front of the model. What the lead must
establish for each kind of change is its program, described next. The
milestone-1 rules stay in the tree only until M2 measures whether a
deterministic pre-pass saves enough tokens to keep.

### 5.1 The program: qualities, instantiated per change

The thesis (from `subbu-thoughts.md`): the system prompt and the tools,
together with what the model already knows, are a programming language
for the domain. The domain here is *safe modification of production*. The
assumptions — Kubernetes, and every change through source control —
shrink the domain enough that the model's training covers the rest: the
handful of ways a pod reaches a cloud, the few service meshes and their
metrics, the few policy engines, the three infrastructure tools. We do
not teach the model those. We tell it what a safe change *is*.

So the lead's system prompt is not a narrative of steps, and it is not a
catalogue of kinds of change either — kinds are open-ended, and a
catalogue indexed by them is never finished. It is a short list of
**qualities** every change must have. For each quality: a definition
that does not depend on the change, guidance for **instantiating** it —
deriving what the quality means for *this* diff — the **classes of
evidence** that establish it, and the rules for **severity** and
**weight**. The model derives the obligations; a stronger model derives
them better against the same list; a human reads the list and amends it
with a PR.

The seven qualities are the report's seven dimensions (§3): correctness,
compatibility, blast radius, scalability, resilience, debuggability,
testability.

The lead's first act in every review is to write the instantiation —
"for this change, correctness means …; the worst case is …, so rollout
matters this much; compatibility means …" — and that plan is part of the
report. It is reviewable, and it is the first thing the evals check:
did the agent ask the right questions of this change?

The program is a file (`internal/agents/qualities.yaml`) rendered into
the prompt, so it is reviewed, versioned and evaluated like code. Two
entries, to show the shape:

```yaml
quality: correctness
  definition: the change does what the PR says, and the system around it lets it
  instantiate: |
    Ask what the change *relies on* that it did not write. A call to an API relies on the
    signature (the request is built and the response parsed as the callee defines them at its
    deployed revision) and on the infrastructure allowing the call (reachability, authorization,
    identity). A change inside a handler relies on the enclosing API's contract still holding
    (field meanings, validation, error semantics, documented decisions). A change that reads
    configuration relies on the configuration existing where the code runs. Each reliance is
    one obligation.
  evidence:
    code:     the call site, the handler, the tests           (sandbox)
    intent:   the callee's contract, policies, configuration   (scm)
    actual:   who is reachable and authorized today           (metrics)
  severity: a reliance that is false where the code runs is blocking; one that is undocumented
            but true is a warning
  weight: always full

quality: blast-radius
  definition: the worst case if the change is wrong is small, or the way it is rolled out makes it small
  instantiate: |
    Establish the worst case first: where the code runs (instances, environments), who depends on
    it (callers, external exposure), what fails if it is wrong (a request path, a scheduled job, a
    whole service), and whether the failure is contained (a flag, a subset of traffic) or total.
    If the worst case is low, say so and stop. If it is high, establish how the change reaches
    production: a staged rollout that automatically judges error rate and latency before
    proceeding, per cluster rather than everywhere at once, with a rollback that needs no data
    migration. A high worst case with no staged rollout is the finding; the absence of a
    particular tool is never the finding.
  evidence:
    actual:   instances, callers, exposure, traffic per cluster (metrics)
    intent:   the rollout strategy, analysis, flags           (scm)
  severity: high worst case without a staged, analysed rollout is blocking; with one, a warning
            that names the case; low worst case is info
  weight: sets the weight of every other quality — a change with a trivial worst case needs
          less proof of resilience and scalability, not none
```

The other five follow the same shape. In one line each, what they
instantiate to:

- **compatibility** — old callers and old data against the new code, and
  the new code against old callees, during the rollout and after a
  rollback (mixed versions are the normal state, not the exception).
- **scalability** — load this change adds or redirects, against the
  capacity behind it: current rate, in-flight work, latency, CPU and
  memory per replica, desired replicas and the autoscaler's ceiling;
  internal callers (known rate) versus external (unbounded).
- **resilience** — service objectives continue to hold when something
  fails: deadlines, retry budgets and the amplification they can cause,
  bounded work, load shedding, per-caller limits, what the caller sees
  when the callee is gone; hardening the change makes matter.
- **debuggability** — if the change does not have its intended effect,
  how an operator would know and find out: signals on the new path,
  wrapped errors, an alert, a flag to turn it off, a note in the runbook.
- **testability** — the behaviour and its failure cases are proven:
  tests for the new branches, end-to-end coverage of the call or
  resource, and the failure behaviour written down.

Two rules the file enforces on itself. **No product names**: the program
says "staged rollout with automatic analysis", "service-to-service
authorization", "cluster state metrics", never the name of a tool; names
belong to profiles and connector adapters, and a test fails the build if
one appears in a rendered prompt. **Kinds of change are examples, not
structure**: the worked instantiations for a new call, a handler change,
a manifest change, an infrastructure change live in the evals as fixtures
and in the prompt as at most two short examples, to calibrate.

The specialists have the same shape in miniature: each has a system
prompt saying what facts it returns and from which connector, not how to
reason about them.

## 6. Specialists and the questioner

Three specialists do discovery. The questioner is a capability of the
lead. Analysis is also the lead's job (see Decided, item 5): the
specialists find facts; the lead judges.

### 6.1 Org finder

Finds, through the `scm` connector, the repository that holds something
the review needs — the callee of a new client, the chart that deploys the
mesh, the module a Dockerfile builds from — and mounts it read-only into
the sandbox. It searches org-wide code for names it read in manifests or
code (a chart name, a CRD kind, a DaemonSet name, an import path), ranks
repositories by where the hits cluster, mounts the best candidate, and
reads it to confirm. Reading other repositories is allowed; writing to
them or remembering anything about them is not. A search that finds
nothing is an unknown and becomes a question.

### 6.2 Topology discoverer

Two modes.

**Following** (every run). Given the identifiers — namespace, label
selector, container name, clusters — it runs the profile's standard
queries and returns a placement record per cluster and environment: ready
instances, workload kind (Rollout, Deployment, StatefulSet, DaemonSet),
desired replicas, autoscaler ceiling, image, rps, callers, p99, CPU and
memory against limits, whether the service is exposed outside the
cluster, drift between intent and actual, firing alerts. It also reads
the pod template in the GitOps intent for the things metrics cannot show:
service account, environment variables, mounted ConfigMaps and Secrets,
policy objects. No API-server access is needed: actual state comes from
kube-state-metrics and the mesh through `metrics`; intent from manifests
through `scm`; drift from the GitOps tool's metrics.

**Deriving** (bootstrap, or when an identifier breaks). It works out the
identifiers from the repository, step by step, and stops when the sources
agree. With `hello` as the example:

1. *Which binaries include the changed code?* `go list -deps` from each
   `cmd/` directory. → `cmd/hello`.
2. *Which image carries that binary?* The Dockerfile's `COPY` and
   `ENTRYPOINT`. → one image, one binary.
3. *What is the image called?* The CI workflow's publish step. →
   `ghcr.io/smallstepgiantleap/hello`.
4. *Which pod template runs that image?* The manifests in this repository
   or, via the org finder, in the GitOps repository. From it: the
   namespace, the pod labels (the selector), the container name, the
   workload kind. → `namespace: hello`, `app.kubernetes.io/name=hello`,
   container `hello`, a Rollout.
5. *In which clusters?* Overlays, Helm values per cluster, ApplicationSet
   selectors, fleet files. → all three.
6. *Confirm.* Run the profile's instance query with the derived labels per
   cluster. → ready pods in dev, staging, prod. Confident.
7. *Else ask.* If step 4 finds no pod template, or step 6 finds no pods,
   the question names exactly which step came up empty and what it blocks.

Steps 1–4 are deterministic reads. Step 5 is where companies diverge and
where a human is most likely needed, once.

A cluster without kube-state-metrics leaves actual state unknown for that
cluster; the discoverer reasons from intent only, says so, and the
affected findings carry lower confidence.

### 6.3 Cloud discoverer

Given a cloud resource reference — a bucket, a topic, a database, a
secret path — it answers, through one generic connector call
`cloud_get(provider, resource, attribute)`: does it exist, what IAM it
carries, what quota it consumes, whether it holds data and is protected,
which identities are bound to it. Provider adapters sit behind the call;
nothing above the connector knows a provider's API. With the topology
discoverer it answers the runtime half: which service account the pod
runs as, whether it maps to a cloud identity (Workload Identity, IRSA) or
a mounted key, and whether that identity holds the role the code needs.

Where do references come from? From the code and the intent, every run:
an environment variable in the pod template, a ConfigMap value, a
constant in the diff. Only when a reference cannot be found — the value
is set at runtime, or lives in a Secret — does it become a question, and
the answer is recorded in `discover.yaml` (section 7.1). A provider or
project the proxy is not bound to is also a question.

### 6.4 Questioner

The lead batches and deduplicates what the specialists could not find and
turns each into one question answerable with **yes** or **no** (plus the
short "where/which" a *yes* needs). Every question states the dimensions
it blocks. Answers arrive as comment replies; a short follow-up run turns
the prose into the file change and commits or suggests it. A question
resolved without an answer by someone other than the author is waived:
the next run proceeds, carries the unknown as a warning with the waiver's
name, and does not ask again on this PR.

## 7. Memory and configuration

Three layers. The agent owns none of them durably.

| Layer | Lives in | Owner | Contains |
|---|---|---|---|
| Identifiers | `.paved-agent/discover.yaml` in the repository | the team (proposed by the bot) | join keys and answers |
| Run memory | the session | nobody | what discovery found this run |
| Proxy configuration | `proxy.yaml` in the deployment | the platform team | backends, fleet, environments, credentials' sources, profile, limits |

### 7.1 Identifiers

The principle: **record join keys and answers; derive everything else
every run.** A join key is what connects this repository to the
environment and is expensive or ambiguous to derive (the chain in 6.2).
An answer is something a human told the agent because no source had it.
Everything derivable from those plus the live environment — callers,
callees, mounted configuration, service account, log selectors — is
derived each run and never written down, because writing it down would
make it stale.

```yaml
# .paved-agent/discover.yaml
service: hello
workloads:
  - namespace: hello
    selector: app.kubernetes.io/name=hello     # labels that identify this service's pods anywhere
    container: hello                            # the container name telemetry reports
    clusters: all                               # or a list of cluster ids, or an environment name
docs:                                           # author-stated rules, read live each run, never copied
  - CLAUDE.md
  - docs/runbook.md
answers:                                        # only what no source could tell us
  - cloud: {provider: gcp, resource: gs://hello-exports}   # value is set at runtime
```

Rules:

- **Identifiers only.** A lint rejects URLs, hostnames, and anything that
  looks like a secret.
- **Followed every run.** Resolved instances are never written back.
- **Self-healing.** A selector that matches nothing in any cluster marks
  the entry broken for the run; derivation restarts; the outcome is a
  proposed fix or a question. The report says which entry broke and why.
- **Not deployed is an answer**: `workloads: []` with a probe
  (`probe: {container: greeter}`) that re-opens the question if it ever
  returns something.
- **Docs are referenced, not copied**, so they cannot go stale in memory.

Invariants, two kinds:

- **Author-stated** — rules a human wrote in the referenced docs.
  Enforced: a diff that breaks one is a finding whose evidence is the
  rule (file and line) and the offending change.
- **Agent-inferred** — patterns in the code. Never stored. At review time
  the lead derives them from *similar code in the same repository*:
  sibling handlers, other clients, neighbouring tests. "Every other
  handler here counts its status; this one doesn't" is a finding whose
  evidence is the neighbouring handlers.

The same split applies to PR norms: a PR template or a documented review
checklist is enforced; "PRs here usually touch the e2e too" is inferred
at review time from recent merged PRs, not remembered.

### 7.2 Run memory

Everything discovery produced this run. It lives in the session, is
summarized in the report's `discovery` block, and is gone after. If a
value seems worth keeping, the right place is an identifier that
re-derives it.

### 7.3 Proxy configuration

Deployed by the platform team inside the customer's network, configured
once. Repositories never see any of it.

```yaml
# proxy.yaml — one per deployment of the proxy
profile: auto                               # or a name; auto = pick from the capability probe
fleet:
  cluster_label: cluster                    # the metric label that names a cluster
  clusters:
    - {id: dev-us,     env: dev,     labels: {region: us}}
    - {id: staging-us, env: staging, labels: {region: us}}
    - {id: prod-us,    env: prod,    labels: {region: us}}
    - {id: prod-eu,    env: prod,    labels: {region: eu}}
slots:
  metrics: {kind: promql, endpoint: https://thanos.internal, auth: oidc:metrics-reader}
  logs:    {kind: gcp-logging, auth: wif:logs-reader}
  alerts:  {kind: pagerduty, auth: secret:pd-token}
  scm:     {kind: github, org: smallStepGiantLeap, auth: app:paved-agent, allow_bot_commits: [hello, echo]}
  cloud:
    gcp:   {projects: [acme-data, acme-prod], auth: wif:gcp-reviewer}
limits:   {query_range_max: 7d, result_bytes_max: 262144, calls_per_session_max: 200}
```

**Environments.** Every cluster carries an `env`. Discovery results are
grouped by environment, and the report distinguishes "runs in prod" from
"runs in dev" throughout — a blast radius of three dev clusters is not the
same as one prod cluster. A service's `clusters:` may name an environment
(`clusters: prod`) instead of ids.

**One proxy per trust boundary.** If dev, staging and prod share a metrics
backend and a network, one proxy sees them all. If prod is a separate
boundary with its own backends and credentials, it gets its own proxy
with its own `proxy.yaml`; the controller sends the same discovery to each
and merges the results by cluster. Same profile code, different
configuration. M2 runs one proxy.

**`auth` names a source, never a value**: an OIDC client, a workload
identity federation binding, a mounted secret. On the laptop in M2 this is
the operator's ADC with impersonation and a Keychain item; in a cluster it
is the proxy's service account federated to read-only roles. Nothing
above the connector changes when it moves.

**`allow_bot_commits`**, in plain words: may the agent push a commit onto
a PR branch in this repository? Repositories listed here get commits;
all others get a GitHub suggested change that the author applies with one
click. Either way a human merges.

### 7.4 Profiles and the capability probe

A profile is the code that knows, for one metrics stack, how to turn
identifiers into queries. The questions a profile answers are fixed; the
queries differ by stack.

kube-state-metrics is not universal. It ships with the common Prometheus
distributions (kube-prometheus-stack, GKE's and EKS's managed
collectors offer it as a package), but Datadog exposes the same facts as
`kubernetes_state.*`, Google Cloud Monitoring as `kubernetes.io/…`
resources, and some fleets have none of these. So the proxy **probes** at
start: it asks the metrics backend which metric families exist
(`kube_pod_info`, `istio_requests_total`, `rollout_info_replicas_desired`,
`argocd_app_info`, `keda_scaler_active`, …) and records a capability map.
`profile: auto` picks the profile whose required families are present;
missing optional families are reported (`proxy status`) and the
corresponding questions degrade to "intent only, lower confidence" rather
than failing.

The first profile is **kube-state-metrics + Istio**. What it answers, and
how, in words:

- *How many instances are ready, per cluster?* Count pods in the namespace
  whose labels match the selector and whose Ready condition is true. Pods
  are the unit because they exist regardless of workload kind.
  ```
  count by (cluster) (
    kube_pod_info{namespace="hello"}
    * on (pod) group_left kube_pod_status_ready{condition="true"}
    * on (pod) group_left kube_pod_labels{label_app_kubernetes_io_name="hello"})
  ```
- *What kind of workload owns them, and how many are desired?* Follow the
  owner chain: `kube_pod_owner` says a pod belongs to a ReplicaSet or a
  StatefulSet or a DaemonSet; `kube_replicaset_owner` says whether that
  ReplicaSet belongs to a Deployment or an Argo Rollout. Then ask the
  right object for its desired count — `rollout_info_replicas_desired`
  for Rollouts (from the Argo Rollouts controller), `kube_deployment_spec_replicas`,
  `kube_statefulset_replicas`, `kube_daemonset_status_desired_number_scheduled`.
  Unknown kind → read the intent, lower confidence.
- *What is the ceiling?* `kube_horizontalpodautoscaler_spec_max_replicas`
  for the HPA that targets the workload (KEDA creates one); if none, the
  desired count is the ceiling.
- *How much traffic, from whom, how fast?* The mesh's request metrics by
  destination workload: `istio_requests_total` rated over 5 minutes for
  rps, summed by `source_workload` over an hour for callers,
  `istio_request_duration_milliseconds_bucket` for p99.
- *Is it reachable from outside the cluster?* Callers whose
  `source_workload` is an ingress gateway, plus `kube_service_spec_type`
  = LoadBalancer or an Ingress/Gateway object in the intent.
- *How hot is it?* `container_cpu_usage_seconds_total` and
  `container_memory_working_set_bytes` against
  `kube_pod_container_resource_limits`.
- *Does what runs match what git says?* `argocd_app_info{sync_status,
  health_status}` for the application.
- *Is anything already on fire?* `ALERTS{alertstate="firing"}` in the
  namespace, and alert rules that mention the workload (the rules API).

A second profile covers the same questions for a meshless fleet with
OTel/gRPC metrics; a third could cover Google Cloud Monitoring's native
Kubernetes metrics for fleets without kube-state-metrics. Adding a stack
is adding a profile, reviewed as code.

## 8. Sample PRs, end to end (code only)

Fixtures live as open draft PRs in the demo org and double as the live
eval. Costs are estimates to be measured in M2; the cap is enforced.

### A. Business logic: `hello` rejects empty names

Diff: `internal/handler/hello.go` returns `InvalidArgument` when `name` is
empty. No manifest changes. `discover.yaml` exists.

- Discovery follows the identifiers: 3 clusters (dev, staging, prod), 2
  ready pods each, a Rollout, desired 2, ceiling 4; rps ~5; callers
  `frontend` only, from inside each cluster; not exposed outside; CPU 12%
  of limit; no firing alerts; synced and healthy.
- The lead reads the diff against the proto: `name` is a plain `string`
  with no comment or validation rule marking it required, so rejecting
  empty input is a behaviour change the contract did not promise; `buf
  breaking` passes. The org finder mounts `frontend` (a caller per the
  metrics); its call site passes the query string through, so empty names
  are possible. The lead also reads `frontend`'s retry configuration for
  this call and `hello`'s server-side limits, because a new error path is
  also a new retry path.
- Instantiation: worst case is every `hello` request from `frontend`
  returning an error — contained to one RPC, one caller; weight medium.
- **Correctness:** warning —
  the proto does not declare `name` required and `frontend` forwards user
  input unchecked, so empty names become `InvalidArgument` surfaced to
  users; either document it in the proto or validate in `frontend`.
  **Compatibility:** info — no wire change; old `frontend` builds simply
  see a new error code. **Blast radius:** warning — 3 clusters, 1
  internal caller, staged rollout with analysis in staging and prod, none
  in dev. **Scalability:** info — no load change; `frontend` does not retry on
  `InvalidArgument` (its retry policy covers `UNAVAILABLE` only), so a
  burst of empty names cannot amplify; `hello` has a per-caller admission
  limit as a backstop. **Resilience:** info — the canary's analysis counts
  `InvalidArgument` as a client error, so it would not roll back on it;
  said so. **Debuggability:** info — the status code is counted by the
  platform's interceptor; no alert on it, none needed. **Testability:**
  warning — the handler test covers the new branch; no test in `frontend`
  for the error it now receives.
- Questions: none. Proposals: none.
- Verdict: warning. Cost estimate: $0.8–1.5.

### B. New client: `frontend` calls `ledger`

Diff: `internal/web/handler.go` constructs a `ledger` gRPC client and
calls `GetBalance` per page view. `service.yaml` untouched.

- The lead recognises a new client. Discovery follows `frontend`'s
  identifiers (30 rps, 3 clusters, **exposed through the ingress
  gateway** — external, unbounded callers) and, via the org finder, mounts
  `ledger` and follows its identifiers (12 rps, a Rollout, ceiling 3,
  in-flight 60 of threshold 80, CPU 70% of limit, internal callers only
  today). 1 call per page view → +30 rps (+250%) under normal traffic, and
  whatever the internet sends under abnormal traffic. The lead reads the
  constructor: dials `ledger.ledger.svc:8080` plain, relies on the mesh;
  no deadline; the mesh's default retry policy retries `UNAVAILABLE` twice.
  Reads `ledger`'s `service.yaml`: `frontend` not in `authorizedCallers`.
- Instantiation: worst case is every page view failing or slowing when
  `ledger` does — total for `frontend`'s users; weight full.
- **Correctness:** blocking —
  `frontend`'s `egress` lacks `ledger` and `ledger` does not authorize
  `frontend`; the platform's access-request flow is the fix; the e2e
  would fail. **Compatibility:** info — `GetBalance` is stable; the
  client is generated from the deployed proto. **Blast radius:** warning
  — every page view now depends on `ledger`; no other open PR touches
  `ledger`; the rollout is staged. **Scalability:** blocking — +250% against a ceiling of 3 replicas
  already at 70% CPU and 75% of the in-flight threshold; an external
  caller now drives load into an internal service with no per-caller
  limit on `ledger`'s side; and if `ledger` starts returning `UNAVAILABLE`,
  two mesh retries per page view triple the load at the worst moment.
  **Resilience:** blocking — no deadline; no fallback when `ledger` is
  down, so a `ledger` outage is a `frontend` outage. **Debuggability:**
  warning — the call is instrumented by the platform's client interceptor
  but the handler swallows the error into a 500 without wrapping. **Testability:** warning — no test for `ledger` unavailable or slow.
- Questions: none. Proposals: none (callers and callees are derived, not
  recorded).
- Verdict: blocking. Cost estimate: $1.5–2.5.

### C. Cloud resource: `ledger` exports to GCS

Diff: `internal/export/gcs.go` writes daily exports to the bucket named by
`LEDGER_EXPORT_BUCKET`; `go.mod` adds `cloud.google.com/go/storage`.

- The lead recognises a cloud SDK import. Topology reads `ledger`'s pod
  template in the intent: no `LEDGER_EXPORT_BUCKET` anywhere, service
  account `ledger` with no Workload Identity annotation and no mounted
  key. The cloud discoverer has no reference to follow: the value exists
  nowhere in code or intent. → **question**, check *action required*:
  *"`LEDGER_EXPORT_BUCKET` is set nowhere I can see, so I cannot verify
  correctness (IAM, identity) or resilience for the export. Which bucket
  and project?"* The author replies "gs://ledger-exports in acme-data";
  the bot records it under `answers:` and commits; CI re-runs.
- Second run: `cloud_get("gcp", "gs://ledger-exports", "iam")` → no
  binding for any `ledger` identity; `protection` → retention 30 days,
  no deletion protection.
- Instantiation: worst case is the export never running — a scheduled
  path, no request impact; weight low, except correctness.
- **Correctness:** blocking — config absent at runtime; no credential
  path; no IAM grant. **Compatibility:** info. **Blast radius:** info — 3
  clusters, a scheduled path. **Scalability:** info. **Resilience:** warning — no timeout on the upload; no
  retry; a 2 GB export streamed from memory. **Debuggability:** warning —
  errors logged without the bucket or object name. **Testability:**
  warning — no test for the upload failing.
- Verdict: blocking. Cost estimate: $1.5–2.5 across the two runs.

### D. Unknown placement: `greeter`

Diff: any handler change in `greeter`. No `.paved-agent/`.

- Bootstrap: derivation finds a Dockerfile and a pod template but no
  overlay, and the instance query returns no ready pod with container
  `greeter` in any cluster → question (section 4.1), check *action
  required*.
- The author replies "no". The bot commits `workloads: []` with a probe.
  Second run: blast radius zero; correctness against the contract and
  docs only; the rest "not deployed".
- Verdict: info. Cost: $0.3–0.6 for the first run, under $0.3 after.

### E. Refactor: rename a private function in `echo`

- The lead reads the diff: behaviour-preserving, tests unchanged and
  passing in CI. Discovery follows `echo`'s identifiers (cheap) so the
  report still states where it runs.
- Verdict: info, seven qualities "no impact" with the reason. Cost: under
  $0.3. This case keeps the cheap path cheap.

## 9. Connectors and tools

Everything the agent can do outside the sandbox is a connector call
through the proxy. The question is the smallest library the language
needs. Given Kubernetes and source control, the answer is three:

| Slot | Operations | Bound to (examples) | Read / write |
|---|---|---|---|
| `metrics` | `query`, `query_range`, `series`, `label_values`, `rules` — firing alerts come from `ALERTS` and alert rules from the rules API, so there is no separate alerts slot | Prometheus, Thanos, Mimir, AMP, GMP, Victoria; (later) Datadog, Cloud Monitoring via adapters | read |
| `scm` | `search_code`, `read(repo, ref, path)`, `mount(repo, ref)`, `pr(…)`, `open_prs`, `comment_replies` | GitHub, GitLab | read; writes: `comment`, `check`, `propose` (`.paved-agent/` on the PR branch only) |
| `logs` | `search(selector, query, range)` → bounded lines | Loki, Cloud Logging, CloudWatch Insights, OpenSearch | read |

Why these three suffice for most obligations: under the source-control
assumption, *intent* for everything — manifests, policies, IAM bindings
in Terraform or Crossplane, dynamic configuration — is readable through
`scm`; *actual* state of the fleet is readable through `metrics`; and
*behaviour* is readable through `logs`. The model already knows how
clouds are reached from Kubernetes, how meshes expose traffic, how
policy engines express rules; it needs the facts, not the lessons.

Two more slots come later, when an obligation needs actual-versus-intent
outside the cluster:

| Slot | Operations | When |
|---|---|---|
| `cloud` | `cloud_get(provider, resource, attribute)` — `exists`, `iam`, `quota`, `protection`, `bindings` | M4: the cloud resource obligations, where intent in Terraform is not enough (drift, quotas, protection flags) |
| `incidents` | `incidents(service, range)` from PagerDuty or similar | with the improvement loop (§15): correlating passed PRs with what broke |

A `traces` slot remains possible for debuggability.

Inside the sandbox the agent has `read`, `glob`, `grep` and `bash` over
the mounted repositories. Bash is on because the sandbox is a pod whose
only egress is the Anthropic API and the proxy (section 10): no
credentials, no service-account token, non-root, read-only root
filesystem, CPU and memory limits. `go list`, `git`, `buf` are in the
image.

The connector operations are **custom tools registered on the agents**
(typed schemas, so the model gets validated inputs) and **executed by the
worker** as thin HTTP clients of the proxy. The model knows tool names;
it does not know, and cannot affect, who executes them or with what
identity. `ask` and `propose` are two of those tools, backed by the
proxy's `scm` writes.

## 10. Runtime and security model

### 10.1 The pieces and how a tool call travels

Three places are involved in every review. **Managed Agents** (Anthropic)
runs the lead and the specialists. The **sandbox** pod, in our cluster,
executes every tool call. The **proxy** pod, also in our cluster, holds
the credentials and talks to the real systems.

When an agent wants a tool, Managed Agents emits a `tool_use` event on the
session's stream. The worker in the sandbox pod is listening: for a
built-in (`read`, `grep`, `bash`) it runs the command locally against the
checked-out repositories; for a connector tool it makes an HTTP call to
the proxy, which checks policy, mints a credential, calls the backend,
writes an audit line, and returns the result. The worker posts the result
back onto the stream, and the agent continues. Every tool call crosses the
stream exactly once — that is how a model at Anthropic reaches tools in
our environment at all — and there is one worker per session, not two.

```
  Anthropic                        our cluster (kind in M2)
  ┌──────────────┐                 ┌─────────────────────┐  HTTP  ┌──────────────────────┐
  │ Managed      │  tool_use  ───▶ │ sandbox pod         │ ─────▶ │ proxy pod            │
  │ Agents:      │                 │  worker             │        │  policy, audit       │
  │ lead +       │ ◀─── result     │  repos in /work     │ ◀───── │  credentials (WIF)   │
  │ specialists  │                 │  read/grep/bash     │        │  connectors          │
  └──────────────┘                 │  connector clients  │        │  controller          │
                                   └─────────────────────┘        └──────┬───────────────┘
                                   egress: Anthropic API, proxy          │ read-only, plus
                                   — nothing else (NetworkPolicy)        ▼ 3 writes to the PR
                                                            metrics · logs · alerts · cloud · GitHub
```

### 10.2 Boundaries

- **The sandbox holds no credential of any kind.** Repositories arrive as
  tarballs the proxy serves after cloning with its own token. No
  service-account token is mounted. If the model used bash to call the
  proxy directly instead of through a tool, it would get exactly what the
  tool gives it: policy lives in the proxy, not in the caller.
- **The proxy enforces policy on every call**: read-only operations,
  bounded ranges and result sizes, metric and label allowlists where the
  org wants them, per-session call and cost limits, an audit line per
  call. It owns the single write path: `propose` checks the path is under
  `.paved-agent/` and the ref is the PR's head; `comment` and `check` are
  the other two writes. Branch protection still requires a human to
  merge. The controller — which reacts to PR events, clones, creates
  sessions, posts check states and collects reports — runs in the same
  pod.
- **The PR under review is untrusted input.** Any PR can contain text
  aimed at the agent. The worst a fully subverted agent can do is read
  what the org granted the proxy and propose a file change on the very PR
  it is reviewing, which a human then reads. Wide in reach, narrow in
  power. The proxy is the only component worth hardening, and it is
  small: five adapters and a policy layer.
- **Credentials are short-lived.** The proxy pod's service account is
  federated to read-only cloud roles and metric/log readers at call time.
  No long-lived keys; nothing in any repository.
- **Why kind and not Docker alone.** The same manifests (two Deployments,
  a NetworkPolicy, a Service) are the production deployment on a
  customer's cluster; kind is that deployment on the laptop.

### 10.3 Path to a hosted service (future project)

Nothing in M2 is built in a way that blocks this; two things move:

- The **controller** (PR events, session creation, check states, report
  collection) moves out of the proxy pod into a hosted control plane,
  multi-tenant, with billing and the per-tenant Managed Agents
  environment and budget.
- The **proxy** stays inside each customer's boundary — it is the thing
  that must hold their credentials and see their metrics — and the
  sandbox either stays with it or moves to Anthropic's cloud sandbox,
  which also has no network. The control plane talks to each tenant's
  proxy over an authenticated channel for the three PR writes and the
  tarball fetch; discovery calls still go sandbox → proxy inside the
  boundary.

Tenant isolation is one proxy, one environment, one budget per tenant.
The report contract, the connectors and the profiles do not change.

### 10.4 Sandboxes per tenant (future project)

A session is a *work item* on the tenant's self-hosted environment, and a
sandbox claims exactly one. The SDK splits the two halves — a poller that
claims items, and a handler that runs one claimed item — which is the
pattern used here:

- **One tenant = one Managed Agents environment, one environment key,
  one namespace, one proxy, one budget.** The environment key is what
  lets the tenant's sandbox-manager claim work for that tenant and
  nothing else.
- **One sandbox = one session.** The sandbox-manager, running in the
  tenant's cluster next to the proxy, claims one item and creates one
  sandbox pod for it: a fresh image, the tenant's NetworkPolicy, no
  credentials. The pod runs the item to completion and exits. Nothing
  carries over between PRs or between tenants.
- **A warm pool** of K idle, unassigned pods per tenant hides image pull
  so that claim-to-running is seconds. Pool size is per-tenant
  configuration.
- **The API** is the sandbox-manager's: claim-and-run one item; set the
  pool size. Tenants who do not need repositories kept on-prem can use
  Anthropic's cloud sandbox instead, with no pool at all — same proxy,
  same session.

**What may be cached across sandboxes.** Fresh pods per session lose
nothing that matters if the expensive, immutable artifacts live outside
the pod: a bare git mirror per tenant (the proxy already clones to serve
tarballs; it keeps the mirror, fetches deltas on PR events, serves
`git archive`), and a per-tenant Go module proxy or read-only module
cache (`GOPROXY` points at it; `GOSUMDB` stays on; a warmer job runs
`go mod download` after merges). Both are read-only to sandboxes — only
the warmer writes, with no model in the loop — per tenant, and hold only
content the consumer verifies (`go.sum`, git SHAs, image digests), so a
bad entry fails rather than lies. Discovery results, derivations and
model outputs are never cached: they are run memory by design. This
cache layer is one of the concrete reasons self-hosted sandboxes beat the
cloud ones for a large Go codebase.

The sandbox-manager — pool, mirror, module proxy, warmers, garbage
collection — is the orchestrator for self-hosted sandboxes; it falls out
of this design rather than being a second system.

## 11. Implementation design

### 11.1 Packages

```
cmd/change-agent      setup | review | worker | proxy
deploy/               kind manifests: sandbox and proxy Deployments, NetworkPolicy, Service
internal/change       the change (exists)
internal/findings     report contract: seven qualities, findings, unknowns, questions, proposals
internal/identifiers  .paved-agent/discover.yaml: schema, load, lint (identifiers only)
internal/profile      capability probe; profiles: kube-state-metrics+istio first
internal/proxy        the HTTP service: slots, adapters, policy, audit, config, repo tarballs;
                      and the controller (PR events, sessions, check states, collect)
internal/connectors   the tools' side: typed schemas registered on the agents, HTTP clients of the proxy
internal/discover     follow identifiers through the proxy with a profile; derive at bootstrap
internal/sandbox      the worker: workdir, unpack tarballs, agenttoolset + connector tools
internal/agents       lead, org-finder, topology, cloud (definitions as code; the change-kind
                      catalogue lives in the lead's system prompt)
internal/session      create, watch, collect the report
internal/github       PRs → changes (exists); checks, comment threads, suggestions, branch commits
internal/classify     milestone-1 rules; kept only until M2 measures a pre-pass
evals/                fixtures (PR refs + expectations), recorded metric/scm responses, judge rubric
```

### 11.2 Session lifecycle

1. The controller (or the CLI) loads the PR and `.paved-agent/`, lints
   it, clones the repository with the proxy's token, and creates the
   session: lead agent version, environment, budget, and an initial
   message holding the brief — the change, the identifiers, the paths of
   the referenced docs, and the roster of specialists.
2. The worker claims the session, fetches the repository tarball from
   the proxy, and serves every tool call. The lead delegates to
   specialists as it needs them.
3. Discovery follows the identifiers through the proxy; broken ones are
   reported to the lead as such.
4. If the lead ends with questions, the controller posts each as a
   comment with its answer-to-file mapping, sets the check to *action
   required*, and the session ends. A reply triggers a short answer run
   (lead only, no discovery) that turns the prose into the file and
   commits or suggests it; the push starts the next full run. In the CLI
   the operator edits the file and re-runs.
5. If the lead ends with a report, the controller validates it (every
   finding has evidence; every dimension has a verdict), attaches it to
   the check, summarizes it in a comment, and applies its proposals.
6. One run per PR; a new head SHA cancels the running session and starts
   over. Nothing is shared between runs except git.

### 11.3 Bootstrap

The first run of a repository with no `.paved-agent/` (or `review
--bootstrap`) is the same session with derivation allowed to spend more.
There is no separate learning step: nothing else is learned.

### 11.4 Report contract

```json
{"change":"org/repo#N@sha",
 "instantiation":{"worst_case":"…","weight":"full",
                  "obligations":[{"quality":"correctness","establish":"…"}]},
 "qualities":{
   "correctness":{"verdict":"blocking","summary":"…"},
   "compatibility":{"verdict":"info","summary":"…"},
   "blast_radius":{"verdict":"warning","summary":"…"},
   "scalability":{"verdict":"blocking","summary":"…"},
   "resilience":{"verdict":"blocking","summary":"…"},
   "debuggability":{"verdict":"warning","summary":"…"},
   "testability":{"verdict":"warning","summary":"…"}},
 "discovery":{"followed":["workloads[0]"],"broken":[],
              "by_env":{"prod":{"clusters":["prod-us","prod-eu"],"instances":4,"rps":4.8,
                                "callers":["frontend"],"external":false,"owner":"Rollout","ceiling":4}},
              "cpu_of_limit":{"prod-us":0.12}},
 "findings":[{"quality":"scalability","severity":"blocking","claim":"…",
   "evidence":[{"kind":"metric","source":"metrics.query","query":"…","value":"…"}],
   "recommendation":"…","confidence":0.85,"studied":["frontend","ledger"]}],
 "unknowns":[{"what":"…","tried":["…"],"blocks":["blast_radius","scale"]}],
 "questions":[{"id":"q1","text":"…","blocks":["blast_radius","scale"],
               "answers":{"yes":{"path":".paved-agent/discover.yaml","content":"…"},
                          "no":{"path":".paved-agent/discover.yaml","content":"…"}},
               "waived_by":null}],
 "proposals":[{"path":".paved-agent/discover.yaml","content":"…","reason":"…"}],
 "verdict":"blocking","summary":"…"}
```

The controller applies `proposals` and posts `questions`; the model never
writes to git directly.

### 11.5 Agents

| Agent | Model | Connector access | Owns |
|---|---|---|---|
| lead | claude-opus-5, effort high | sandbox tools, `change_diff`, `scm.open_prs`, `metrics.rules`, `alerts`, `ask`, `propose`; may delegate to the three below | reading the code, deciding what to discover, judging the seven qualities, questions, proposals, the report |
| org finder | claude-sonnet-5 | `scm.search_code`, `scm.mount`, sandbox tools | finding and mounting the defining repository |
| topology discoverer | claude-sonnet-5 | sandbox tools, `metrics`, `logs` | following and deriving workload identifiers; placement, scale inputs, exposure, identity, configuration presence |
| cloud discoverer | claude-sonnet-5 | `cloud_get` | following cloud references; identity and IAM |

The seven-quality judgment is done by the lead itself in M2, in one
context that holds all the facts. The specialists are unaffected by that
choice: they discover; they do not judge. If reports get long or slow,
the judgment can be split into per-dimension analysts fed by the lead's
fact sheet without changing the contract.

## 12. Costs

Model usage is the only material cost; the laptop and kind are free; GCP
is cents outside the Autopilot hours.

| Operation | Tokens (est.) | Cost (est., at Opus $5/$25 and Sonnet $3/$15 per M in/out; verify against the price list) | Cap |
|---|---|---|---|
| Review with identifiers (A, B) | lead ~300k cached + 40k fresh in, 10k out; 2–3 specialists ~150k in, 6k out each | $1–2.5 | $3 |
| Review, cheap path (E, later runs of D) | lead only; discovery is a handful of queries | < $0.3 | $1 |
| Bootstrap run (no identifiers, or a broken one) | adds derivation, ~+200k Sonnet in | +$0.5–1 | $4 |
| Answer run (prose → file) | lead only, tiny | < $0.1 | $0.5 |
| Fixture eval run (5 PRs, 7 runs) | | $6–12 | $15 |
| GKE Autopilot for fixture C | ~2 pods for 2 h | < $1 | delete after |

Levers, in order: identifiers plus a profile make discovery a handful of
deterministic queries (derivation runs once per repository, not per PR);
Sonnet for everything that reads, Opus only where it judges; prompt
caching of the identifiers and referenced docs; at most 4 specialists per
review; a question ends the session, so waiting costs nothing; the
platform budget as the backstop. Monthly development target: under $50;
`review` prints the session's cost on exit and the proxy keeps a running
monthly total and refuses past the cap.

## 13. Test plan

The live evaluation is a matrix of four kind environments (Istio + Argo,
Linkerd + Flux, Istio ambient + plain CI, no mesh) by ten pull requests,
with expected findings per cell and discovery expectations per
environment: [evals/PLAN.md](evals/PLAN.md). The layers below are how
that matrix and the deterministic parts are exercised.

| Layer | What | How | Pass |
|---|---|---|---|
| Unit | no product names in any rendered prompt (denylist test); identifier schema and lint; capability probe → profile selection; profile query rendering; owner-kind resolution (Rollout, Deployment, StatefulSet, DaemonSet, unknown); external-exposure detection; report validation; check-state transitions; proxy policy (read-only methods, path restriction on `propose`, limits) | `go test` | green |
| Recorded | `discover` against recorded kube-state-metrics/Istio/Argo responses from the sgrpc fleet, grouped by env; `cloud_get` against recorded GCP responses | fixtures under `evals/recorded`, no model | deterministic |
| Derivation | the bootstrap chain on each demo repo reproduces the hand-written `discover.yaml` | `go test` with the repos vendored as fixtures, plus one live run per repo | equal, or a documented difference |
| Live fixtures | the five PRs of section 8 as open drafts in the demo org | `make eval-live` (budgeted) | per fixture: required findings present (by dimension, entity, severity), no forbidden claims, cost ≤ cap, questions == expected, proposals == expected files |
| Self-healing | break `hello`'s selector in a fixture branch | eval-live | reported broken, derivation proposes the fix, no false findings |
| Gate | D: first run asks exactly one question and gates; a prose reply produces the file; second run passes with no question. Waiver: the author resolving the thread keeps the gate; a second account resolving it lifts the gate with the unknown as a warning naming them | eval-live, CLI first, GitHub check in M5 | state sequence as in 4.2 |
| Judge | claim quality against a rubric (evidence matches claim; recommendation actionable; no unsupported numbers) | Sonnet judge over the report JSON, scores stored | ≥ baseline; drift flagged |
| Safety | every connector refuses non-read operations; `propose` only touches `.paved-agent/` on the PR head; the sandbox pod can reach only the Anthropic API and the proxy (tested from inside the pod), mounts no service-account token, runs non-root on a read-only root; event and proposal scan for `github_pat_`, `ya29.`, bearer tokens; a PR containing instructions to the agent produces no action beyond a proposal on itself | unit + scan + one adversarial fixture in eval-live | zero hits |
| Cost | per-fixture ceilings; later runs of a repository cheaper than its bootstrap; monthly cap | eval-live | CI fails on breach |

Golden reports are kept per fixture; a change to a prompt or a profile
re-runs the fixtures and diffs the judge scores before it is merged.

### 13.1 What testing costs

Every run of the lead on the strongest model costs money, so the suite is
built to be mostly free and the paid part is tiered: iterating costs
cents; only the release gate costs dollars.

| Tier | What runs | Model | Cost per run | When |
|---|---|---|---|---|
| Unit + recorded | proxy policy, profiles, discovery, identifiers, controller, check states, the no-product-names test — everything deterministic | none | $0 | every commit |
| Replay | worker, controller and proxy driven end-to-end by a recorded session: a fake event stream replays a past run's `tool_use` events and expects the same results | none | $0 | every commit |
| Smoke | 2 fixtures, lead on the smallest model at low effort; checks the instantiation (right obligations?) and the report's shape, not verdict quality | Haiku | ~$0.05 | every prompt change |
| Dev | 5 fixtures, Sonnet lead and specialists; verdicts and required findings | Sonnet | ~$1–2 | before pushing a prompt or profile change |
| Release | all fixtures, Opus lead; judge scores against golden reports | Opus | ~$6–12 | merge to main; nightly at most |

Why this holds: most iteration is on deterministic code (profiles,
discovery, proxy, controller) and costs nothing; the program is
model-agnostic, so a cheaper model running the same `qualities.yaml` is a
valid lower bound — if it instantiates the right obligations the stronger
model will too, and if it cannot produce a well-formed report the prompt
is broken regardless of model; every fixture shares the same prompt
prefix, so cached input is a fraction of the price and fixtures run
back-to-back to keep the cache warm; a change to one quality's entry
re-runs only the fixtures that exercise it; and three caps bound the
damage of a runaway loop — per run, per CI run, per month.

## 14. Milestones

- **M2 — identifiers, profile, proxy.** `discover.yaml` schema and lint;
  the capability probe and the `kube-state-metrics+istio` profile; the
  proxy service with `metrics` and `scm` slots, policy and tarballs;
  connector tools as HTTP clients; `discover` with owner kinds, env
  grouping and exposure; derivation; the questioner on the CLI; the
  sandbox and proxy deployed on kind with the NetworkPolicy;
  kube-state-metrics and Istio metrics on the sgrpc fleet; hand-written
  identifiers for the demo repos; fixtures A, D, E. Exit: A reviewed with
  seven qualities; D asks exactly one question and passes after the file
  lands; E under $0.3; the derivation test matches the hand-written files.
- **M3 — cross-repo and signals.** Org finder, `scm.open_prs`, `logs` and
  `alerts` slots, fixture B. Exit: B blocks for correctness, scale and
  resilience with metric and policy evidence, including retry
  amplification and external exposure.
- **M4 — cloud.** `cloud_get` with the GCP adapter and WIF, fixture C with
  a short-lived Autopilot cluster. Exit: C asks for the bucket once, then
  finds the missing config, identity and IAM grant.
- **M5 — surfaces.** GitHub check with the states of 4.2, question threads
  with prose answers turned into commits, opt-in bot commits. Then Slack
  notifications linking back to the PR.

## 15. How the agent improves

"Self-learning" means four different things here, in increasing order of
ambition. The first exists; the second and third are what the milestones
build toward; the fourth is a research question we do not claim to solve.

1. **Learning the environment.** Identifiers in `.paved-agent/`, answers
   to questions, the capability probe. The agent learns *facts*; the
   program does not change. This is what §7 describes.

2. **Improving the program through evaluation.** The program is text and
   code under version control: the obligations catalogue, the system
   prompts, the profiles. A change to it is a PR, and the fixtures plus
   the judge are its test suite. That makes the ordinary engineering loop
   available — propose a change, run the evals, keep it if the scores
   rise — and it makes the loop automatable: a session whose task is "the
   review of fixture B missed retry amplification; propose the smallest
   change to the catalogue that catches it without regressing the others"
   can open that PR itself. Humans merge. Managed Agents' *outcomes*
   (a grader iterating an agent against a rubric) is a platform primitive
   for exactly this loop.

3. **Learning from what happened.** Every review produces signals we can
   capture: a human dismissed a finding (false positive), a human added a
   point the agent missed (false negative), a question's answer, and —
   with the `incidents` slot — a PR the agent passed that was later
   implicated in an incident. Each becomes a fixture: the PR, the
   environment snapshot from the run's `discovery` block, the expected
   outcome. The eval set grows from production, and loop 2 runs against
   it. This is the flywheel: outcomes → fixtures → program changes →
   gated by the fixtures. The agent that reviews PRs improves by the same
   PR loop it reviews.

4. **Learning new capabilities.** When a fixture fails because the
   program *cannot* express the fix — a metrics dialect the profile does
   not know, a derivation step the chain lacks — the change is code, not
   prompt: a new profile, a new connector adapter, a new obligation kind.
   An agent can draft it; a human reviews it as any code. What we do not
   attempt is learning inside the model's weights; the program stays
   outside the model, where it can be read.

What M2–M5 do for this: M2 records every run's `discovery` block and
report so fixtures can be built from them; M5's surfaces capture
dismissals and additions as structured signals (a reaction or a reply on
the finding's comment); the `incidents` slot and the fixture-builder
command come after M5, and loop 2 is run by hand on the five fixtures
until there are enough production-derived ones to run it unattended.

## 16. Decided

1. Seven qualities: blast radius, correctness, scale, resilience,
   debuggability, test coverage.
2. Author-stated invariants are enforced from the referenced docs;
   agent-inferred ones are derived at review time from similar code in
   the same repository and never stored.
3. Bash is on inside the kind-deployed sandbox pod; the NetworkPolicy,
   the absence of credentials and the pod's hardening are the boundary.
4. The seven-quality judgment is the lead's job in M2; the specialists
   discover and do not judge.

## 17. Open for review

1. **Bot commits.** Should the agent be allowed to push a commit onto a
   PR branch at all, or should it always use GitHub's suggested-change
   feature (one click for the author to apply)? Proposed: allowed per
   repository, listed in `proxy.yaml`; suggested changes everywhere else.
2. **Second profile.** After kube-state-metrics + Istio, which stack
   matters most to support next: a meshless fleet with OTel/gRPC metrics
   (same facts, different metric names), or Google Cloud Monitoring's
   native Kubernetes metrics (for fleets with no kube-state-metrics)?
5. **The qualities file** (§5.1) shows two of seven entries. Before M2's
   prompts are written, all seven — definitions, instantiation guidance,
   evidence classes, severity and weight — are the thing to review most
   carefully; it is the program.
3. **Multiple proxies.** When prod is its own trust boundary, the
   controller fans one discovery out to several proxies and merges by
   cluster. Fine to leave this out of M2 and run one proxy over all
   environments on kind?
4. Anything in section 8 that does not match how you'd expect the reviewer
   to think.
