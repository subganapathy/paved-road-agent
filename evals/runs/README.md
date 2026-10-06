# Recorded live runs

Each directory holds one review: the report as JSON and as text, and the
session trace (every event, one line each). Kept for the write-up and as
the raw material for fixtures.

| Run | PR | Case | Tier | Time | Cached input | Output | Tool calls | Verdict |
|---|---|---|---|---|---|---|---|---|
| `2026-10-05-hello1` | hello#1 (merged access request) | bootstrap, no identifiers | release (Opus lead) | 974 s | 1.37M | 81k | ~150, 3 threads | warning, 18 findings, 4 questions |
| `2026-10-05-hello1-run3` | hello#1 | bootstrap, after the token diet | release | 593 s | 1.50M | 49k | 92 | warning; stopped by the $2 budget |
| `2026-10-05-hello2-dev` | hello#2 (fixture P8, refactor) | identifiers with `measure:` present | dev (Sonnet lead) | 42 s | 68k | 4.2k | 8, no delegation | info |

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
