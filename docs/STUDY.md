# Study guide: what was built, end to end

A reading order for the system, with the questions to be able to answer
after each part. Everything here is in this repository or in the Go SDK
(`github.com/anthropics/anthropic-sdk-go`); paths are given so you can
open the code beside the text.

## 0. The one-paragraph map

A **review** is one Managed Agents **session**. The **lead** agent (a
versioned config: model, system prompt from `qualities.yaml`, tools)
reads the pull request, instantiates the nine properties, delegates
discovery to **specialists**, grades, and writes a JSON report. Models run
at Anthropic. Every tool call they make is an **event** on the session's
stream; our **worker** (the sandbox) executes it — built-ins locally over
the checked-out repositories, **connector** tools by calling our
**proxy**, the only thing with credentials. The **controller** (or the
CLI) creates the session, waits for the lead's turn to end, parses the
report, and posts it. The only memory is `.paved-agent/discover.yaml`
in the repository: identifiers, a stack binding with verify checks, and
the model's own measurement queries, which the controller runs *before*
the session so the lead starts with numbers.

```
 GitHub PR ─▶ controller/CLI ─▶ session (lead + specialists at Anthropic)
                  │                      │ tool_use events
                  │ pre-measure          ▼
                  ▼               worker (sandbox) ──HTTP──▶ proxy ──▶ metrics / GitHub
             proxy (metrics)              │ read/grep/bash on /work/<session>/<repo>
                  ▲                       ▼
                  └──── report JSON ◀── lead's last message
```

## 1. The program — `internal/agents/qualities.yaml`

Read it top to bottom once. Then `qualities.go`: `LoadProgram` checks the
shape; `Render` turns it into the lead's system prompt; `FindProductNames`
is the denylist test that keeps products out of every prompt.

Questions: Why is the program indexed by *property* rather than by kind of
change? What does "proportionality" do, and which property sets it? Why
is "no impact, and here is why" a finding?

## 2. The agents — `internal/agents/agents.go`

`Lead(program, model, effort)` = the program + "mechanics" (where things
are, how to delegate, questions, proposals, the file schema, the report
JSON). `Specialists` = the topology discoverer (the stack-discovery
procedure, §7.4 of the design) and the org finder. `Params` builds the
API request: the built-in toolset with only read/glob/grep/bash enabled,
plus custom tools from the connector registry, plus the roster.

Questions: What does the lead know that its system prompt cannot (hint:
the brief, `internal/session/session.go:Brief`)? Why do specialists see
nothing of the lead's conversation, and what follows for how a task must
be written?

## 3. The only memory — `internal/identifiers/identifiers.go`

The schema, the lint (no endpoints, hostnames, secrets), `measure:`, and
`answers:`. Compare with `DESIGN.md` §7.1: join keys, the stack binding,
answers — and nothing live.

Questions: Why is `calls:` *not* in the file? Why do `verify:` lines
exist if `measure:` lines are executed anyway? What happens when a
selector matches nothing?

## 4. The proxy — `internal/proxy/`

`config.go` (slots bound to backends; credentials as *sources*),
`policy.go` (auth, per-session call budget, range bound, result clipping,
audit), `metrics.go` (five PromQL reads), `scm.go` (reads, org-scoped;
tarballs fetched with the proxy's token), `scm_write.go` (the three
writes, path-restricted to `.paved-agent/`), `server.go` (`guard` is the
one path every call takes).

Run the tests and read `server_test.go`: they are the security claims.

Questions: Why is the proxy the *only* component worth hardening? What
can a fully subverted agent do, at most? Why did `:8080` bind only IPv6
on the laptop, and why did the model find the proxy anyway (see the
first run's trace)?

## 5. The connectors — `internal/connectors/connectors.go`

Typed tools (`toolrunner.NewBetaToolFromJSONSchema`) whose bodies are
HTTP calls to the proxy. `Definitions` turns the same registry into agent
configuration so the two cannot drift. `compactVector` is why a metric
result costs a fifth of the raw JSON. `Mount` unpacks a tarball and drops
symlinks and escaping paths. A failing backend returns `unavailable: …`
as *text*, so the model treats absence as evidence.

Questions: Who executes a connector tool, and with whose identity? Why
does the model never need a write tool?

## 6. The worker — `internal/sandbox/sandbox.go` and the SDK

This is the distributed-systems part. Read `sandbox.go` first (it is
short), then these in the SDK, in order:

1. `lib/environments/poller.go` — `WorkPoller`: long-poll `work.poll`
   with the environment key; **claim** an item (a lease with a TTL,
   300 s on the server); `AutoStop`; `ReclaimOlderThanMs`; backoff.
2. `lib/environments/worker.go` — `HandleItem`: the per-item
   credential (the work item's secret carries a sessions token);
   **heartbeat** the lease with `expected_last_heartbeat` (a fence: a
   heartbeat that does not match the server's last one is refused, and
   the SDK treats that as a lost lease and *releases without stopping*);
   run the tool runner while heartbeating; force-stop at the end unless
   the lease was lost.
3. `betasessiontoolrunner.go` — `SessionToolRunner`: subscribe to the
   event **stream** (SSE); **reconnect** with backoff; **reconcile** on
   reconnect by listing events, so a tool call that arrived while the
   stream was down is still answered; `seen`/`answered` sets so each tool
   call is executed and posted **exactly once**; the **idle countdown**
   (`MaxIdle`) armed on `session.status_idle` with any stop reason other
   than `requires_action`.
4. `tools/agenttoolset/` — bash/read/glob/grep confined to the workdir;
   `Env` for bash (we pass a minimal map: no credentials).

What happened in the third run, 2026-10-05 18:25 PT, is the worked
example: a heartbeat timed out on our side but reached the server; the
next heartbeat's precondition failed; the SDK released the item; the
session sat orphaned with a tool call nobody answered. The fix is
`ReclaimOlderThanMs` plus a looping worker. Questions: why can the
client not tell whether a timed-out heartbeat succeeded? Why "release
without stopping"? What does the server do with an item whose lease
expires?

## 7. One session, event by event — `internal/session/`

`Brief` (the initial message); `Create` (agent version, environment,
budget in cents, `initial_events`); `Wait` — note the subtlety: a session
is *idle* between every tool call too (stop reason `requires_action`), so
finished means idle with any other reason, read from the last
`session.status_idle` event; `LastLeadMessage` (the last `agent.message`
on the main thread); `Trace` (`change-agent trace --session ID`).
`tiered.go`: triage on the smaller model, escalation to the deep tier.

Open `evals/runs/2026-10-05-hello1/trace.txt` and follow one review:
the brief, the lead's first turn, `scm_mount` and `scm_pr`, the
delegation (`agent.thread_message_sent`), the specialist threads, the
report. Then `evals/runs/2026-10-05-hello2-dev/trace.txt` — the same
machinery with identifiers present: eight tool calls, no delegation.

Questions: What is the unit of cost (hint: a *turn* re-reads the whole
context; cached reads are ~10%)? Why did `measure:` cut 50 turns to
zero? What does a budget stop look like (run 3)?

## 8. The report — `internal/findings/findings.go`

The contract, `Parse` (the last fenced JSON block), `Normalize` (drops
evidence-free findings; derives verdicts), `Render`, `Markdown`.

Question: why does the *controller* apply proposals and post questions,
rather than the model writing to git?

## 9. The controller — `internal/controller/controller.go`

Webhook (HMAC) and poll; one run per PR; the review; the gate as a commit
status (check runs are App-only); the answer run (prose → file, merged,
linted, committed); the state file per PR.

Question: with `watch.repos` empty, what bounds which repositories are
reviewed?

## 10. The numbers — `evals/runs/README.md` and `DESIGN.md` §12

Bootstrap ~$3 once per repo; identifiers runs $0.08 (Sonnet) / ~$1
(Opus); the tiers; the daily release-fitness run. What is established,
what is still a guess (precision; whether Sonnet can finalise), and what
disappointed (bootstrap exploration; no batching).

## Things to try tonight, each under a dollar

- `change-agent review --pr smallStepGiantLeap/hello#2 --dry-run` — read
  the brief, including the pre-measured facts.
- `change-agent trace --session sesn_01UJBkDthabRW8i64xgqeB8q --width 400`
  — the 42-second run, every event.
- `change-agent review --pr smallStepGiantLeap/hello#2 --tier dev --budget 1`
  — run it yourself, with the proxy running in another terminal
  (`change-agent proxy`); watch the worker log and the proxy's audit.
- Break the `enforcer` line's `verify` in a copy of `discover.yaml` on a
  branch and see the lead report it broken.
