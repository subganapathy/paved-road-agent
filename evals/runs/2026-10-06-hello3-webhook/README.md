# The webhook path, run live (2026-10-06)

`change-agent serve --poll 60s --no-worker` against the demo org, with
the sandbox pod serving the tools and `watch.repos: [hello]`. Three
reviews of hello#3 (fixture P4), one human reply on the thread.

| step | what happened | cost |
|---|---|---|
| 1 | The poll found hello#3 unreviewed. Status `impact: pending — reviewing`; staged review; comment posted; verdict **warning**; one question (does the traffic generator ever send a blank name?); status `pending — action required: 1 question(s) — reply on the thread`. | $0.90 |
| — | The human replied **no**. The answer model returned the merged file as one JSON string and `MaxTokens: 2048` cut it off; the parser failed; the cursor had already advanced, so the reply was lost in silence. Fixed: two fenced blocks (json, then yaml), 8k tokens, and a comment on failure. Cursor reset; the reply was re-read within 15 s; `.paved-agent/discover.yaml` committed to the PR branch as `adf776c` with the new answer under the two existing ones; follow-up comment posted. | — |
| 2 | The push re-triggered the review. Verdict **blocking**: the lead saw the `answers:` line arrive *inside the PR under review* and concluded the PR was certifying itself — and asked the same question again. From what it could see, that was the right call: nothing told it the commit was the reviewer's, recording a person with write access. | $0.98 |
| — | Fixed: the controller remembers every answer it committed (question, who, which commit), the brief lists them, and the lead's rules say a recorded answer is a fact with a named source — cite it, never re-ask, report a contradiction as a finding. Agents rolled (lead v8, lead-dev v6, instantiator v4). | — |
| 3 | Same head, reviewed again. Verdict **warning**, no question; recorded answers cited; status **`impact: success — passed with warnings`**. The PR is mergeable. | $1.02 |

What the unit tests could not have found: all three. The webhook, the
status gate, the comment and the commit all worked first time; the model
protocols around them did not. Reports for each step are beside this
file.
