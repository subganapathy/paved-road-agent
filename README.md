# paved-road-agent

A GitHub webhook that reviews every pull request in an organisation for
the properties a good change must have — and checks them against the
*running system*, not just the diff. It is built on
[Anthropic Managed Agents](https://platform.claude.com/docs/en/managed-agents/overview):
the models run at Anthropic; every tool they call runs in a sandbox pod in
your cluster that can reach exactly one address.

It reviews. It never executes. It never holds a credential.

```
verdict: WARNING
No blocking issue: both dev clusters, 1 client replica, and the path appliance
(standalone Envoy, 20rps local rate limit + untripped circuit breakers) already
cap and protect the server. The real risk is correctness, not capacity: the
claimed 50rps is unreachable …

[warning] within_budget (confidence 0.85)
  The path appliance enforces a 20rps ingress token-bucket rate limit on the
  api.e5.internal route, below both the claimed 50rps and the realistic ~20-25rps
  the loop can reach, so sustained traffic above ~20rps will receive HTTP 429
  rather than reach hello_server_cluster.
  evidence (config): pra-infra e5/envoy.yaml local_ratelimit = token_bucket
                     max_tokens=20 tokens_per_fill=20 fill_interval=1s, status=429
  evidence (metric): envoy_http_local_rate_limit_ok / _rate_limited = 2939 / 0 at ~4.5rps
  recommend: The appliance protects the server, but the experiment as described
             cannot exercise the server at 50rps; expect sustained 429s above ~20rps.

QUESTION q1 (blocks correct, reversible):
  Does the pipeline that applies deploy/client.yaml perform a rollout restart after
  a ConfigMap-only change?  if yes → write to .paved-agent/discover.yaml: …
```

That is a real review ([evals/runs/2026-10-06-e5-p13](evals/runs/2026-10-06-e5-p13))
of a PR that raised a client's call rate 5 → 50 rps. The client and the
server live in different Kubernetes clusters with a stock Envoy appliance
between them. Nothing told the agent about the appliance: it followed the
hostname, found the appliance's config in a *different* repository, read
its stats, and reasoned about what a 20 rps token bucket does to a 50 rps
experiment. It also asked a question nobody could answer from the code,
and offered to record the answer in the repository so it never has to
ask again.

## What it checks

Every change is judged on nine properties, each named for the incident
it prevents: **correct**, **reversible**, **within budget**,
**stable under failure**, **progressively delivered**, **available**,
**secure**, **observable**, **proven**. They are written once, as prose,
in [internal/agents/qualities.yaml](internal/agents/qualities.yaml) — no
product names, no profiles, no rules engine. The model instantiates them
for the change in front of it (*what is the worst case here, and what
must be shown to rule it out?*), discovers what the service actually runs
on, and grades from evidence. Three moves; see [DESIGN.md](DESIGN.md).

## It is a webhook

```
GitHub ── pull_request / issue_comment ──▶ change-agent serve (your cluster or laptop)
                                              │  controller: status "impact" = pending
                                              │  pre-measures the repo's own queries
                                              │  creates a session with a $ budget
                                              ▼
                                        Managed Agents (Anthropic)
                                              │  lead + specialists think; every tool call
                                              │  is an event the sandbox answers
                                              ▼
                                        sandbox pod ──▶ proxy ──▶ metrics · repos · DNS · cloud
                                              │
                                              ▼
                              report → PR comment + commit status (success / failure / pending)
```

- **Org-wide.** `change-agent serve` takes one webhook for the whole
  organisation; `watch.repos` narrows it if you want. Install it as a
  GitHub App or an organisation webhook pointing at `/github/webhook`,
  with the HMAC secret in `watch.webhook_secret`.
- **A commit status gates the merge.** `impact` is `pending` while the
  review runs, `success` on info or warning, `failure` on a blocking
  finding, and **`pending` with "action required" when the agent has a
  question** — the PR cannot merge until someone with permission answers
  on the thread. The bot writes the answer into
  `.paved-agent/discover.yaml` on the PR branch (linted: no URLs, no
  hostnames, no secrets) and re-reviews.
- **The repository is the memory.** `.paved-agent/discover.yaml` holds
  the service's identifiers (namespace, selector, container, clusters),
  its stack binding with a verify query per line, the `measure:` queries
  the team wrote, and past answers. The controller runs the measures
  *before* the session, so the review starts with numbers instead of
  spending turns finding them: the same repository cost $2.83 to review
  bare and $0.12 once the file existed.
- **No public URL?** `serve --poll 60s` polls open PRs instead.

## The sandbox

The models run at Anthropic. Everything they *do* runs here, and "here"
is deliberately small ([deploy/README.md](deploy/README.md)):

```
 sandbox pod (kind + Calico, or any cluster)          laptop / cluster
 ┌──────────────────────────────────────┐             ┌────────────────────────┐
 │  worker              shell           │   one IP,   │ proxy                  │
 │  proxy token         nothing         │   one port  │  credentials           │
 │  read / grep / glob  bash            │ ──────────▶ │  policy, budgets, audit│
 │        └── /work ──────┘ (loopback)  │             │  agent-API forward     │
 └──────────────────────────────────────┘             └────────────────────────┘
   egress: the proxy — nothing else.  No DNS. No cluster API. No metadata. No internet.
```

- **The shell holds nothing.** `bash` runs in a second container with no
  token, a different uid, no view of the worker's processes, and only
  `/work` in common. The worker forwards each call over the pod's
  loopback. A model that finds a way to make the shell talk to the proxy
  gets a 401.
- **The worker holds one credential** — the proxy's token — and the
  proxy holds everything else. Even the worker's own platform calls
  (claim a work item, heartbeat the lease, serve the session's stream)
  go through the proxy, which forwards nine allowlisted paths and
  injects the environment key. No Anthropic credential is in the pod.
- **The pod proves it before it works.** At start the worker checks: not
  root, no container-runtime socket, read-only system, no service
  account token, no route to the internet, to DNS, to the cluster API or
  the metadata service — and a route to the proxy. Any failure and it
  refuses to claim work. `hack/sandbox/verify.sh` runs the same fourteen
  checks from inside the shell, the way an attacker would.
- **The proxy is the library.** Metrics (PromQL, bounded ranges, clipped
  results), repositories (org-scoped, served as tarballs), DNS, cloud
  (`cloud_get(provider, resource, attribute)`, read-only). Three writes,
  all to the PR under review: a comment, a commit status, a file under
  `.paved-agent/`. Every call is audited with the session that made it.

Why this much? Because a prompt is not a boundary. In the review quoted
above, running on a laptop with a plain `bash`, the executor wanted to
know whether the client image's `sleep 0.02` really slept — so it found
Docker on the PATH and ran the image. Clever, correct, and exactly what
the sandbox exists to make impossible. In the pod it has a real shell,
and that shell's world is `/work`.

## Try it from the CLI

You need Go 1.26, a Managed Agents workspace, a Kubernetes cluster you
can read metrics from (kind is fine), and `kubectl`, `kind`, `docker`.
Credentials are never values in files — only *sources*: `env:NAME`,
`keychain:SERVICE` (macOS), or `none`. Developed on a Mac with Docker
Desktop; on Linux use `env:` sources and, for the pod, export
`PRA_PROXY_TOKEN` and have the proxy listen on the Docker bridge.

```sh
go build -o change-agent ./cmd/change-agent
cp proxy.example.yaml proxy.yaml           # the fleet, the metrics endpoint, the GitHub org

./change-agent setup --config proxy.yaml   # once: the self-hosted environment + five agents
                                           # then create an environment key in the Console

./change-agent proxy --config proxy.yaml   # terminal 1: the connectors

hack/sandbox/up.sh                         # terminal 2: kind + Calico, images, the pod
hack/sandbox/verify.sh                     # fourteen checks from inside the shell

./change-agent review --config proxy.yaml --pr yourorg/service#42 \
    --worker=false --budget 1 --out report.json        # the pod serves it
```

`--dry-run` prints the brief the lead would receive — the diff, the
changed files, the identifiers, the pre-measured facts — with no API
call. `--tier` picks the shape: `staged` (default: the strongest model
writes the obligations, a smaller one executes them), `auto` (triage on
the smaller model, escalate on a warning), `dev`, `release`.
`--commit-proposals` lets the bot commit a proposed `discover.yaml` to
the PR branch.

Then look at what it cost, and why:

```sh
./change-agent sessions --config proxy.yaml                # every session with its list cost
./change-agent cost --config proxy.yaml --session sesn_…   # tokens attributed to the tool call
                                                           # that caused each turn, per agent
./change-agent trace --config proxy.yaml --session sesn_…  # every event, one line each
```

Every session carries a budget the platform enforces. The costs in
[evals/runs](evals/runs/README.md) are the platform's own figures, not
estimates.

## Evals

[evals/PLAN.md](evals/PLAN.md) defines environments — a single cluster
with a sidecar mesh, progressive delivery and an autoscaler; a cluster
where half the services are on a different mesh; two clusters with an
appliance between them — and thirteen PRs with a known right answer: a
refactor that should pass quietly, a logic change whose canary analysis
is blind to the new error, a dropped NetworkPolicy, a Terraform plan that
destroys a database, a tenfold rate increase through a rate limiter.
[evals/runs](evals/runs) holds every live run: report, trace, cost.

## Layout

```
cmd/change-agent        setup | proxy | worker | review | serve | trace | cost | sessions
cmd/shell-sidecar       the shell container
internal/agents         the five agents as code; qualities.yaml is the program
internal/identifiers    .paved-agent/discover.yaml: schema, lint, what is missing
internal/proxy          slots (metrics, scm, dns, cloud, agent-API forward), policy, audit
internal/connectors     the typed tools the models see; each is an HTTP call to the proxy
internal/sandbox        the worker: claim, serve, verify confinement
internal/shell          bash across the container boundary
internal/session        brief, create, wait, collect, nudge; staged and tiered; cost
internal/findings       the report contract
internal/controller     webhook, polling, commit status, the answer path
deploy/                 the sandbox pod, its policy, the kind cluster that enforces it
hack/                   the eval environments
```

## Where this goes

Single tenant today: one organisation, one environment, one proxy, one
sandbox pod serving one session at a time. The next step is designed
([DESIGN.md §10.3–10.4](DESIGN.md)) and nothing here blocks it:

- **Tenant-level isolation.** One tenant is one Managed Agents
  environment, one environment key, one namespace, one proxy, one
  budget. A tenant's sandbox manager can claim that tenant's work and
  nothing else; its proxy can see that tenant's systems and nothing
  else. The residual risk of today's shape — the worker and the shell
  share a pod, so a subverted worker could claim a sibling session —
  disappears when the process that claims is not the pod that executes.
- **One sandbox per session, from a warm pool.** The manager claims a
  work item and hands it to a pod that already exists: fresh image, the
  tenant's policy, no credentials, destroyed when the session ends. A
  pool of K idle pods per tenant hides the image pull, so
  claim-to-first-tool-call is seconds, and nothing carries over between
  PRs or tenants except what the repository chose to remember in
  `.paved-agent/`.
- **The control plane moves out; the proxy stays in.** PR events,
  session creation, check states and billing become a hosted service;
  the proxy stays inside each tenant's boundary, because it is the thing
  that must hold their credentials and see their systems.

## License

Apache-2.0.
