# paved-road-agent

A change-impact reviewer built as an [Anthropic managed agent](https://platform.claude.com/docs/en/managed-agents/overview).
Give it a pull request; it works out which aspects of the running system
the change touches, sends one specialist per aspect to study the
environment, and comes back with findings that cite what they measured.
It reviews. It never executes.

```
$ change-agent review --pr acme/orders#412

session sess_01… (budget $3.00)
  lead           change_diff      {}                                         ok
  thread 9f2e1c  ledger_get       {"entity":"payments","aspect":"capacity"}  ok
  thread 9f2e1c  prom_query       {"query":"sum(rate(vikrant_rpcs_total{...  ok
  thread 9f2e1c  kube_get         {"kind":"scaledobject","namespace":"pay... ok
  thread 4ab7d0  kube_get         {"kind":"networkpolicy","namespace":"pa... ok
  …
verdict: BLOCKING
orders starts calling payments at 40 rps; payments is autoscaled to a ceiling of 3 replicas that already run at 71% of their threshold.

[blocking] new-call (payments, orders; confidence 0.82)
  payments has 29% headroom under its KEDA ceiling; the new call adds roughly 55% to its load.
  evidence (metric): prometheus sum(rate(vikrant_rpcs_total{namespace="payments"}[5m])) = 142
  evidence (config): kube_get scaledobject payments/payments = maxReplicaCount: 3, threshold: 80
  recommend: raise maxReplicaCount or ship behind a flag and ramp.
```

See [VISION.md](VISION.md) for why this exists and where it goes.

## How it works

```
                  rules                         managed agents (Anthropic cloud)
PR ──▶ load ──▶ classify ──▶ brief ──▶ ┌────────────────────────────────────────┐
                  │                     │  impact-lead (claude-opus-5)           │
                  │ aspects, reasons,   │   completes classification             │
                  │ unclassified files  │   delegates one task per aspect ──┐    │
                  ▼                     │                                   ▼    │
            no impact?                  │  calls-and-capacity  policy  rollout-  │
            answer without              │  terraform           (claude-sonnet-5) │
            a session                   │        │ read/glob/grep in /mnt/repo   │
                                        │        │ custom tools, run host-side ──┼──▶ Prometheus
                                        │        ▼                               │    kubectl get
                                        │  findings ──▶ lead synthesizes ──▶ JSON│    the ledger
                                        └────────────────────────────────────────┘
                                                                     │
                                            verdict + findings + evidence ◀──────┘
```

1. **Load.** `--pr owner/repo#N` fetches the pull request, its files and
   patches, and both sides of the small configuration files
   (`service.yaml`, `deploy/**`, `*.tf`). `--case evals/cases/x.json` loads a
   synthetic change instead.
2. **Classify** ([internal/classify](internal/classify)). Deterministic rules
   map files and hunks to a closed set of aspects: `new-call`, `contract`,
   `authz`, `network`, `traffic`, `capacity`, `rollout`, `terraform`,
   `new-resource`, `deletion`, `image`, `delivery`. `service.yaml` is diffed
   semantically (keys, not hunks), Terraform plans by `resource_changes`
   actions. Files no rule understands are passed on as *unclassified*, not
   dropped. A change with no aspect, nothing unclassified and no plan is
   answered as "no impact" without starting a session.
3. **Brief.** The lead receives the change, the classification with its
   reasons, the unclassified files, the roster, and where the repository
   is mounted.
4. **Delegate.** The lead reads the unclassified files, decides their
   aspects, and delegates one self-contained task per aspect, in parallel,
   to the specialist that owns it ([internal/agents](internal/agents)).
5. **Study.** Specialists read the mounted repository with the built-in
   `read`/`glob`/`grep` tools and ask for live evidence through custom
   tools that run on the host ([internal/tools](internal/tools)):
   `change_diff`, `terraform_plan`, `prom_query`, `kube_get` (get and list,
   allow-listed kinds), `ledger_get`, `ledger_put`. The sandbox has no
   network and no credentials; the host has read-only ones.
6. **Report.** Findings follow one contract
   ([internal/findings](internal/findings)): aspect, severity, a claim with
   its number, evidence (kind, source, query, value), recommendation,
   confidence, what was studied. A finding without evidence is dropped. The
   verdict is the most severe finding.

The **ledger** ([internal/ledger](internal/ledger)) is what the agent
remembers between assessments: facts keyed by entity and aspect, with the
query they came from and how long they stay valid. Specialists consult it
first and re-study only what is missing or stale, so the hundredth review
of a service costs less than the first.

## Running it

Prerequisites: Go 1.25, an `ANTHROPIC_API_KEY` with Managed Agents access,
and, for live evidence, a Prometheus URL and a kube context with read
access. Everything live is optional: a tool whose backend is not
configured says so, and that becomes evidence ("metrics unavailable")
rather than a guess.

```sh
go build -o change-agent ./cmd/change-agent

# Once per workspace: create the environment and the five agents.
# Re-run after editing a prompt; each agent gets a new version.
export ANTHROPIC_API_KEY=…
./change-agent setup                     # writes agents.lock.json

# What the rules say, no model involved.
./change-agent classify --pr smallStepGiantLeap/hello#1

# See exactly what the lead would be told, no API calls.
./change-agent review --pr smallStepGiantLeap/hello#1 --dry-run

# A live assessment, capped at $3.
export PROMETHEUS_URL=http://localhost:9090
export KUBE_CONTEXT=kind-sgrpc
./change-agent review --pr smallStepGiantLeap/hello#1 --budget 3 --out report.json
```

`GITHUB_TOKEN` is optional for public repositories and required for
private ones (it is used both to read the pull request and to mount the
repository in the session).

Sessions are billed at API rates. Every `review` carries a budget
(`--budget`, default $3) that the platform enforces; the trace of every
session is in the Console under Sessions.

## Evals

[evals/cases](evals/cases) holds synthetic changes with a known impact:
one replica in prod, a dropped NetworkPolicy, a canary without analysis,
egress to an unknown host, a breaking proto change, a Terraform plan that
destroys a database, a Dockerfile that runs as root, a CI change that
skips e2e, and so on, plus changes that should produce *no* impact
(comments, handler logic). `go test ./...` scores the classifier against
them (precision and recall are printed) and checks the agents' wire
format. The same cases drive live runs with `review --case`, which is how
the specialists are scored.

## Layout

```
cmd/change-agent      setup | classify | review
internal/change       a change: files, patches, both sides of configs, a plan
internal/classify     rules → aspects, with reasons
internal/findings     the finding contract, validation, verdict
internal/agents       the lead and four specialists, as code
internal/tools        host-side custom tools, one registry for setup and serving
internal/ledger       facts with provenance and validity
internal/github       pull requests → changes
internal/evalcase     the synthetic cases
evals/cases           the cases themselves
```

## Status

Milestone 1 (see VISION.md): offline core and the live loop are in place.
The next steps are scoring the specialists against the eval cases on a
real cluster, posting the report as a PR check, and a GitHub App trigger.

## License

Apache-2.0.
