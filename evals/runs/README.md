# Recorded live runs

Each directory holds one review: the report as JSON and as text, and the
session trace (every event, one line each). Kept for the write-up and as
the raw material for fixtures.

Costs are what the platform reports as `list_cost` for the session
(`change-agent sessions` lists every session with it; `change-agent cost
--session` attributes a session's tokens to the tool calls that caused
each turn). The earlier estimates from token counts ran *low*: the staged
hello#3 review was $1.20 — $0.48 for the instantiator, $0.72 for the
executor — not the $0.72 first recorded; E5 was $1.65; hello#2 dev tier
$0.12. Ledger as of 2026-10-06: 12 sessions, $10.46.

| Run | PR | Case | Tier | Time | Cached input | Output | Tool calls | Verdict |
|---|---|---|---|---|---|---|---|---|
| `2026-10-05-hello1` | hello#1 (merged access request) | bootstrap, no identifiers | release (Opus lead) — **$2.83** | 974 s | 1.37M | 81k | ~150, 3 threads | warning, 18 findings, 4 questions |
| `2026-10-05-hello1-run3` | hello#1 | bootstrap, after the token diet | release — **$2.03** | 593 s | 1.50M | 49k | 92 | warning; stopped by the $2 budget |
| `2026-10-05-hello2-dev` | hello#2 (fixture P8, refactor) | identifiers with `measure:` present | dev (Sonnet lead) — **$0.12** | 42 s | 68k | 4.2k | 8, no delegation | info |
| `2026-10-05-hello3-p4` | hello#3 (fixture P4, reject blank names) | identifiers present | release (Opus lead) — **$1.16** | 338 s | 657k | 26.7k | ~90, 3 threads | warning: canary analysis blind to InvalidArgument; e2e matcher accepts it; zero alert rules; one question (what does the live caller send?) |
| `2026-10-06-e5-p13` | pra-client#1 (fixture P13: call rate 5 → 50 rps, client and server in different clusters, a stock-Envoy appliance between them) | identifiers present, no `path` line | staged: Opus instantiates ($0.41), Sonnet executes ($1.24) — **$1.65** | 706 s | 1.75M | 65k | ~200 | **warning**: found the appliance from `pra-infra/e5/envoy.yaml` and its stats — 20 rps token bucket, ×3 retries not on 429, breakers untripped; server 2/2 with headroom; recorded `path:`; also found that the claimed 50 rps is unreachable (grpcurl overhead, measured) and that an `envFrom` ConfigMap change needs a pod restart; one question. The executor ran `docker run` on the laptop to measure — bash is now off outside the pod. |
| `2026-10-06-e5-p13-sandbox` | pra-client#1 again, **served by the sandbox pod** (bash in the shell container; no credential in the pod), after the cost fixes | identifiers present, no `path` line | staged: Opus instantiates ($0.33, 3 turns), Sonnet executes ($0.89) — **$1.22** vs $1.65 | 555 s | 127k | 51k | ~115 | **blocking** this time: same appliance, same 20 rps bucket, but argued that a shared token bucket drained by one client starves every consumer of the route; one false positive feeds that (Envoy's internal `async-client` stats read as a second consumer); a live 29 rps reading left "unreconciled" because the specialist hit the 60-call cap. Lead: 11 turns, none spent waiting (was 35, 20 waiting). The report was still written three times — this time because the strict parser rejected `instances` as a per-cluster map and the collector nudged; fixed (lenient parse, nudge only for a missing report). Without those two nudges ≈ $1.05. |
| `2026-10-06-hello3-sandbox` | hello#3 again, **served by the sandbox pod**, after the cost fixes | identifiers present | staged: Opus instantiates ($0.56, 8 turns — it read the rollout's analysis template, the e2e harness and CI before writing, which is where the findings live), Sonnet executes ($0.46) — **$1.02** vs $1.20 | 327 s | 102k | 26k | ~35 | **blocking**, same findings as before (canary analysis blind to InvalidArgument, e2e matchers accept it, no alert rules, the question about what `traffic` sends). Lead 10 turns, none waiting; org-finder 8 turns (was 20: the futile search is gone). The executor fell $0.72 → $0.46; the Opus stage is now more than half the bill. |
| `2026-10-05-hello3-staged` | hello#3 | identifiers present | staged: Opus instantiates ($0.48), Sonnet executes ($0.72) — **$1.20** | 377 s | 1.37M | 30.6k | ~85 | blocking; same findings as above plus the dev overlay stripping the analysis; the executor spent most of its tokens searching 16 repositories for a workload manifest that exists in no repository, before asking |

What the first run found on its own, with no identifiers and no product
named anywhere in its prompts: the stack (Istio 1.31.1 sidecar with
strict mTLS, Argo Rollouts, KEDA via its HPA, Pod Security admission with
no policy engine, Prometheus with kube-state-metrics and no alert rules);
that the PR's effect was already live in the cluster; that a load
generator shared the caller's mesh identity; that the cluster ran the dev
overlay while labelled prod; that nothing alerts on the new path.

What the third run shows: once a repository carries its identifiers and
the measurement queries the reviewer wrote for it, the review reads the
numbers instead of spending turns discovering them.

## Where the money went (2026-10-06 analysis)

`change-agent cost` over the sessions above. In every session output
tokens were the largest bucket, then cache reads (turns × context), then
new input. The turns, by what caused them:

| session | lead turns | of which waiting | the pattern |
|---|---|---|---|
| E5 executor, $1.24 | 35 (+20 in specialists) | 20 | `sleep` / "check" while the topology specialist ran: 700k cache-read tokens for nothing; then the 7k-token report written three times — once before the specialist replied, twice more because the collector raced its own nudge |
| hello#3 executor, $0.72 | 18 (+20) | 10 | waited for the org-finder, which spent 20 turns searching 16 repositories for a manifest that exists in none |
| hello#3 instantiator, $0.48 | 8 | — | six turns of `cat` to orient before writing 5k tokens of obligations |
| hello#2 dev, $0.12 | 4 | 0 | the shape to aim for: identifiers present, measured facts in the brief, no delegation |

What changed as a result: the lead ends its turn after delegating and
the collector knows that is not the end (`session.Wait`); the last
message that parses is the report, prose after it does not trigger a
nudge; the executor writes `instantiation: {adopted: true}` instead of
copying the senior reviewer's text back; the diff and the changed files
are in the brief so the instantiator can write after zero or one tool
call; long label listings come back grouped by family.

What the rerun (`2026-10-06-e5-p13-sandbox`) says they were worth: the
instantiator went from 4–8 turns to 3 and $0.41 → $0.33; the lead from
35 turns to 11 with zero spent waiting; the executor $1.24 → $0.89; the
whole review $1.65 → $1.22, and ≈ $1.05 once the parser stops asking for
the report again. What got worse: the topology specialist ran 20 turns
(was 11) and hit the per-session call cap — the grouped `__name__`
listing makes it drill into families one `contains` at a time, and it
re-measured what the brief's `measure:` block did not cover. Next lever
is there: a `measure:` block for the appliance's counters, so the
numbers arrive with the brief.

hello#3 rerun (`2026-10-06-hello3-sandbox`): $1.20 → $1.02; the executor
$0.72 → $0.46 with the same findings. The instantiator did not get
cheaper ($0.48 → $0.56): given the diff and the changed files up front,
it still read the rollout analysis template, the e2e harness and CI
before writing — six reads, each informing the next — and its
obligations are what made the executor's work short. The Opus stage is
now more than half of a staged review; whether a PR deserves it is the
tiering question (DESIGN §12.2), not a prompt question.
