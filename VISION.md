# paved-road-agent: vision

An agent that assesses the impact of a change before it ships, by learning
about the environment the change will run in. It classifies the change,
dispatches one specialist per impacted aspect to study the environment, and
returns findings with evidence and a verdict. It reviews; it never executes.

## The problem

Every SaaS company ends up with a platform: billing, quotas, infra
provisioning and image pipelines, an application core (APIs, async
workflows, persistence, security guardrails, observability, CI/CD), and
day-2 operations. And every one of them has the same four pains:

- nothing protects the system from a bad decision by a developer;
- nothing isolates a bad deployment or a bad infra update;
- evolution work stalls half-way, so environments are heterogeneous (a
  central control plane with a service mesh and traffic shifting next to
  regional ones without either);
- nobody can troubleshoot a fine-grained control plane end to end.

Golden paths fix this in greenfield. Companies live in brownfield, and
frameworks fail there because they are prescriptive. The common thread
through the four pains is **safety of change in heterogeneous
infrastructure**: discovering what exists, judging a change against it,
executing with a bounded blast radius, verifying, and diagnosing.

## Principles

1. **Deterministic core, agents for judgment.** Actions (render, policy,
   rollout, verification) are deterministic and proven, as in
   [scalable-grpc](https://github.com/subganapathy/scalable-grpc). Agents
   discover, assess, explain and diagnose. An agent never holds
   credentials that can change production.
2. **A reviewer with a vote, not an executor.** Output is a PR check and a
   review. Blocking findings fail the check; humans and the deterministic
   platform make the change.
3. **Evidence, not opinions.** Every finding cites what it measured or
   read. "No impact, and here is why" is a first-class result.
4. **Meet the company where it is.** Profiles describe what exists (mesh
   with traffic shifting, mesh without, in-place rollouts only). A plan
   must be valid for the profile in front of it.
5. **Agents are services.** Each has a typed contract, least-privilege
   tools, a budget, and traces.

## Topology

```
change (diff + target environment)
   │
   ▼
classifier ── deterministic rules first (file paths, resource kinds),
   │          the model only for what rules can't decide
   ▼
orchestrator ── durable, idempotent steps; fans out in parallel
   ├── specialist: calls and capacity
   ├── specialist: Terraform plans
   ├── specialist: new resources and quotas
   ├── specialist: deletions
   └── specialist: blast radius and rollout
   ▼
synthesizer ── findings → review + verdict; adds no facts
   ▼
PR check + comment; everything recorded in the fact ledger
```

- Control flow we know is code: classify → fan out → synthesize → post is a
  fixed pipeline, with durable steps because a specialist can take minutes.
- Judgment inside a step is an agent loop with a tool allow-list.
- Specialists do not talk to each other. A second stage is added only when
  a real case needs one.
- Start with one agent and two specialists; grow the topology from measured
  need (context overflow, a privilege boundary), never on day one.

## Taxonomy of impact

The closed set of aspects a change can touch. The classifier emits a subset;
one specialist studies each. Sourced from incidents in the wild.

| Aspect | Trigger in the change | What the specialist decides |
| --- | --- | --- |
| New call introduced | a client call, dependency or egress rule appears | callee capacity at the caller's scale (rate × fan-out); the caller's latency budget with the new hop; retry sanity (idempotency, backoff, budget); bounded vs unbounded work on the callee |
| API contract and callers | proto or schema change | breaking changes; which callers are affected |
| Identity and authorization | authz policy, service account, role | who gains or loses access |
| Network reachability | NetworkPolicy, firewall, mesh config | what can newly connect, what is cut off |
| Traffic and routing | routes, weights, DNS | where traffic goes during and after the change |
| Terraform change | any plan | partial-apply window (order, replacements); plan vs PR description; sparse description → reject; drift separated from the PR's changes; policy results |
| New resource | a create in the plan | quota headroom at the resource's maximum scale; indirect consumption (IPs, API quota); ownership, sharing, cost |
| Resource deletion | a destroy in the plan | rename or real delete; still used (30 days of evidence); dependents across stacks; data and backups; protection flags; recoverability |
| Capacity and scaling | replicas, autoscaler thresholds, limits | headroom under peak load |
| Rollout strategy | canary steps, analysis, environments | can it be rolled back safely; bake time; waves |
| Blast radius | any of the above | which cell, zone, region or share of customers is exposed first |

Learning material per aspect lives in the companion doc
"Change-impact taxonomy: learning material".

## The finding contract

Every specialist returns zero or more findings of this shape:

```json
{
  "aspect": "new-call",
  "severity": "blocking | warning | info",
  "claim": "frontend's calls to echo will time out",
  "evidence": [
    {"kind": "metric", "source": "prometheus", "query": "...", "value": "1200 rps over 24h"},
    {"kind": "config", "source": "deploy/base/network/allow-from-frontend.yaml", "detail": "rule removed in this diff"}
  ],
  "recommendation": "keep the ingress rule, or move frontend off echo first",
  "confidence": 0.9,
  "studied": ["frontend", "echo"]
}
```

Rules: no finding without evidence; "no impact" is a finding with
`severity: info` and the evidence that shows it; the synthesizer may rank
and merge findings but never add claims.

## Memory: the fact ledger

Studying the environment on every change is slow and expensive, so
specialists consult a ledger first and only study what is missing or stale.

- A fact is keyed by entity (repo, service, cluster, resource) and aspect,
  with provenance (where it was read) and validity (a commit, a cluster
  snapshot time, a TTL).
- Deduplication is an idempotency key: change at commit X, environment at
  snapshot Y, profile Z → the same assessment is not produced twice.
- Outcomes are recorded too (did the change ship, did it cause an
  incident), which is what the evals score against.
- Embeddings come later, for fuzzy lookup over unstructured material such
  as postmortems and runbooks. Not before the keyed ledger works.

## Guardrails

- Read-only, least-privilege tools per specialist: metrics, config, git,
  plan JSON, quota APIs. No write credentials anywhere in the agent.
- Budgets per run: tokens, wall time, tool calls, with the orchestrator
  enforcing them.
- Fleet-wide platform changes roll out across repos and clusters as
  canaries, with the same analysis logic rollouts use.
- Everything traced; every finding reproducible from its evidence.

## Why a managed agent

The loop, sessions and the tool-execution sandbox are hosted, so the work
here is the part that is actually ours: the orchestrator, the tool
contracts, the ledger and the evals. Specialists become stored agent
configurations with versions, so a change to a specialist is reviewable
and reversible like any other change. Tools that read the environment
(GitHub, metrics, cluster state, plans) run on our side with least
privilege, and the hosted agent only ever sees their results.

## Milestone 1: impact assessment on a known environment

Environment: the generated repositories (hello, ledger, echo, frontend) and
the kind fleet, where ground truth exists.

1. About ten synthetic changes with known impact: remove an ingress rule,
   set replicas to one, drop the analysis step, add egress to an unknown
   host, a breaking proto change, a harmless comment edit, and so on.
2. The classifier, mostly rules, measured by precision and recall per
   aspect against those changes. The first eval.
3. Two specialists: calls and policy; rollout and capacity. Findings in the
   contract above, posted as a PR check and comment.
4. The ledger, with the dedup key, and a second run that studies nothing.
5. The eval: findings scored against the known impact.

Milestone 2 is diagnosis: inject a failure into the fleet and have an agent
find it from logs, metrics and config.

## Open questions

- Durable runtime: build a minimal one to learn the primitives, or stand on
  an existing workflow engine. Current lean: minimal first.
- Where the fact ledger lives, and who else may write to it.
- How a profile is discovered for a brownfield environment, versus declared.
